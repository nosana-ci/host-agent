package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Recorded events survive a restart (the exec into a new agent) and go once host-manager took them.
func TestEventsAreKeptUntilDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, _ := Open(path)
	st.Info("update", "agent updated to %s (was %s)", "1.0.4", "1.0.3")
	st.Record("command", "error", "stop-node", "%s", "podman did not answer")
	select {
	case <-st.EventsAdded():
	default:
		t.Fatal("no signal after recording")
	}

	st, _ = Open(path)
	evs := st.State().Events
	if len(evs) != 2 || evs[0].Message != "agent updated to 1.0.4 (was 1.0.3)" || evs[0].Level != "info" ||
		evs[1].Verb != "stop-node" || evs[1].Level != "error" || evs[0].ID == evs[1].ID || evs[0].At.IsZero() {
		t.Fatalf("got %+v", evs)
	}
	if err := st.Delivered([]string{evs[0].ID}); err != nil {
		t.Fatal(err)
	}
	if left := st.State().Events; len(left) != 1 || left[0].ID != evs[1].ID {
		t.Fatalf("left %+v", left)
	}
}

// The outbox drops its oldest events beyond maxEvents and cuts a message to host-manager's limit.
func TestEventsAreBounded(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	for i := range maxEvents + 5 {
		st.Warn("node", "crash %d", i)
	}
	st.Info("node", "%s", strings.Repeat("é", 2*maxMessage))
	evs := st.State().Events
	if len(evs) != maxEvents || evs[0].Message != "crash 6" {
		t.Fatalf("%d events, first %q", len(evs), evs[0].Message)
	}
	if n := len([]rune(evs[len(evs)-1].Message)); n != maxMessage {
		t.Fatalf("message of %d characters", n)
	}
	var nilStore *Store
	nilStore.Info("node", "nothing") // tests build daemons without a store
}
