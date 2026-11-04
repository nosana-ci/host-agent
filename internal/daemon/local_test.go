package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/control"
)

// A CLI command is queued for commandLoop with its args, and the socket answers with the result.
func TestLocalCommandIsQueuedAndAnswered(t *testing.T) {
	d := &daemon{cmds: make(chan []job, 1)}
	go func() {
		jobs := <-d.cmds
		j := jobs[0]
		j.answer(api.CommandResult{Ok: true, Message: j.Verb + " " + j.Args["id"].(string)})
	}()

	rec := httptest.NewRecorder()
	d.localHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/commands/fix-check", strings.NewReader(`{"id":"podman"}`)))

	var r api.CommandResult
	if err := json.NewDecoder(rec.Body).Decode(&r); err != nil {
		t.Fatal(rec.Code, err)
	}
	if !r.Ok || r.Message != "fix-check podman" {
		t.Fatalf("got %+v", r)
	}
}

func TestLocalCommandRejectsBadArgs(t *testing.T) {
	d := &daemon{cmds: make(chan []job, 1)}
	rec := httptest.NewRecorder()
	d.localHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/commands/stop-node", strings.NewReader(`[`)))
	if rec.Code != http.StatusBadRequest || len(d.cmds) != 0 {
		t.Fatalf("code %d, queued %d", rec.Code, len(d.cmds))
	}
}

// doctor --fix is the console's fix-check through the queue; the socket answers with the checks
// that run left, or the fix's error.
func TestLocalDoctorFixIsQueued(t *testing.T) {
	d := &daemon{cmds: make(chan []job, 1)}
	go func() {
		j := (<-d.cmds)[0]
		if j.Verb != "fix-check" || j.Args["id"] != "podman" {
			j.answer(api.CommandResult{Message: "wrong command " + j.Verb})
			return
		}
		d.mu.Lock()
		d.checks = []api.Check{{Id: "podman", Status: api.CheckStatusOk}}
		d.mu.Unlock()
		j.answer(api.CommandResult{Ok: true})
	}()

	rec := httptest.NewRecorder()
	d.localHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/doctor?fix=podman", nil))
	var checks []api.Check
	if err := json.NewDecoder(rec.Body).Decode(&checks); err != nil {
		t.Fatal(rec.Code, err)
	}
	if len(checks) != 1 || checks[0].Id != "podman" {
		t.Fatalf("got %+v", checks)
	}

	go func() { (<-d.cmds)[0].answer(api.CommandResult{Message: `no fix for check "nope"`}) }()
	rec = httptest.NewRecorder()
	d.localHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/doctor?fix=nope", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nope") {
		t.Fatalf("code %d, body %q", rec.Code, rec.Body.String())
	}
}

// register signs the node's registration with host-manager, after the CLI had the agreement
// accepted; without the acceptance or with a bad email it never reaches host-manager. The
// current details are read from host-manager for the CLI to show.
func TestLocalRegister(t *testing.T) {
	var got map[string]string
	hm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/registration" || r.Header.Get("Authorization") == "" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"registration":{"email":"old@example.com","twitter":"old"}}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"registration":` + mustJSON(got) + `}`))
	}))
	defer hm.Close()
	_, wallet, _ := ed25519.GenerateKey(rand.Reader)
	d := &daemon{ctrl: control.New(hm.URL, wallet)}
	post := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		d.localHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/register?"+query, nil))
		return rec
	}

	if rec := post("email=me%40example.com"); rec.Code != http.StatusBadRequest || got != nil {
		t.Fatalf("without the agreement: %d %v", rec.Code, got)
	}
	if rec := post("email=nope&accept=1"); rec.Code != http.StatusBadRequest || got != nil {
		t.Fatalf("bad email: %d %v", rec.Code, got)
	}
	if rec := post("email=+me%40example.com+&discord=me&twitter=&accept=1"); rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	if _, ok := got["twitter"]; got["email"] != "me@example.com" || got["discord"] != "me" || ok {
		t.Fatalf("host-manager got %v", got)
	}

	rec := httptest.NewRecorder()
	d.localHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/registration", nil))
	var current api.AgentRegistration
	if err := json.NewDecoder(rec.Body).Decode(&current); err != nil || current.Registration == nil || current.Registration.Email != "old@example.com" || *current.Registration.Twitter != "old" {
		t.Fatalf("GET /registration: %d %s (%v)", rec.Code, rec.Body, err)
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
