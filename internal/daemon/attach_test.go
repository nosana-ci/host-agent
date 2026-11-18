package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/store"
)

// serveUnix serves h on a unix socket in a temporary directory and returns the socket's path.
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

// fakePodman is a podman with a node container whose terminal draws a frame and then shows the
// keys it gets.
func fakePodman(t *testing.T, running, tty bool) *podman.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v5.0.0/libpod/containers/nosana-node/json", func(w http.ResponseWriter, _ *http.Request) {
		status := "exited"
		if running {
			status = "running"
		}
		fmt.Fprintf(w, `{"State":{"Status":%q,"Running":%t},"Config":{"Tty":%t}}`, status, running, tty)
	})
	mux.HandleFunc("POST /v5.0.0/libpod/containers/nosana-node/attach", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stdin") != "true" || r.URL.Query()["detachKeys"] == nil {
			t.Errorf("attach query %q", r.URL.RawQuery)
		}
		conn, buf, _ := http.NewResponseController(w).Hijack()
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nframe")
		_ = buf.Flush()
		key := make([]byte, 1)
		for {
			if _, err := buf.Read(key); err != nil {
				return
			}
			_, _ = conn.Write([]byte("key:" + string(key)))
		}
	})
	return podman.New(serveUnix(t, mux))
}

func attachTo(t *testing.T, pc *podman.Client) (net.Conn, error) {
	d := &daemon{pc: pc, cmds: make(chan []job, 1)}
	old := local.SocketPath
	local.SocketPath = serveUnix(t, d.localHandler())
	t.Cleanup(func() { local.SocketPath = old })
	return local.Attach(context.Background(), "/node/attach")
}

// The CLI's attach is a two-way stream with the node's terminal: what the node draws comes out,
// and what goes in are its keys.
func TestAttachJoinsTheNodesTerminal(t *testing.T) {
	conn, err := attachTo(t, fakePodman(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	read := func(want string) {
		t.Helper()
		got := make([]byte, len(want))
		for n := 0; n < len(got); {
			m, err := conn.Read(got[n:])
			if err != nil {
				t.Fatalf("got %q, %v; want %q", got[:n], err, want)
			}
			n += m
		}
		if string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	read("frame")
	if _, err := conn.Write([]byte("s")); err != nil {
		t.Fatal(err)
	}
	read("key:s")
}

func TestAttachNeedsARunningNodeWithATerminal(t *testing.T) {
	for want, pc := range map[string]*podman.Client{
		"not running":     fakePodman(t, false, true),
		"no terminal yet": fakePodman(t, true, false), // a container from before the node had a terminal
	} {
		if _, err := attachTo(t, pc); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want an error with %q", err, want)
		}
	}
}

// What node attach waits for, in the order the agent goes through.
func TestNodeStart(t *testing.T) {
	crashed := "node exited with code 1; restart 2 in 10s"
	node := func(container string, desired api.NodeStatusDesired, lastError *string) api.NodeStatus {
		return api.NodeStatus{Container: container, Desired: desired, Image: "node:1", LastError: lastError}
	}
	for name, c := range map[string]struct {
		node    api.NodeStatus
		moving  string
		pulling bool
		want    local.NodeStart
	}{
		"running":                   {node("running", api.Running, &crashed), "", false, local.NodeStart{Running: true}},
		"a takeover goes first":     {node("missing", api.Stopped, nil), "the old node finishes its job first", true, local.NodeStart{Waiting: "the old node finishes its job first"}},
		"stopped by its owner":      {node("exited", api.Stopped, nil), "", false, local.NodeStart{}},
		"the image is being pulled": {node("missing", api.Running, &crashed), "", true, local.NodeStart{Waiting: "the agent pulls the node image node:1"}},
		"a crash's backoff":         {node("exited", api.Running, &crashed), "", false, local.NodeStart{Waiting: crashed}},
		"about to start":            {node("missing", api.Running, nil), "", false, local.NodeStart{Waiting: "the node container starts"}},
	} {
		if got := nodeStart(c.node, c.moving, c.pulling); got != c.want {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
	}
}

// node stop, node start and node attach in a row: the supervisor has not been round yet, so its
// view still has the node stopped, or running. What podman and the owner's last command say counts.
func TestNodeStartNowDoesNotTrustTheSupervisorsLastRound(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Update(func(s *store.State) { s.NodeStopped = true })
	exited := fakePodman(t, false, true)
	d := &daemon{pc: exited, st: st, sup: node.New(exited, st, "mainnet")} // the supervisor saw: stopped
	if got := d.nodeStartNow(context.Background()); got != (local.NodeStart{}) {
		t.Fatalf("a stopped node: got %+v", got)
	}
	_ = st.Update(func(s *store.State) { s.NodeStopped = false }) // node start
	if got := d.nodeStartNow(context.Background()); got.Running || got.Waiting == "" {
		t.Fatalf("a node just started is waited for: got %+v", got)
	}
	d.pc = fakePodman(t, true, true) // the container runs before the supervisor's next round
	if got := d.nodeStartNow(context.Background()); !got.Running {
		t.Fatalf("a running container: got %+v", got)
	}
}
