package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nosana-ci/nosana-host/internal/api"
	"github.com/nosana-ci/nosana-host/internal/keys"
)

func TestSignedMessageMatchesHostManager(t *testing.T) {
	// The same vector host-manager's deviceSignature test uses.
	got := string(SignedMessage("POST", "/agent/heartbeat", "1700000000", []byte("{}")))
	want := "POST\n/agent/heartbeat\n1700000000\n44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestHeartbeatIsSignedWithTheDeviceKey(t *testing.T) {
	_, wallet, _ := ed25519.GenerateKey(rand.Reader)
	_, device, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Nosana-Signature"))
		msg := SignedMessage(r.Method, r.URL.Path, r.Header.Get("X-Nosana-Timestamp"), body)
		if !ed25519.Verify(device.Public().(ed25519.PublicKey), msg, sig) {
			http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Nosana-Node") != keys.Address(wallet) {
			http.Error(w, `{"message":"wrong node"}`, http.StatusUnauthorized)
			return
		}
		sum := sha256.Sum256(body)
		if !strings.Contains(string(msg), hex.EncodeToString(sum[:])) || !strings.Contains(string(body), `"status"`) {
			http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"commands":[{"id":3,"verb":"run-doctor","args":{}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, wallet, device)
	cmds, err := c.Heartbeat(context.Background(), api.AgentStatus{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Verb != "run-doctor" || cmds[0].Id != 3 {
		t.Fatalf("got %+v", cmds)
	}
}

func TestClaimUsesTheWalletHeaderAndErrorsAreTyped(t *testing.T) {
	_, wallet, _ := ed25519.GenerateKey(rand.Reader)
	_, device, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/claim":
			parts := strings.Split(r.Header.Get("Authorization"), ":")
			if len(parts) != 3 || parts[0] != keys.Address(wallet) {
				http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			http.Error(w, `{"message":"claim token expired"}`, http.StatusForbidden)
		case "/agent/heartbeat":
			http.Error(w, `{"message":"unknown host"}`, 404)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, wallet, device)
	err := c.Claim(context.Background(), "nht_x", "box")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 || he.Message != "claim token expired" {
		t.Fatalf("got %v", err)
	}
	_, err = c.Heartbeat(context.Background(), api.AgentStatus{})
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("got %v", err)
	}
}
