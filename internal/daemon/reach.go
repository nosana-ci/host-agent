package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/node"
)

// reachEvery is how often the agent asks the node's public domain for /node/info, the way jobs
// and the explorer reach the node.
const reachEvery = 2 * time.Minute

type reach struct {
	url, address string

	mu   sync.Mutex
	last *api.ReachStatus
}

func (r *reach) run(ctx context.Context) {
	sleep(ctx, 20*time.Second) // the tunnel needs the node up first
	for ctx.Err() == nil {
		r.refresh(ctx)
		sleep(ctx, reachEvery)
	}
}

func (r *reach) refresh(ctx context.Context) {
	next := probe(ctx, r.url, r.address)
	r.mu.Lock()
	r.last = &next
	r.mu.Unlock()
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

// check turns the probe into a doctor check; a node that is not running cannot be reached, so
// the check is skipped then.
func (r *reach) check(nodeRunning bool) *api.Check {
	st := r.status()
	if st == nil {
		return nil
	}
	group := "network"
	c := api.Check{Id: "node-domain", Group: &group, Title: "Node reachable at its public domain", Status: api.CheckStatusOk}
	detail := st.Url
	switch {
	case st.Ok:
	case nodeRunning:
		detail += ": " + *st.Error
		c.Status = api.CheckStatusFail
		fix := "The node runs but the network cannot reach it: look for tunnel errors in nosana-host logs node; a node restart usually sets it up again."
		c.Fix = &fix
	default:
		c.Status, detail = api.CheckStatusSkip, "the node is not running"
	}
	c.Detail = &detail
	return &c
}
