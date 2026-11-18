package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nosana-ci/host-agent/internal/local"
)

// fakeAgent answers GET /node/start with the given answers in turn, the last one from then on.
func fakeAgent(t *testing.T, answers ...local.NodeStart) {
	t.Helper()
	asked := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node/start", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(answers[min(asked, len(answers)-1)])
		asked++
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

// Podman reports leaving with the detach key as an error, and a node that stopped as none.
func TestAttachEnd(t *testing.T) {
	failed := errors.New("exit status 125")
	for name, c := range map[string]struct {
		err              error
		said, out, error string
	}{
		"left with Ctrl+C":        {failed, "Error: detached from container\n", "detached; the node keeps running\n", ""},
		"the node stopped":        {nil, "", "the node's terminal closed: the node stopped\n", ""},
		"podman's own error":      {failed, "Error: you can only attach to running containers\n", "", "podman attach: you can only attach to running containers"},
		"podman could not be run": {failed, "", "", "podman attach: exit status 125"},
	} {
		var out bytes.Buffer
		err := attachEnd(c.err, c.said, &out)
		if out.String() != c.out || (err == nil) != (c.error == "") || err != nil && err.Error() != c.error {
			t.Errorf("%s: got %q, %v", name, out.String(), err)
		}
	}
}
