package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/node"
)

const (
	// reachEvery is how often the agent asks the node's public domain for /node/info, the way
	// jobs and the explorer reach the node.
	reachEvery = 2 * time.Minute
	// reachRetry is the next probe after a failed one, so one slow answer is not a red check for
	// two minutes.
	reachRetry = 30 * time.Second
	// reachFailAfter failed probes in a row make the check fail.
	reachFailAfter = 2
)

type reach struct {
	url, address string
	ready        func() bool // the node opened its tunnel; before that the domain answers 503
	probe        func(ctx context.Context, url, address string) api.ReachStatus

	mu    sync.Mutex
	last  *api.ReachStatus // a success, or the failure that made the check fail; nil: not known yet
	fails int              // failed probes in a row
	next  time.Time        // the next probe
}

func newReach(url, address string, ready func() bool) *reach {
	return &reach{url: url, address: address, ready: ready, probe: probe}
}

func (r *reach) run(ctx context.Context) {
	for ctx.Err() == nil {
		sleep(ctx, 10*time.Second)
		r.tick(ctx, time.Now())
	}
}

// tick probes the domain when it is due. A node that has not opened its tunnel (just started,
// stopped, waiting for SOL) is not probed, and what was known about it is forgotten.
func (r *reach) tick(ctx context.Context, now time.Time) {
	if !r.ready() {
		r.mu.Lock()
		r.last, r.fails, r.next = nil, 0, time.Time{}
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	due := !now.Before(r.next)
	r.mu.Unlock()
	if due {
		r.refresh(ctx, now)
	}
}

// refresh probes now, whatever the schedule, as the owner's Run checks asks: the check it shows
// is fresh, not the result of the last scheduled probe.
func (r *reach) refresh(ctx context.Context, now time.Time) {
	if !r.ready() {
		return
	}
	st := r.probe(ctx, r.url, r.address)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.Ok {
		r.fails, r.next = 0, now.Add(reachEvery)
	} else {
		r.fails, r.next = r.fails+1, now.Add(reachRetry)
	}
	if st.Ok || r.fails >= reachFailAfter {
		r.last = &st
	}
}

func (r *reach) status() *api.ReachStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// probe turns node.Public into the heartbeat's reach status.
func probe(ctx context.Context, url, address string) api.ReachStatus {
	st := api.ReachStatus{Url: url, CheckedAt: time.Now().UTC(), Ok: true}
	if _, err := node.Public(ctx, url, address); err != nil {
		msg := err.Error()
		st.Ok, st.Error = false, &msg
	}
	return st
}

// check turns the probe into a doctor check, skipped while the node is not running or has not
// opened its tunnel yet: it cannot be reached then.
func (r *reach) check(nodeRunning bool) *api.Check {
	st := r.status()
	group := "network"
	c := api.Check{Id: "node-domain", Group: &group, Title: "Node reachable at its public domain", Status: api.CheckStatusOk}
	detail := r.url
	switch {
	case !nodeRunning:
		c.Status, detail = api.CheckStatusSkip, "the node is not running"
	case st == nil:
		c.Status, detail = api.CheckStatusSkip, "the node is still opening its tunnel"
	case st.Ok:
	default:
		detail += ": " + *st.Error
		c.Status = api.CheckStatusFail
		fix := "The node runs but the network cannot reach it: look for tunnel errors in nosana-host logs node; a node restart usually sets it up again."
		c.Fix = &fix
	}
	c.Detail = &detail
	return &c
}
