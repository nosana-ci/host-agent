package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/legacy"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/store"
)

// Import makes the key in keyPath the node wallet of the installed agent. Another wallet the agent
// had is backed up first. A node that runs in docker with the key (start.sh's, or a node container)
// is asked to stop before its next job and taken over by the agent: it stops that node once it did
// or is seen between jobs, and starts its own once the old one is gone (daemon's legacyLoop).
func Import(ctx context.Context, keyPath string, out io.Writer) (err error) {
	say := sayTo(out)
	cfg, err := config.Load()
	if err != nil {
		return errors.New("the agent is not installed; run the install command first")
	}
	key, err := keys.Read(keyPath)
	if err != nil {
		return err
	}
	address := keys.Address(key)
	old := legacy.Running(ctx, address)
	if len(old) > 0 {
		for _, c := range doctor.GPU(ctx) {
			if c.Status == api.CheckStatusFail {
				return fmt.Errorf("%s: %s; fix this first, the old node keeps running: %s", c.Title, deref(c.Detail), deref(c.Fix))
			}
		}
	}
	have, err := keys.Read(config.WalletPath)
	swap := err == nil && !have.Equal(key)
	if swap && !ownerStopped() { // asked at its domain: the agent may be stopped, its node not
		was := keys.Address(have)
		if info, err := node.Public(ctx, node.Domain(cfg.Network, was), was); err == nil && node.Busy(info.State) {
			return fmt.Errorf("this agent's node %s runs a job; run this again between jobs, or stop it first: nosana-host node stop", was)
		}
	}

	_ = service.Systemctl("stop", service.AgentUnit) // it would overwrite the state we change
	defer func() {
		if err != nil {
			_ = service.Systemctl("start", service.AgentUnit)
		}
	}()
	if swap {
		removeNode(ctx, say) // it runs with the other wallet
		backup, err := backupWallet()
		if err != nil {
			return fmt.Errorf("the node wallet could not be backed up, so it was kept: %w", err)
		}
		say("wallet of node %s backed up to %s", keys.Address(have), backup)
		if err := os.Remove(config.WalletPath); err != nil {
			return err
		}
	}
	if _, err := keys.Import(keyPath, config.WalletPath); err != nil {
		return err
	}
	st, err := store.Open(config.StatePath)
	if err != nil {
		return err
	}
	moving := len(old) > 0
	if moving {
		if err := legacy.Drain(ctx, address); err != nil {
			return fmt.Errorf("ask the old node to stop after its job: %w", err)
		}
	}
	if err := st.Update(func(s *store.State) {
		s.NodeStopped, s.Handover, s.Moved = moving, moving, s.Moved || moving
	}); err != nil {
		return err
	}
	for _, args := range [][]string{{"enable", service.AgentUnit}, {"restart", service.AgentUnit}} {
		if err := service.Systemctl(args...); err != nil {
			return err
		}
	}
	if !moving {
		say("node %s starts under the agent", address)
		return nil
	}
	say("node %s runs in docker (%s); it finishes a job that runs, then the agent stops it and starts the node itself", address, strings.Join(old, ", "))
	say("to go back: sudo nosana-host uninstall, then start the old node again; %s is untouched", keyPath)
	say("watching the move; Ctrl+C stops watching, the agent finishes the move on its own")
	return watch(ctx, say)
}

// ownerStopped reports whether the owner stopped the agent's node (node stop): its job is given up,
// though its domain may still answer for it while it shuts down.
func ownerStopped() bool {
	st, err := store.Open(config.StatePath)
	return err == nil && st.State().NodeStopped
}

// watch prints what the agent's takeover waits for, until the node runs under the agent.
func watch(ctx context.Context, say func(string, ...any)) error {
	last := ""
	for {
		s, err := agentStatus(ctx)
		switch {
		case err != nil: // uninstall removes the unit, also without --purge
			if _, err := os.Stat(filepath.Join(service.UnitDir, service.AgentUnit)); err != nil {
				return errors.New("the agent was uninstalled, so the move is undone; the old node keeps running")
			}
		case s.Moving == "":
			say("node %s runs under the agent", s.NodeAddress)
			return nil
		case s.Moving != last:
			last = s.Moving
			say("%s", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func agentStatus(ctx context.Context) (*local.Status, error) {
	answer, err := local.Call(ctx, http.MethodGet, "/status", nil)
	if err != nil {
		return nil, err
	}
	defer answer.Close()
	var s local.Status
	return &s, json.NewDecoder(answer).Decode(&s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
