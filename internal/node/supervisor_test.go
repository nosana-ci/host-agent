package node

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/store"
)

func TestMayInterrupt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, st, "mainnet")
	unfunded := false
	s.WaitingForSOL = func() bool { return unfunded }
	node := func(container, state string) {
		s.observed = true
		s.status = api.NodeStatus{Container: container}
		if state != "" {
			s.status.State = &state
		}
	}
	waitedFor := func(d time.Duration) {
		since := time.Now().Add(-d)
		_ = st.Update(func(st *store.State) { st.Update.WaitingSince = &since })
	}

	if s.MayInterrupt() {
		t.Fatal("a node not observed yet may not be interrupted")
	}
	node("exited", "")
	if !s.MayInterrupt() {
		t.Fatal("a stopped node may be interrupted")
	}

	node("running", "RUNNING_JOB")
	waitedFor(48 * time.Hour)
	if s.MayInterrupt() {
		t.Fatal("a job is always waited for")
	}

	s.ChangeApplied()
	node("running", "QUEUED")
	if s.MayInterrupt() || st.State().Update.WaitingSince == nil {
		t.Fatal("a queued node waits, and the wait is recorded")
	}
	waitedFor(25 * time.Hour)
	if !s.MayInterrupt() {
		t.Fatal("a change waits at most maxWait for a queued node")
	}

	s.ChangeApplied()
	node("running", "")
	if s.MayInterrupt() {
		t.Fatal("a funded node whose API does not answer waits")
	}
	unfunded = true
	if !s.MayInterrupt() {
		t.Fatal("a node waiting for SOL may be interrupted")
	}

	node("running", "STARTING")
	if !s.MayInterrupt() {
		t.Fatal("a starting node may be interrupted")
	}
}
