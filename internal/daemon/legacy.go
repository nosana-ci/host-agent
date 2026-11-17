package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/legacy"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/store"
)

const (
	// handoverEvery is how often a handover asks the old node what it does: the old node is stopped
	// the moment it is seen between jobs, before the market hands it the next one.
	handoverEvery = 5 * time.Second
	// legacyEvery is how often the agent of a moved host looks for the old node.
	legacyEvery = time.Minute
)

// step is what a handover does next.
type step int

const (
	waitJob       step = iota // the old node runs a job or a benchmark
	waitUnknown               // it runs in docker but does not answer, so its job is unknown
	stopOld                   // it is between jobs, or stopped by itself
	waitElsewhere             // nothing runs in docker, but the node answers at its domain
	startNode                 // the old node is gone
)

// nextStep decides from whether the old node runs in docker, whether it took the drain request,
// and what its domain answers. A tunnel may answer for a while after its node stopped, so an answer
// alone is waited out.
func nextStep(inDocker, drained bool, info *node.Info, err error) step {
	switch {
	case inDocker && drained:
		return stopOld
	case inDocker && err != nil:
		return waitUnknown
	case inDocker && node.Busy(info.State):
		return waitJob
	case inDocker:
		return stopOld
	case err == nil:
		return waitElsewhere
	}
	return startNode
}

// handover is what a handover in this agent run has done and said.
type handover struct {
	pulled bool
	step   step
	since  time.Time // when step began
	told   bool      // step is in Activity
}

// legacyLoop takes the node over from the old one after a node import, then keeps the old node
// stopped (guard).
func (d *daemon) legacyLoop(ctx context.Context) {
	h := &handover{step: -1}
	told := false
	for ctx.Err() == nil {
		wait := legacyEvery
		switch st := d.st.State(); {
		case st.Handover && !st.NodeStopped: // the owner started the node: the guard takes it from here
			_ = d.st.Update(func(s *store.State) { s.Handover = false })
			d.setMoving("")
		case st.Handover:
			d.handOver(ctx, h)
			wait = handoverEvery
		case st.Moved && !st.NodeStopped:
			d.guard(ctx, &told)
		}
		sleep(ctx, wait)
	}
}

// handOver pulls the node image while the old node still runs, stops the old node once it stopped
// itself (node import asked it to, with legacy.Drain) or is seen between jobs, and starts the
// agent's node once the old one no longer answers at its domain.
func (d *daemon) handOver(ctx context.Context, h *handover) {
	if !h.pulled {
		d.setMoving("pulling the node image while the old node runs")
		if err := d.sup.Pull(ctx); err != nil {
			d.setMoving(fmt.Sprintf("the node image could not be pulled (%v); trying again", err))
			return
		}
		h.pulled = true
	}
	names := legacy.Running(ctx, d.address)
	where := strings.Join(names, ", ")
	info, err := node.Public(ctx, d.reach.url, d.address)
	next := nextStep(len(names) > 0, legacy.Drained(ctx, d.address), info, err)
	if next != h.step {
		h.step, h.since, h.told = next, time.Now(), false
	}
	switch next {
	case stopOld:
		if err := legacy.Stop(ctx, d.address); err != nil {
			h.say(d, "warn", 0, fmt.Sprintf("the old node could not be stopped: %v; trying again", err))
			return
		}
		d.st.Info("node", "the old node is stopped between jobs (%s)", where)
		d.setMoving("waiting until the old node no longer answers at " + d.reach.url)
	case startNode:
		_ = d.st.Update(func(s *store.State) { s.Handover = false })
		_ = d.sup.Start()
		d.st.Info("node", "the old node is gone; the node runs under the agent")
		d.setMoving("")
	case waitJob:
		h.say(d, "info", 0, fmt.Sprintf("the old node finishes its job first (%s)", info.Status))
	case waitUnknown:
		h.say(d, "warn", 2*time.Minute, fmt.Sprintf("the old node (%s) does not answer at %s, so whether it runs a job is unknown; the move goes on once it answers, or once it is stopped: docker stop %s", where, d.reach.url, strings.Join(names, " ")))
	case waitElsewhere:
		h.say(d, "warn", 3*time.Minute, fmt.Sprintf("the node answers at %s, but no old node runs in docker here: something else runs this key; stop it, and the move goes on", d.reach.url))
	}
}

// say shows msg as what the handover waits for, and puts it in Activity once it has waited after.
func (h *handover) say(d *daemon, level string, after time.Duration, msg string) {
	d.setMoving(msg)
	if !h.told && time.Since(h.since) >= after {
		h.told = true
		d.st.Record("node", level, "", "%s", msg)
	}
}

// guard keeps the old node stopped on a host that moved from it, also after a reboot or when a
// service of the owner's own starts it again: the market queue holds one node per key, and it must
// be the agent's. While the agent's node is stopped by the owner, the old one is left alone. The
// owner hears about it once per agent run; the journal has every time.
func (d *daemon) guard(ctx context.Context, told *bool) {
	names := legacy.Running(ctx, d.address)
	if len(names) == 0 {
		return
	}
	where := strings.Join(names, ", ")
	err := legacy.Stop(ctx, d.address)
	if err != nil {
		slog.Warn("legacy: the old node runs again and could not be stopped", "containers", where, "err", err)
	} else {
		slog.Info("legacy: the old node ran again and is stopped", "containers", where)
	}
	if *told {
		return
	}
	*told = true
	if err != nil {
		d.st.Error("node", "the old node runs on this machine with this node's key (%s) and could not be stopped: %v", where, err)
	} else {
		d.st.Warn("node", "the old node was started again on this machine (%s) and is stopped; disable what starts it", where)
	}
}

// setMoving is what a handover waits for, for the CLI's status; "" when none runs.
func (d *daemon) setMoving(msg string) {
	d.mu.Lock()
	d.moving = msg
	d.mu.Unlock()
}
