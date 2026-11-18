package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nosana-ci/host-agent/internal/local"
)

// fakeAgent answers GET /node/start with the given answers in turn, the last one from then on.
// Its attach finds the node not running the first time, as when the node stopped in between.
func fakeAgent(t *testing.T, answers ...local.NodeStart) {
	t.Helper()
	asked, attached := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node/start", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(answers[min(asked, len(answers)-1)])
		asked++
	})
	mux.HandleFunc("POST /node/attach", func(w http.ResponseWriter, _ *http.Request) {
		if attached++; attached == 1 {
			http.Error(w, "the node is not running", http.StatusConflict)
			return
		}
		conn, buf, _ := http.NewResponseController(w).Hijack()
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nframe")
		_ = buf.Flush()
	})
	old, every := local.SocketPath, waitEvery
	local.SocketPath, waitEvery = filepath.Join(t.TempDir(), "cli.sock"), time.Millisecond
	l, err := net.Listen("unix", local.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		_ = srv.Close()
		local.SocketPath, waitEvery = old, every
	})
}

// node attach waits for a node that is on its way and says what for, each thing once.
func TestWaitForNodeSaysWhatItWaitsFor(t *testing.T) {
	pull := local.NodeStart{Waiting: "the agent pulls the node image node:1"}
	fakeAgent(t, pull, pull, local.NodeStart{Waiting: "the node container starts"}, local.NodeStart{Running: true})
	var out bytes.Buffer
	if err := waitForNode(&out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "waiting for the node to start") ||
		lines[1] != "  the agent pulls the node image node:1" || lines[2] != "  the node container starts" {
		t.Fatalf("got %q", out.String())
	}
}

func TestWaitForNodeIsSilentForARunningNode(t *testing.T) {
	fakeAgent(t, local.NodeStart{Running: true})
	var out bytes.Buffer
	if err := waitForNode(&out); err != nil || out.Len() != 0 {
		t.Fatalf("got %v, %q", err, out.String())
	}
}

// A node its owner stopped does not start by itself: waiting for it would never end.
func TestWaitForNodeDoesNotWaitForAStoppedNode(t *testing.T) {
	fakeAgent(t, local.NodeStart{})
	if err := waitForNode(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "node start") {
		t.Fatalf("got %v", err)
	}
}

// Right after an install the agent is still starting; an agent that stays away is an error.
func TestWaitForNodeGivesAStartingAgentTime(t *testing.T) {
	old, grace, every := local.SocketPath, agentGrace, waitEvery
	local.SocketPath, agentGrace, waitEvery = filepath.Join(t.TempDir(), "none.sock"), 20*time.Millisecond, time.Millisecond
	defer func() { local.SocketPath, agentGrace, waitEvery = old, grace, every }()
	var out bytes.Buffer
	if err := waitForNode(&out); !errors.Is(err, local.ErrNotRunning) || !strings.Contains(out.String(), "the agent starts") {
		t.Fatalf("got %v, %q", err, out.String())
	}
}

// The agent's answer and the attach are two requests: a node that stopped in between (it was
// just stopped, or it restarts) is waited for again instead of an error.
func TestAttachWaitsAgainWhenTheNodeStoppedInBetween(t *testing.T) {
	fakeAgent(t, local.NodeStart{Running: true})
	conn, err := attachWhenRunning(context.Background(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got, _ := io.ReadAll(conn); string(got) != "frame" {
		t.Fatalf("got %q", got)
	}
}
