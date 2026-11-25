package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
)

// fakeReach answers the probes from answers in turn and counts them.
func fakeReach(ready *bool, answers ...bool) (*reach, *int) {
	n := 0
	r := newReach("https://n.example", "N", func() bool { return *ready })
	r.probe = func(context.Context, string, string) api.ReachStatus {
		ok := answers[n]
		n++
		st := api.ReachStatus{Ok: ok}
		if !ok {
			msg := "answered 503 Service Unavailable"
			st.Error = &msg
		}
		return st
	}
	return r, &n
}

func TestReachWaitsForTheTunnel(t *testing.T) {
	ready := false
	r, probes := fakeReach(&ready, true)
	now := time.Now()
	r.tick(context.Background(), now)
	if *probes != 0 {
		t.Fatal("probed before the node opened its tunnel")
	}
	if c := r.check(true); c.Status != api.CheckStatusSkip {
		t.Fatalf("before the tunnel: %s, want skip", c.Status)
	}
	ready = true
	r.tick(context.Background(), now)
	if c := r.check(true); *probes != 1 || c.Status != api.CheckStatusOk {
		t.Fatalf("after the tunnel: %d probes, %s", *probes, c.Status)
	}
}

func TestReachFailsOnlyTwiceInARow(t *testing.T) {
	ready := true
	r, probes := fakeReach(&ready, true, false, true, false, false)
	now := time.Now()
	r.tick(context.Background(), now) // ok
	now = now.Add(reachEvery)
	r.tick(context.Background(), now) // one failure: still ok
	if c := r.check(true); c.Status != api.CheckStatusOk {
		t.Fatalf("one failure: %s, want ok", c.Status)
	}
	r.tick(context.Background(), now.Add(reachRetry/2))
	if *probes != 2 {
		t.Fatal("probed again before the retry was due")
	}
	now = now.Add(reachRetry)
	r.tick(context.Background(), now) // ok again
	now = now.Add(reachEvery)
	r.tick(context.Background(), now) // failure
	now = now.Add(reachRetry)
	r.tick(context.Background(), now) // failure: the check fails
	if c := r.check(true); c.Status != api.CheckStatusFail || c.Fix == nil {
		t.Fatalf("two failures in a row: %s, want fail with a fix", c.Status)
	}
	if c := r.check(false); c.Status != api.CheckStatusSkip {
		t.Fatalf("stopped node: %s, want skip", c.Status)
	}
	ready = false
	r.tick(context.Background(), now)
	if r.status() != nil {
		t.Fatal("a node that closed its tunnel kept the old result")
	}
}
