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

	apply  sync.Mutex // one Check applies at a time: Run and the commands may call it together
	mu     sync.Mutex
	status api.UpdateStatus
	ready  string // the agent version installAgent linked in, waiting for Exec
}

func New(cfg config.Config, st *store.Store, sup *node.Supervisor, pc *podman.Client) *Updater {
	u := &Updater{cfg: cfg, st: st, sup: sup, pc: pc, status: api.UpdateStatus{State: "idle"}}
	if p := st.State().Update.Pending; p != nil && p.Version == version.Version {
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
	if p := u.st.State().Update.Pending; p != nil && p.Version == version.Version {
		keep = time.After(keepAfter)
	}
	for {
		if _, err := u.Check(ctx, false); err != nil && ctx.Err() == nil {
			slog.Warn("update check", "err", err)
		}
		u.Exec()
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
		case <-time.After(checkEvery):
		}
	}
}

// Check reads the manifest and applies it: the node image right away, the runtime bundle and the
// agent when the node is idle or force is set. It returns what it did, in a sentence.
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
		u.set("error", err.Error())
		return "", err
	}
	u.mu.Lock()
	u.status.LatestVersion = &m.Version
	u.mu.Unlock()
	u.sup.SetImage(m.Node.Image)

	busy := u.sup.Busy() && !force
	// The runtime only moves up: a halted release must not downgrade podman under its database.
	if cur := bundle.Current(); version.Compare(m.Runtime.Version, cur) > 0 && !slices.Contains(u.st.State().Update.BlockedRuntime, m.Runtime.Version) {
		if busy {
			u.set("deferred", "runtime "+m.Runtime.Version+" waits until the node is idle")
			return "runtime update to " + m.Runtime.Version + " deferred: the node is busy", nil
		}
		if err := u.switchRuntime(ctx, m, cur); err != nil {
			u.set("error", err.Error())
			return "", err
		}
	}
	upd := u.st.State().Update
	switch {
	case version.Compare(m.Version, version.Version) <= 0:
		u.set("idle", "agent is up to date")
		return "agent " + version.Version + " is up to date", nil
	case slices.Contains(upd.Blocked, m.Version):
		u.set("idle", "version "+m.Version+" failed here and is skipped")
		return "version " + m.Version + " failed here before and is skipped", nil
	case busy:
		u.set("deferred", "update to "+m.Version+" waits until the node is idle")
		return "update to " + m.Version + " deferred: the node is busy", nil
	}
	u.set("downloading", "agent "+m.Version)
	if err := u.installAgent(ctx, m); err != nil {
		u.set("error", err.Error())
		return "", err
	}
	return "switching to agent " + m.Version, nil
}

// Exec switches to the agent installAgent linked in, once the caller has reported the result.
// It does not return then; the node keeps running.
func (u *Updater) Exec() {
	u.mu.Lock()
	to := u.ready
	u.mu.Unlock()
	if to != "" {
		slog.Info("switching to agent", "to", to)
		execSelf()
	}
}

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
		_ = u.st.Update(func(s *store.State) { s.Update.Blocked = append(s.Update.Blocked, m.Version) })
		return fmt.Errorf("agent %s does not run here (%v: %s); not switching to it", m.Version, err, strings.TrimSpace(string(out)))
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
	u.mu.Lock()
	u.ready = m.Version
	u.mu.Unlock()
	return nil
}

// commit keeps the running version: it stayed up long enough.
func (u *Updater) commit() {
	p := u.st.State().Update.Pending
	_ = u.st.Update(func(s *store.State) { s.Update.Pending = nil })
	pruneBinaries(p.Previous)
	slog.Info("agent update committed", "version", version.Version, "was", binVersion(p.Previous))
	u.set("idle", "")
}

// switchRuntime installs the manifest's runtime bundle, restarts the podman service on it and
// goes back to the old bundle when podman does not come up.
func (u *Updater) switchRuntime(ctx context.Context, m *manifest.Manifest, old string) error {
	u.set("downloading", "runtime "+m.Runtime.Version)
	ver, err := bundle.Install(ctx, m)
	if err != nil {
		return err
	}
	if err := u.activate(ctx, ver); err == nil {
		slog.Info("runtime switched", "from", old, "to", ver)
		bundle.Prune(old)
		return nil
	}
	slog.Warn("runtime did not come up; going back", "failed", ver, "to", old)
	_ = u.st.Update(func(s *store.State) { s.Update.BlockedRuntime = append(s.Update.BlockedRuntime, ver) })
	if old == "" {
		return fmt.Errorf("runtime %s does not start and there is nothing to go back to", ver)
	}
	if err := u.activate(ctx, old); err != nil {
		return fmt.Errorf("runtime %s does not start, and going back to %s failed: %w", ver, old, err)
	}
	return fmt.Errorf("runtime %s does not start; back on %s", ver, old)
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
