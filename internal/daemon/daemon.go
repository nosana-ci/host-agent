// Package daemon wires the agent together: the node supervisor, the updater, the host checks and
// the link to host-manager (claim, heartbeats, commands).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nosana-ci/nosana-host/internal/api"
	"github.com/nosana-ci/nosana-host/internal/config"
	"github.com/nosana-ci/nosana-host/internal/control"
	"github.com/nosana-ci/nosana-host/internal/doctor"
	"github.com/nosana-ci/nosana-host/internal/gpu"
	"github.com/nosana-ci/nosana-host/internal/keys"
	"github.com/nosana-ci/nosana-host/internal/node"
	"github.com/nosana-ci/nosana-host/internal/podman"
	"github.com/nosana-ci/nosana-host/internal/runtime"
	"github.com/nosana-ci/nosana-host/internal/service"
	"github.com/nosana-ci/nosana-host/internal/store"
	"github.com/nosana-ci/nosana-host/internal/updater"
	"github.com/nosana-ci/nosana-host/internal/version"
)

const (
	heartbeatEvery = 15 * time.Second
	doctorEvery    = 10 * time.Minute
)

type daemon struct {
	cfg  config.Config
	st   *store.Store
	pc   *podman.Client
	sup  *node.Supervisor
	upd  *updater.Updater
	ctrl *control.Client

	mu     sync.Mutex
	gpu    api.GpuStatus
	checks []api.Check
}

func Run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("not installed: %w", err)
	}
	st, err := store.Open(config.StatePath)
	if err != nil {
		return err
	}
	updater.Preflight(st)
	if changed, err := service.WriteUnits(); err == nil && changed {
		_ = service.Systemctl("daemon-reload") // the new units take effect at the next restart
	}
	wallet, err := keys.Wallet(config.WalletPath)
	if err != nil {
		return err
	}
	pc := podman.New(config.PodmanSocket)
	sup := node.New(pc, st, cfg.Network)
	d := &daemon{
		cfg: cfg, st: st, pc: pc, sup: sup,
		upd:  updater.New(cfg, st, sup, pc),
		ctrl: control.New(cfg.ControlURL, wallet),
		gpu:  api.GpuStatus{Gpus: []api.Gpu{}}, // host-manager wants an array before the first detection too
	}
	slog.Info("nosana-host", "version", version.Version, "node", keys.Address(wallet), "control", cfg.ControlURL)

	var wg sync.WaitGroup
	for _, run := range []func(context.Context){d.sup.Run, d.upd.Run, d.doctorLoop, d.heartbeatLoop, d.commandLoop} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(ctx)
		}()
	}
	wg.Wait()
	return nil
}

func (d *daemon) doctorLoop(ctx context.Context) {
	for {
		d.runDoctor(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(doctorEvery):
		}
	}
}

func (d *daemon) runDoctor(ctx context.Context) []api.Check {
	g := gpu.Detect(ctx)
	checks := doctor.Run(ctx, d.cfg.ControlURL, d.pc, g)
	d.mu.Lock()
	d.gpu, d.checks = g, checks
	d.mu.Unlock()
	return checks
}

// heartbeatLoop claims the host when it is not linked, and otherwise reports the status every
// heartbeatEvery. Failures back off up to five minutes.
func (d *daemon) heartbeatLoop(ctx context.Context) {
	fails := 0
	for {
		var err error
		if d.st.State().Claimed {
			err = d.heartbeat(ctx)
		} else {
			err = d.claim(ctx)
		}
		wait := heartbeatEvery
		if err != nil {
			fails++
			wait = min(heartbeatEvery<<min(fails, 5), 5*time.Minute)
			if ctx.Err() == nil {
				slog.Warn("control", "err", err, "retryIn", wait)
			}
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// claim links the host with the token the installer saved. A token host-manager refuses is
// dropped: the owner makes a new one and runs the install command again.
func (d *daemon) claim(ctx context.Context) error {
	raw, err := os.ReadFile(config.ClaimTokenPath)
	if err != nil {
		return errors.New("not linked to an account: run the install command from the console again")
	}
	hostname, _ := os.Hostname()
	err = d.ctrl.Claim(ctx, strings.TrimSpace(string(raw)), hostname)
	var he *control.HTTPError
	if errors.As(err, &he) && he.Status != 429 && he.Status < 500 {
		_ = os.Remove(config.ClaimTokenPath)
		return fmt.Errorf("claim refused (%s); make a new install command in the console", he.Message)
	}
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	_ = os.Remove(config.ClaimTokenPath)
	_ = d.st.Update(func(s *store.State) { s.Claimed = true })
	slog.Info("linked to the account")
	return nil
}

func (d *daemon) heartbeat(ctx context.Context) error {
	cmds, err := d.ctrl.Heartbeat(ctx, d.status(ctx))
	switch {
	case errors.Is(err, control.ErrUnknownHost):
		_ = d.st.Update(func(s *store.State) { s.Claimed = false })
		return errors.New("host-manager does not know this host anymore; waiting for a new claim")
	case err != nil:
		var he *control.HTTPError
		if errors.As(err, &he) {
			d.upd.Unhealthy()
		}
		return fmt.Errorf("heartbeat: %w", err)
	}
	now := time.Now().UTC()
	_ = d.st.Update(func(s *store.State) { s.LastHeartbeatAt = &now })
	d.upd.Committed()
	d.runCommands(ctx, cmds)
	return nil
}

// commandLoop long-polls for commands while the host is linked.
func (d *daemon) commandLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if !d.st.State().Claimed {
			time.Sleep(heartbeatEvery)
			continue
		}
		cmds, err := d.ctrl.WaitCommands(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, control.ErrUnknownHost) {
				slog.Warn("command poll", "err", err)
			}
			time.Sleep(heartbeatEvery)
			continue
		}
		d.runCommands(ctx, cmds)
	}
}

func (d *daemon) status(ctx context.Context) api.AgentStatus {
	d.mu.Lock()
	g, checks := d.gpu, d.checks
	d.mu.Unlock()
	if checks == nil {
		checks = []api.Check{}
	}
	return api.AgentStatus{
		Version: 1,
		Agent:   api.AgentInfo{Version: version.Version, Commands: verbs(), Update: d.upd.Status()},
		Node:    d.sup.Status(),
		Gpu:     g,
		Disk:    doctor.Disk(),
		Checks:  checks,
		Runtime: d.runtimeStatus(ctx),
	}
}

func (d *daemon) runtimeStatus(ctx context.Context) api.RuntimeStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st := api.RuntimeStatus{}
	if b := runtime.Current(); b != "" {
		st.Bundle = &b
	}
	if info, err := d.pc.Info(ctx); err == nil {
		st.Reachable = true
		st.PodmanVersion = &info.Version.Version
	}
	return st
}
