// Package node runs the nosana-node container: creates it, keeps it running with backoff after a
// crash, restarts it when it asks (exit 129), swaps the image the release names, and reads the
// node's own state from its API.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	labelSpec     = "io.nosana.host.spec"
	// specVersion changes with the container's definition; a container made with another one is
	// recreated once the node is idle. 2: a terminal, so the node draws its spinners and bars and
	// reads keys (logs node -f, attach).
	specVersion = "2"
	// The terminal's size until a CLI that watches it sends its own.
	termCols, termRows = 120, 40
	// With a terminal every frame of a spinner is in the log, some 3 to 4 kB a second, and podman
	// starts the file over when it is full: this holds half a day to a day of it.
	logSizeMax  = 200 << 20
	tick        = 5 * time.Second
	stopTimeout = 2 * time.Minute
	maxBackoff  = 5 * time.Minute
	stableAfter = 2 * time.Minute // running this long forgets earlier crashes
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
	swapNow   bool      // the owner asked for the update: the next image swap does not wait
	eventsAt  time.Time // the newest node state change reported as an event

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
	if Busy(state) {
		s.waitingSince()
		return false
	}
	switch state {
	case "", "QUEUED", "JOINING_MARKET":
		if state == "" && s.WaitingForSOL != nil && s.WaitingForSOL() {
			return true
		}
		return time.Since(s.waitingSince()) >= maxWait
	}
	return true // starting, restarting, or waiting for SOL or its registration: nothing to lose
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

// Image is the node image the release names, the one the container runs or will run.
func (s *Supervisor) Image() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.image
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
		s.observe(ctx, nil, "")
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
	defer func() { s.observe(ctx, ctr, image) }()
	if swapNow && ctr != nil && s.current(ctr, image) {
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
		s.mu.Lock()
		s.status.LastError = nil // stopped as wanted: an earlier error (podman still starting at boot) is over
		s.mu.Unlock()
		return nil
	case ctr == nil:
		if restart {
			s.restartDone() // creating is the restart
		}
		if err := s.create(ctx, image); err != nil {
			return err
		}
		s.st.Info("node", "node container created on image %s", image)
		return nil
	case restart:
		slog.Info("node: restarting")
		if err := s.pc.StopContainer(ctx, ContainerName, stopTimeout); err != nil {
			return err
		}
		s.restartDone() // only once it happened; podman may have been away
		return s.startContainer(ctx, ctr.Config.Tty)

	case image != "" && !s.current(ctr, image) && (swapNow || s.MayInterrupt()):
		pulled, err := s.ensureImage(ctx, image) // the node keeps running during the pull
		if err != nil || pulled {
			return err // the swap waits for the next tick, which asks again whether the node is idle
		}
		slog.Info("node: recreating", "image", image, "network", s.network)
		if err := s.pc.RemoveContainer(ctx, ContainerName); err != nil {
			return err
		}
		s.ChangeApplied()
		if err := s.create(ctx, image); err != nil {
			return err
		}
		s.st.Info("node", "node restarted on image %s", image)
		return nil
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
	return s.restartExited(ctx, ctr)
}

// current reports whether ctr is the container the agent would create now.
func (s *Supervisor) current(ctr *podman.Container, image string) bool {
	l := ctr.Config.Labels
	return l[LabelImage] == image && l[labelNetwork] == s.network && l[labelSpec] == specVersion
}

// startContainer starts the node. Its terminal gets a size before its process runs: on a
// terminal without one the node's spinners flood the log with cursor moves.
func (s *Supervisor) startContainer(ctx context.Context, tty bool) error {
	if tty {
		if err := s.pc.InitContainer(ctx, ContainerName); err != nil {
			return fmt.Errorf("init node container: %w", err)
		}
		if err := s.pc.ResizeTTY(ctx, ContainerName, termCols, termRows); err != nil {
			return fmt.Errorf("size the node's terminal: %w", err)
		}
	}
	return s.pc.StartContainer(ctx, ContainerName)
}

// restartExited starts an exited node again: at once when it asked for it or the owner started
// it, otherwise with a backoff that doubles per crash.
func (s *Supervisor) restartExited(ctx context.Context, ctr *podman.Container) error {
	code := ctr.State.ExitCode
	s.mu.Lock()
	if s.start || code == exitRespawn {
		s.start = false
		s.mu.Unlock()
		slog.Info("node: starting", "exitCode", code)
		return s.startContainer(ctx, ctr.Config.Tty)
	}
	if s.nextStart.IsZero() {
		s.crashes++
		delay := min(5*time.Second<<min(s.crashes-1, 6), maxBackoff)
		s.nextStart = time.Now().Add(delay)
		msg := fmt.Sprintf("node exited with code %d; restart %d in %s", code, s.crashes, delay.Round(time.Second))
		s.status.LastError = &msg
		s.mu.Unlock()
		slog.Warn("node: crashed", "code", code, "restartIn", delay)
		s.st.Warn("node", "%s", msg)
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
	return s.startContainer(ctx, ctr.Config.Tty)
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
		Labels:  map[string]string{LabelImage: image, labelNetwork: s.network, labelSpec: specVersion},
		Mounts: []podman.Mount{
			{Source: config.NodeDir, Destination: "/root/.nosana"},
			{Source: config.PodmanSocketDir, Destination: "/root/.nosana/podman"},
		},
		Network:    networkName,
		Ports:      []podman.Port{{HostIP: "127.0.0.1", HostPort: apiPort, ContainerPort: 5001, Protocol: "tcp"}},
		LogSizeMax: logSizeMax,
		Terminal:   true,
	}
	if err := s.pc.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("create node container: %w", err)
	}
	slog.Info("node: created and starting", "image", image)
	return s.startContainer(ctx, true)
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

// Pull fetches the node image unless it is there already, so a node held back starts at once.
func (s *Supervisor) Pull(ctx context.Context) error {
	_, err := s.ensureImage(ctx, s.Image())
	return err
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

// observe refreshes the status from the container, its log and the node's own API.
func (s *Supervisor) observe(ctx context.Context, ctr *podman.Container, image string) {
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
			if info := readInfo(ctx); info != nil {
				status.State = &info.State
				status.Activity = info.Activity()
				if p := info.Prompt(); p != "" {
					status.Prompt = &p
				}
				var events []Event
				events, s.eventsAt = info.Events(s.eventsAt)
				for _, e := range events {
					s.st.Record("node", e.Level, "", "%s", e.Message)
				}
			}
		}
	}
	s.mu.Lock()
	status.Desired, status.Restarts, status.LastError = s.status.Desired, s.status.Restarts, s.status.LastError
	s.status, s.observed = status, true
	s.mu.Unlock()
}
