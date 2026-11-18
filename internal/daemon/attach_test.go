package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/podman"
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
		fmt.Fprintf(w, `{"State":{"Running":%t},"Config":{"Tty":%t}}`, running, tty)
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
