package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/control"
	"github.com/nosana-ci/host-agent/internal/store"
)

func eventDaemon(t *testing.T, handle http.HandlerFunc) *daemon {
	hm := httptest.NewServer(handle)
	t.Cleanup(hm.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, wallet, _ := ed25519.GenerateKey(rand.Reader)
	return &daemon{st: st, ctrl: control.New(hm.URL, wallet)}
}

// Events wait until the host is linked, go in batches of eventBatch, and leave the state once
// host-manager took them.
func TestSendEventsInBatches(t *testing.T) {
	var batches [][]api.AgentEvent
	d := eventDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		var body api.AgentEventsBody
		if r.URL.Path != "/agent/events" || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		batches = append(batches, body.Events)
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	for range eventBatch + 1 {
		d.st.Warn("node", "node exited with code 1")
	}
	d.st.Record("command", "info", "stop-node", "node stopping")
	if err := d.sendEvents(context.Background()); err != nil || len(batches) != 0 {
		t.Fatalf("sent while not linked: %v, %d batches", err, len(batches))
	}

	_ = d.st.Update(func(s *store.State) { s.Claimed = true })
	if err := d.sendEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != eventBatch || len(batches[1]) != 2 {
		t.Fatalf("batches of %d", len(batches))
	}
	last := batches[1][1]
	if last.Type != "command" || last.Verb == nil || *last.Verb != "stop-node" || last.Level != api.AgentEventLevelInfo {
		t.Fatalf("got %+v", last)
	}
	if left := d.st.State().Events; len(left) != 0 {
		t.Fatalf("%d events left", len(left))
	}
}

// A host-manager that is away keeps the events; one that refuses them as invalid drops them.
func TestSendEventsFailures(t *testing.T) {
	status := http.StatusBadGateway
	d := eventDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"nope"}`))
	})
	_ = d.st.Update(func(s *store.State) { s.Claimed = true })
	d.st.Info("agent", "agent 1.0.4 started")
	if err := d.sendEvents(context.Background()); err == nil || len(d.st.State().Events) != 1 {
		t.Fatalf("err %v, %d events", err, len(d.st.State().Events))
	}
	status = http.StatusUnprocessableEntity
	if err := d.sendEvents(context.Background()); err != nil || len(d.st.State().Events) != 0 {
		t.Fatalf("err %v, %d events", err, len(d.st.State().Events))
	}
}

// A command typed on the machine is recorded as an event; the console's are not, host-manager has
// those already.
func TestCLICommandsAreRecorded(t *testing.T) {
	d := eventDaemon(t, http.NotFound)
	d.runJobs(context.Background(), []job{
		{AgentCommand: api.AgentCommand{Verb: "nope"}, answer: func(api.CommandResult) {}, cli: true},
		{AgentCommand: api.AgentCommand{Verb: "nope"}, answer: func(api.CommandResult) {}},
	})
	evs := d.st.State().Events
	if len(evs) != 1 || evs[0].Type != "command" || evs[0].Verb != "nope" || evs[0].Level != "error" || evs[0].Message != `unknown command "nope"` {
		t.Fatalf("got %+v", evs)
	}
}
