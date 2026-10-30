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

	"github.com/nosana-ci/nosana-host/internal/api"
	"github.com/nosana-ci/nosana-host/internal/config"
	"github.com/nosana-ci/nosana-host/internal/node"
	"github.com/nosana-ci/nosana-host/internal/podman"
	"github.com/nosana-ci/nosana-host/internal/runtime"
	"github.com/nosana-ci/nosana-host/internal/service"
	"github.com/nosana-ci/nosana-host/internal/store"
	"github.com/nosana-ci/nosana-host/internal/updater/download"
	"github.com/nosana-ci/nosana-host/internal/version"
)

const (
	checkEvery = 10 * time.Minute
	// healthDeadline is how long a new version has to get a heartbeat accepted before it is
	// rolled back.
	healthDeadline = 5 * time.Minute
	runtimeStartup = 45 * time.Second
)

type Updater struct {
	cfg  config.Config
	st   *store.Store
	sup  *node.Supervisor
	pc   *podman.Client
	wake chan struct{}

	mu     sync.Mutex
	status api.UpdateStatus
}

func New(cfg config.Config, st *store.Store, sup *node.Supervisor, pc *podman.Client) *Updater {
	u := &Updater{cfg: cfg, st: st, sup: sup, pc: pc, wake: make(chan struct{}, 1), status: api.UpdateStatus{State: "idle"}}
	if p := st.State().Update.Pending; p != nil && p.Version == version.Version {
		u.set("pending", "version "+p.Version+" must send a heartbeat before it is kept")
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

// Run checks the release every checkEvery and applies what it finds.
func (u *Updater) Run(ctx context.Context) {
	for {
		if _, err := u.Check(ctx, false); err != nil && ctx.Err() == nil {
			slog.Warn("update check", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(checkEvery):
		case <-u.wake:
		}
	}
}

// Check reads the manifest and applies it: the node image right away, the runtime bundle and the
// agent when the node is idle or force is set. It returns what it did, in a sentence.
func (u *Updater) Check(ctx context.Context, force bool) (string, error) {
	if p := u.st.State().Update.Pending; p != nil {
		return "version " + p.Version + " is still proving itself", nil
	}
	u.set("checking", "")
	m, err := FetchManifest(ctx, u.cfg.ControlURL)
	if err != nil {
		u.set("error", err.Error())
		return "", err
	}
	u.mu.Lock()
	u.status.LatestVersion = &m.Version
	u.mu.Unlock()
	u.sup.SetImage(m.Node.Image)

	busy := u.sup.Busy() && !force
	if cur := runtime.Current(); m.Runtime.Version != cur && !slices.Contains(u.st.State().Update.BlockedRuntime, m.Runtime.Version) {
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
	case upd.IsBlocked(m.Version):
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
	return "switching to agent " + m.Version, nil // not reached: installAgent execs
}

// Poke makes Run check now.
func (u *Updater) Poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Updater) installAgent(ctx context.Context, m *api.ReleaseManifest) error {
	art := m.Agent.Artifacts.LinuxAmd64
	if art.Url == "" {
		return fmt.Errorf("release %s has no %s build", m.Version, runtime.Platform)
	}
	previous, err := filepath.EvalSymlinks(config.CurrentBinary)
	if err != nil {
		return errors.New("agent is not installed (no bin/current)")
	}
	dst := config.BinaryPath(m.Version)
	if err := download.File(ctx, art.Url, dst, art.Sha256, 0o755); err != nil {
		return err
	}
	// A binary that cannot run here could not roll itself back: try it before switching.
	out, err := exec.CommandContext(ctx, dst, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), m.Version) {
		_ = u.st.Update(func(s *store.State) { s.Update.Blocked = append(s.Update.Blocked, m.Version) })
		return fmt.Errorf("agent %s does not run here (%v: %s); not switching to it", m.Version, err, strings.TrimSpace(string(out)))
	}
	if err := u.st.Update(func(s *store.State) {
		s.Update.Pending = &store.Pending{Version: m.Version, Previous: previous, StartedAt: time.Now().UTC()}
	}); err != nil {
		return err
	}
	if err := store.Relink(config.CurrentBinary, dst); err != nil {
		_ = u.st.Update(func(s *store.State) { s.Update.Pending = nil })
		return err
	}
	slog.Info("switching to agent", "version", m.Version)
	execSelf()
	return nil
}

// Committed is called when host-manager accepted a heartbeat: a pending version has come up.
func (u *Updater) Committed() {
	p := u.st.State().Update.Pending
	if p == nil || p.Version != version.Version {
		return
	}
	_ = u.st.Update(func(s *store.State) { s.Update.Pending = nil })
	pruneBinaries(p.Previous)
	slog.Info("agent update committed", "version", version.Version, "was", binVersion(p.Previous))
	u.set("idle", "")
}

// Unhealthy is called when a heartbeat was refused while host-manager answered: a pending version
// that gets nothing accepted within the deadline is rolled back.
func (u *Updater) Unhealthy() {
	p := u.st.State().Update.Pending
	if p != nil && p.Version == version.Version && time.Since(p.StartedAt) > healthDeadline {
		rollback(u.st, fmt.Sprintf("version %s sent no accepted heartbeat within %s", p.Version, healthDeadline))
	}
}

// switchRuntime installs the manifest's runtime bundle, restarts the podman service on it and
// goes back to the old bundle when podman does not come up.
func (u *Updater) switchRuntime(ctx context.Context, m *api.ReleaseManifest, old string) error {
	u.set("downloading", "runtime "+m.Runtime.Version)
	ver, err := runtime.Install(ctx, m)
	if err != nil {
		return err
	}
	if err := u.activate(ctx, ver); err == nil {
		slog.Info("runtime switched", "from", old, "to", ver)
		runtime.Prune(old)
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
	if err := runtime.Activate(ver); err != nil {
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
		time.Sleep(2 * time.Second)
	}
	return errors.New("podman did not answer within " + runtimeStartup.String())
}
