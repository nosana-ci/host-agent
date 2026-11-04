package updater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/bundle"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/manifest"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

const (
	checkEvery = 10 * time.Minute
	// firstCheck gives the supervisor time to see the node first; until then it is not interrupted.
	firstCheck = 30 * time.Second
	// keepAfter is how long a new version must stay up before it is kept; Preflight rolls back
	// a version that fails to start three times before that.
	keepAfter      = 2 * time.Minute
	runtimeStartup = 45 * time.Second
)

type Updater struct {
	cfg config.Config
	st  *store.Store
	sup *node.Supervisor
	pc  *podman.Client
	// proving is set when this process is a pending version that must stay up keepAfter.
	proving bool

	apply  sync.Mutex // one Check applies at a time: Run and the commands may call it together
	mu     sync.Mutex
	status api.UpdateStatus
	ready  chan struct{} // installAgent linked a new agent in
}

func New(cfg config.Config, st *store.Store, sup *node.Supervisor, pc *podman.Client) *Updater {
	u := &Updater{cfg: cfg, st: st, sup: sup, pc: pc, ready: make(chan struct{}, 1), status: api.UpdateStatus{State: "idle"}}
	if p := st.State().Update.Pending; p != nil && p.Version == version.Version {
		u.proving = true
		u.set("pending", "version "+p.Version+" is kept after "+keepAfter.String()+" up")
	}
	return u
}

func (u *Updater) Status() api.UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

func (u *Updater) set(state, detail string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = api.UpdateStatus{State: state, LatestVersion: u.status.LatestVersion}
	if detail != "" {
		u.status.Detail = &detail
	}
}

// Run checks the release every checkEvery and applies what it finds. A pending version is kept
// once this process has been up for keepAfter.
func (u *Updater) Run(ctx context.Context) {
	var keep <-chan time.Time
	if u.proving {
		keep = time.After(keepAfter)
	}
	next := time.After(firstCheck)
	for {
		select {
		case <-ctx.Done():
			if keep != nil { // a clean stop is not a failed start
				_ = u.st.Update(func(s *store.State) {
					if p := s.Update.Pending; p != nil && p.Attempts > 0 {
						p.Attempts--
					}
				})
			}
			return
		case <-keep:
			keep = nil
			u.commit()
		case <-next:
			if _, err := u.Check(ctx, false); err != nil && ctx.Err() == nil {
				slog.Warn("update check", "err", err)
			}
			next = time.After(checkEvery)
		}
	}
}

// Check reads the manifest and applies it: the agent right away (the node container keeps running
// while the agent replaces itself), the node image and the runtime bundle once the node may be
// interrupted (see Supervisor.MayInterrupt) or force is set. It returns what it did, in a sentence.
func (u *Updater) Check(ctx context.Context, force bool) (string, error) {
	if !u.apply.TryLock() {
		return "an update is already running", nil
	}
	defer u.apply.Unlock()
	if p := u.st.State().Update.Pending; p != nil {
		return "version " + p.Version + " is still proving itself", nil
	}
	u.set("checking", "")
	m, err := manifest.Fetch(ctx, u.cfg.ReleaseURL)
	if err != nil {
		return u.fail(err)
	}
	u.mu.Lock()
	u.status.LatestVersion = &m.Version
	u.mu.Unlock()
	u.sup.SetImage(m.Node.Image)
	if force {
		u.sup.SwapNow()
	}

	// The runtime only moves up: a halted release must not downgrade podman under its database.
	if cur := bundle.Current(); version.Compare(m.Runtime.Version, cur) > 0 && !slices.Contains(u.st.State().Update.BlockedRuntime, m.Runtime.Version) {
		u.set("downloading", "runtime "+m.Runtime.Version)
		ver, err := bundle.Install(ctx, m) // the download does not touch the node; the switch does
		if err != nil {
			return u.fail(err)
		}
		if !force && !u.sup.MayInterrupt() {
			return u.done("deferred", "runtime "+ver+" waits until the node is between jobs")
		}
		if err := u.switchRuntime(ctx, ver, cur); err != nil {
			return u.fail(err)
		}
		u.sup.ChangeApplied()
	} else if cur != "" && bundle.Stale(cur) {
		// The bundle's configuration depends on the host (the firewall driver); a new agent may
		// choose differently. Podman reads it at start, so this is a restart like a switch.
		if !force && !u.sup.MayInterrupt() {
			return u.done("deferred", "runtime restart waits until the node is between jobs")
		}
		if err := u.reconfigureRuntime(ctx, cur); err != nil {
			return u.fail(err)
		}
		u.sup.ChangeApplied()
	}
	upd := u.st.State().Update
	switch {
	case version.Compare(m.Version, version.Version) <= 0:
		return u.done("idle", "agent "+version.Version+" is up to date")
	case slices.Contains(upd.Blocked, m.Version):
		return u.done("idle", "version "+m.Version+" failed here before and is skipped")
	}
	u.set("downloading", "agent "+m.Version)
	if err := u.installAgent(ctx, m); err != nil {
		return u.fail(err)
	}
	if !force { // an update the owner asked for is answered as the command's result
		u.st.Info("update", "switching to agent %s (was %s) on its own; it is kept once it has run for %s", m.Version, version.Version, keepAfter)
	}
	return "switching to agent " + m.Version, nil
}

func (u *Updater) done(state, detail string) (string, error) {
	u.set(state, detail)
	return detail, nil
}

func (u *Updater) fail(err error) (string, error) {
	u.set("error", err.Error())
	return "", err
}

// Ready fires once installAgent linked a new agent in. The caller switches to it with Exec once
// every command it took is answered.
func (u *Updater) Ready() <-chan struct{} { return u.ready }

func (u *Updater) installAgent(ctx context.Context, m *manifest.Manifest) error {
	art := m.Agent.Artifacts.LinuxAmd64
	if art.Url == "" {
		return fmt.Errorf("release %s has no %s build", m.Version, bundle.Platform)
	}
	previous, err := filepath.EvalSymlinks(config.CurrentBinary)
	if err != nil {
		return errors.New("agent is not installed (no bin/current)")
	}
	dst := config.BinaryPath(m.Version)
	if err := art.Download(ctx, dst, 0o755); err != nil {
		return err
	}
	// A binary that cannot run here could not roll itself back: try it before switching.
	out, err := exec.CommandContext(ctx, dst, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), m.Version) {
		if ctx.Err() != nil { // a stop mid-test says nothing about the binary
			return ctx.Err()
		}
		_ = u.st.Update(func(s *store.State) { s.Update.Blocked = append(s.Update.Blocked, m.Version) })
		err = fmt.Errorf("agent %s does not run here (%v: %s); not switching to it", m.Version, err, strings.TrimSpace(string(out)))
		u.st.Error("update", "%v; it is skipped until a newer release", err)
		return err
	}
	if err := u.st.Update(func(s *store.State) {
		s.Update.Pending = &store.Pending{Version: m.Version, Previous: previous}
	}); err != nil {
		return err
	}
	if err := store.Relink(config.CurrentBinary, dst); err != nil {
		_ = u.st.Update(func(s *store.State) { s.Update.Pending = nil })
		return err
	}
	slog.Info("switching to agent once the commands are answered", "to", m.Version)
	u.ready <- struct{}{}
	return nil

}

// commit keeps the running version: it stayed up long enough.
func (u *Updater) commit() {
	p := u.st.State().Update.Pending
	_ = u.st.Update(func(s *store.State) { s.Update.Pending = nil })
	pruneBinaries(p.Previous)
	slog.Info("agent update committed", "version", version.Version, "was", binVersion(p.Previous))
	u.st.Info("update", "agent updated to %s (was %s)", version.Version, binVersion(p.Previous))
	u.set("idle", "")
}

// reconfigureRuntime rewrites the configuration of the running bundle ver and restarts podman on
// it. The node is restarted too: its network rules came from the old firewall driver.
func (u *Updater) reconfigureRuntime(ctx context.Context, ver string) error {
	if err := bundle.Reconfigure(ver); err != nil {
		return err
	}
	if err := u.activate(ctx, ver); err != nil {
		u.st.Error("runtime", "runtime %s does not start with its new configuration: %v", ver, err)
		return err
	}
	bundle.DropStaleFirewallRules()
	slog.Info("runtime reconfigured", "version", ver, "firewall", bundle.FirewallDriver())
	if u.st.State().NodeStopped { // a stopped node has no rules; it stays stopped
		u.st.Info("runtime", "runtime %s reconfigured (firewall driver %s)", ver, bundle.FirewallDriver())
		return nil
	}
	u.st.Info("runtime", "runtime %s reconfigured (firewall driver %s); the node is restarted", ver, bundle.FirewallDriver())
	return u.sup.Restart()
}

// switchRuntime restarts the podman service on the installed bundle ver and goes back to the old
// bundle when podman does not come up.
func (u *Updater) switchRuntime(ctx context.Context, ver, old string) error {
	if err := u.activate(ctx, ver); err == nil {
		slog.Info("runtime switched", "from", old, "to", ver)
		if old == "" {
			u.st.Info("runtime", "runtime %s installed", ver)
		} else {
			u.st.Info("runtime", "runtime switched to %s (was %s)", ver, old)
		}
		bundle.Prune(old)
		return nil
	}
	if ctx.Err() != nil { // a stop mid-switch says nothing about the runtime
		if old != "" {
			_ = u.activate(ctx, old)
		}
		return ctx.Err()
	}
	slog.Warn("runtime did not come up; going back", "failed", ver, "to", old)
	_ = u.st.Update(func(s *store.State) { s.Update.BlockedRuntime = append(s.Update.BlockedRuntime, ver) })
	err := fmt.Errorf("runtime %s does not start; back on %s", ver, old)
	if old == "" {
		err = fmt.Errorf("runtime %s does not start and there is nothing to go back to", ver)
	} else if aerr := u.activate(ctx, old); aerr != nil {
		err = fmt.Errorf("runtime %s does not start, and going back to %s failed: %w", ver, old, aerr)
	}
	u.st.Error("runtime", "%v", err)
	return err
}

func (u *Updater) activate(ctx context.Context, ver string) error {
	if err := bundle.Activate(ver); err != nil {
		return err
	}
	if err := service.Systemctl("restart", service.PodmanUnit); err != nil {
		return err
	}
	deadline := time.Now().Add(runtimeStartup)
	for time.Now().Before(deadline) {
		if _, err := u.pc.Info(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("podman did not answer within " + runtimeStartup.String())
}
