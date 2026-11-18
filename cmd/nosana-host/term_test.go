package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nosana-ci/host-agent/internal/local"
)

// waitForNode against an agent that gives the answers in turn, the last one from then on.
func wait(t *testing.T, answers ...local.NodeStart) (string, error) {
	asked := 0
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "cli.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(answers[min(asked, len(answers)-1)])
		asked++
	})}
	go func() { _ = srv.Serve(l) }()
	old, every := local.SocketPath, waitEvery
	local.SocketPath, waitEvery = l.Addr().String(), time.Millisecond
	defer func() { _, local.SocketPath, waitEvery = srv.Close(), old, every }()
	var out bytes.Buffer
	err = waitForNode(&out)
	return out.String(), err
}

func TestWaitForNode(t *testing.T) {
	pull := local.NodeStart{Waiting: "the agent pulls the node image"}
	out, err := wait(t, pull, pull, local.NodeStart{Waiting: "the node container starts"}, local.NodeStart{Running: true})
	if err != nil || strings.Count(out, "\n") != 2 || strings.Count(out, pull.Waiting) != 1 {
		t.Errorf("each thing waited for is said once: got %q, %v", out, err)
	}
	if out, err := wait(t, local.NodeStart{Running: true}); err != nil || out != "" {
		t.Errorf("a running node: got %q, %v", out, err)
	}
	if _, err := wait(t, local.NodeStart{}); err == nil {
		t.Error("a node its owner stopped is not waited for")
	}
}
