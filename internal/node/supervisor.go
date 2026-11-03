// Package node runs the nosana-node container: creates it, keeps it running with backoff after a
// crash, restarts it when it asks (exit 129), swaps the image the release names, and reads the
// node's own state from its API.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

const (
	ContainerName = "nosana-node"
	networkName   = "NOSANA_GATEWAY"
	subnet        = "192.168.101.0/24"
	apiPort       = 15001 // the node's port 5001, published on localhost
	LabelImage    = "io.nosana.host.image"
	labelNetwork  = "io.nosana.host.network"
	logSizeMax    = 10 << 20
	tick          = 5 * time.Second
	stopTimeout   = 2 * time.Minute
	maxBackoff    = 5 * time.Minute
	stableAfter   = 2 * time.Minute // running this long forgets earlier crashes
	// maxWait is how long a change that stops the node waits for a node in the queue (or one
	// whose API does not answer); jobs and benchmarks are always waited for.
	maxWait = 24 * time.Hour
	// exitRespawn is the node's "start me again" exit code (node/src/exitCodes.ts).
	exitRespawn = 129
)

type Supervisor struct {
	pc      *podman.Client
	st      *store.Store
	network string
	wake    chan struct{}

	mu        sync.Mutex
	observed  bool
	status    api.NodeStatus
	image     string
	start     bool // start an exited node at once, without the crash backoff
	restarted bool // the restart happened but the state could not record it
	crashes   int
	nextStart time.Time
	swapNow   bool // the owner asked for the update: the next image swap does not wait

	// WaitingForSOL reports that the node wallet is below the SOL the node waits for at start.
	WaitingForSOL func() bool
}

func New(pc *podman.Client, st *store.Store, network string) *Supervisor {
	s := &Supervisor{pc: pc, st: st, network: network, wake: make(chan struct{}, 1)}
	s.image = st.State().NodeImage
	s.status = api.NodeStatus{Container: "unknown", Image: s.image, Desired: desired(st.State())}
	return s
}

func desired(state store.State) api.NodeStatusDesired {
	if state.NodeStopped {
		return api.Stopped
	}
	return api.Running
}

func (s *Supervisor) Status() api.NodeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// MayInterrupt reports whether a change that stops the node (a new image, a runtime switch) may
// happen now. A job or a benchmark always waits. A place in the queue, joining a market, and a
// running node whose API does not answer wait at most maxWait, counted from the first time a
// change had to wait (store Update.WaitingSince). A node that waits for SOL at start has nothing
// to lose, and neither has a node that is not running. Callers report an applied change with
// ChangeApplied.
func (s *Supervisor) MayInterrupt() bool {
	s.mu.Lock()
	observed, status := s.observed, s.status
	s.mu.Unlock()
	if !observed || status.Container == "unknown" {
		return false
	}
	if status.Container != "running" {
		return true
	}
	state := ""
	if status.State != nil {
		state = *status.State
	}
	switch state {
	case "RUNNING_JOB", "BENCHMARKING":
		s.waitingSince()
		return false
	case "", "QUEUED", "JOINING_MARKET":
		if state == "" && s.WaitingForSOL != nil && s.WaitingForSOL() {
			return true
		}
		return time.Since(s.waitingSince()) >= maxWait
	}
	return true
}

// waitingSince is when a change first waited for the node, recording now if none did.
func (s *Supervisor) waitingSince() time.Time {
	if w := s.st.State().Update.WaitingSince; w != nil {
		return *w
	}
	now := time.Now().UTC()
	_ = s.st.Update(func(st *store.State) { st.Update.WaitingSince = &now })
	return now
}

// ChangeApplied ends the wait MayInterrupt counts.
func (s *Supervisor) ChangeApplied() {
	s.mu.Lock()
	s.swapNow = false
	s.mu.Unlock()
	_ = s.st.Update(func(st *store.State) { st.Update.WaitingSince = nil })
}

// SwapNow lets the next image swap happen without waiting for the node (the update command).
func (s *Supervisor) SwapNow() {
	s.mu.Lock()
	s.swapNow = true
	s.mu.Unlock()
	s.poke()
}

func (s *Supervisor) Start() error   { return s.want(false, false) }
func (s *Supervisor) Stop() error    { return s.want(true, false) }
func (s *Supervisor) Restart() error { return s.want(false, true) }

// want records what the owner asked for. The restart is kept in the state so that a process
// replaced before it happened does not lose it.
func (s *Supervisor) want(stopped, restart bool) error {
	if err := s.st.Update(func(state *store.State) { state.NodeStopped, state.NodeRestart = stopped, restart }); err != nil {
		return err
	}
	s.mu.Lock()
	s.start, s.restarted, s.crashes, s.nextStart = !stopped, false, 0, time.Time{}
	s.mu.Unlock()
	s.poke()
	return nil
}

// restartDone clears the restart. When the state cannot be written the restart is remembered as
// done in memory, so an unwritable disk does not restart the node every tick.
func (s *Supervisor) restartDone() {
	if err := s.st.Update(func(state *store.State) { state.NodeRestart = false }); err != nil {
		slog.Warn("node: restart not recorded", "err", err)
		s.mu.Lock()
		s.restarted = true
		s.mu.Unlock()
	}
}

// SetImage changes the node image; the container is recreated once the node is idle.
func (s *Supervisor) SetImage(image string) {
	if image == "" {
		return
	}
	s.mu.Lock()
	changed := image != s.image
	s.image = image
	s.mu.Unlock()
	if changed {
		_ = s.st.Update(func(state *store.State) { state.NodeImage = image })
		s.poke()
	}
}

func (s *Supervisor) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Supervisor) Run(ctx context.Context) {
	for {
		if err := s.reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("node", "err", err)
			s.setError(err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		case <-s.wake:
		}
	}
}

func (s *Supervisor) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastError = &msg
}

func (s *Supervisor) reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // a first pull can be slow
	defer cancel()
	state := s.st.State()
	s.mu.Lock()
	image, restart, swapNow := s.image, state.NodeRestart && !s.restarted, s.swapNow
	s.status.Desired = desired(state)
	s.mu.Unlock()

	if _, err := s.pc.Info(ctx); err != nil {
		s.observe(nil, "")
		return fmt.Errorf("podman unreachable: %w", err)
	}
	if err := s.ensureNetwork(ctx); err != nil {
		return err
	}
	ctr, err := s.pc.InspectContainer(ctx, ContainerName)
	if errors.Is(err, podman.ErrNotFound) {
		ctr = nil
	} else if err != nil {
		return err
	}
	defer func() { s.observe(ctr, image) }()
	if swapNow && ctr != nil && ctr.Config.Labels[LabelImage] == image && ctr.Config.Labels[labelNetwork] == s.network {
		s.mu.Lock()
		s.swapNow = false // nothing to swap: a later release waits as usual
		s.mu.Unlock()
	}

	switch {
	case state.NodeStopped:
		if ctr != nil && ctr.State.Running {
			slog.Info("node: stopping")
			return s.pc.StopContainer(ctx, ContainerName, stopTimeout)
		}
		return nil
	case ctr == nil:
		if restart {
			s.restartDone() // creating is the restart
		}
		return s.create(ctx, image)
	case restart:
		slog.Info("node: restarting")
		if err := s.pc.StopContainer(ctx, ContainerName, stopTimeout); err != nil {
			return err
		}
		s.restartDone() // only once it happened; podman may have been away
		return s.pc.StartContainer(ctx, ContainerName)

	case image != "" && (ctr.Config.Labels[LabelImage] != image || ctr.Config.Labels[labelNetwork] != s.network) && (swapNow || s.MayInterrupt()):
		pulled, err := s.ensureImage(ctx, image) // the node keeps running during the pull
		if err != nil || pulled {
			return err // the swap waits for the next tick, which asks again whether the node is idle
		}
		slog.Info("node: recreating", "image", image, "network", s.network)
		if err := s.pc.RemoveContainer(ctx, ContainerName); err != nil {
			return err
		}
		s.ChangeApplied()
		return s.create(ctx, image)
	case ctr.State.Running:
		s.mu.Lock()
		if s.crashes > 0 && time.Since(ctr.State.StartedAt) > stableAfter {
			s.crashes = 0
		}
		s.start = false
		s.status.LastError = nil
		s.mu.Unlock()
		return nil
	}
	return s.restartExited(ctx, ctr.State.ExitCode)
}

// restartExited starts an exited node again: at once when it asked for it or the owner started
// it, otherwise with a backoff that doubles per crash.
func (s *Supervisor) restartExited(ctx context.Context, code int) error {
	s.mu.Lock()
	if s.start || code == exitRespawn {
		s.start = false
		s.mu.Unlock()
		slog.Info("node: starting", "exitCode", code)
		return s.pc.StartContainer(ctx, ContainerName)
	}
	if s.nextStart.IsZero() {
		s.crashes++
		delay := min(5*time.Second<<min(s.crashes-1, 6), maxBackoff)
		s.nextStart = time.Now().Add(delay)
		msg := fmt.Sprintf("node exited with code %d; restart %d in %s", code, s.crashes, delay.Round(time.Second))
		s.status.LastError = &msg
		s.mu.Unlock()
		slog.Warn("node: crashed", "code", code, "restartIn", delay)
		return nil
	}
	due := time.Now().After(s.nextStart)
	if due {
		s.nextStart = time.Time{}
		s.status.Restarts++
	}
	s.mu.Unlock()
	if !due {
		return nil
	}
	return s.pc.StartContainer(ctx, ContainerName)
}

func (s *Supervisor) create(ctx context.Context, image string) error {
	if image == "" {
		return errors.New("no node image known yet; waiting for the release manifest")
	}
	if _, err := s.ensureImage(ctx, image); err != nil {
		return err
	}
	spec := podman.Spec{
		Name:    ContainerName,
		Image:   image,
		Command: []string{"start", "--network", s.network},
		Env:     map[string]string{"NOSANA_HOST_VERSION": version.Version},
		Labels:  map[string]string{LabelImage: image, labelNetwork: s.network},
		Mounts: []podman.Mount{
			{Source: config.NodeDir, Destination: "/root/.nosana"},
			{Source: config.PodmanSocketDir, Destination: "/root/.nosana/podman"},
		},
		Network:    networkName,
		Ports:      []podman.Port{{HostIP: "127.0.0.1", HostPort: apiPort, ContainerPort: 5001, Protocol: "tcp"}},
		LogSizeMax: logSizeMax,
	}
	if err := s.pc.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("create node container: %w", err)
	}
	slog.Info("node: created and starting", "image", image)
	return s.pc.StartContainer(ctx, ContainerName)
}

// ensureImage pulls image when it is not here yet and reports whether it did.
func (s *Supervisor) ensureImage(ctx context.Context, image string) (bool, error) {
	exists, err := s.pc.ImageExists(ctx, image)
	if err != nil || exists {
		return false, err
	}
	slog.Info("node: pulling image", "image", image)
	return true, s.pc.Pull(ctx, image)
}

// ensureNetwork creates the node network. The node only looks it up by name, so when the usual
// subnet is taken on this host (another container engine, a VPN) the next /24 is used.
func (s *Supervisor) ensureNetwork(ctx context.Context) error {
	ok, err := s.pc.NetworkExists(ctx, networkName)
	if err != nil || ok {
		return err
	}
	_, ipnet, _ := net.ParseCIDR(subnet)
	for range 30 {
		gw := slices.Clone(ipnet.IP.To4())
		gw[3] = 1
		err = s.pc.CreateNetwork(ctx, networkName, ipnet.String(), gw.String())
		if !errors.Is(err, podman.ErrSubnetInUse) {
			if err == nil {
				slog.Info("node: created network", "network", networkName, "subnet", ipnet.String())
			}
			return err
		}
		ipnet.IP = slices.Clone(ipnet.IP.To4())
		ipnet.IP[2]++
	}
	return fmt.Errorf("no free subnet for %s: %w", networkName, err)
}

// observe refreshes the status from the container and the node's own API.
func (s *Supervisor) observe(ctr *podman.Container, image string) {
	status := api.NodeStatus{Container: "missing", Image: image}
	if ctr == nil && image == "" {
		status.Container = "unknown"
	}
	if ctr != nil {
		status.Container = ctr.State.Status
		if img := ctr.Config.Labels[LabelImage]; img != "" {
			status.Image = img
		}
		if ctr.State.Running {
			if state := nodeState(); state != "" {
				status.State = &state
			}
		}
	}
	s.mu.Lock()
	status.Desired, status.Restarts, status.LastError = s.status.Desired, s.status.Restarts, s.status.LastError
	s.status, s.observed = status, true
	s.mu.Unlock()

}

var infoClient = &http.Client{Timeout: 3 * time.Second}

// nodeState is the node's own state (QUEUED, RUNNING_JOB, ...) from /node/info, or "" when the
// API does not answer.
func nodeState() string {
	resp, err := infoClient.Get(fmt.Sprintf("http://127.0.0.1:%d/node/info", apiPort))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		State string `json:"state"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&info) != nil {
		return ""
	}
	return info.State
}
