package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/keys"
)

// A test host-manager that answers requests whose authorization header names this wallet.
func server(t *testing.T, wallet ed25519.PrivateKey, handle http.HandlerFunc) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.Header.Get("Authorization"), ":")
		if len(parts) != 3 || parts[0] != keys.Address(wallet) {
			http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, wallet)
}

func TestHeartbeatReturnsCommands(t *testing.T) {
	_, wallet, _ := ed25519.GenerateKey(rand.Reader)
	c := server(t, wallet, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/heartbeat" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"commands":[{"id":3,"verb":"run-doctor","args":{}}]}`))
	})
	cmds, err := c.Heartbeat(context.Background(), api.AgentStatus{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Verb != "run-doctor" || cmds[0].Id != 3 {
		t.Fatalf("got %+v", cmds)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	_, wallet, _ := ed25519.GenerateKey(rand.Reader)
	c := server(t, wallet, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/claim":
			http.Error(w, `{"message":"claim token expired"}`, http.StatusForbidden)
		default:
			http.Error(w, `{"message":"unknown host"}`, http.StatusNotFound)
		}
	})
	var he *HTTPError
	err := c.Claim(context.Background(), "nht_x", "box")
	if !errors.As(err, &he) || he.Status != 403 || he.Message != "claim token expired" {
		t.Fatalf("got %v", err)
	}
	if _, err := c.WaitCommands(context.Background()); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("got %v", err)
	}
}
