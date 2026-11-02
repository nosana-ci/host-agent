package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/version"
)

// serveLocal answers the CLI on the local socket until ctx ends.
func (d *daemon) serveLocal(ctx context.Context) {
	l, err := local.Listen()
	if err != nil {
		slog.Warn("local socket", "err", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		s := d.st.State()
		writeJSON(w, local.Status{
			Agent: version.Version, NodeAddress: d.address, ControlURL: d.cfg.ControlURL, Network: d.cfg.Network,
			Claimed: s.Claimed, ClaimPending: s.ClaimToken != "", Status: d.status(r.Context()), Update: s.Update,
		})
	})
	mux.HandleFunc("POST /doctor", func(w http.ResponseWriter, r *http.Request) {
		if id := r.URL.Query().Get("fix"); id != "" {
			if err := doctor.Fix(r.Context(), id); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		writeJSON(w, d.runDoctor(r.Context()))
	})
	mux.HandleFunc("GET /logs/{what}", func(w http.ResponseWriter, r *http.Request) {
		tail, follow := r.URL.Query().Get("tail"), r.URL.Query().Get("follow") == "true"
		out := flushWriter{w}
		var err error
		switch r.PathValue("what") {
		case "node":
			n, _ := strconv.Atoi(tail)
			err = d.pc.Logs(r.Context(), node.ContainerName, n, follow, out)
		case "agent":
			args := []string{"-u", service.AgentUnit, "--no-pager", "-n", tail}
			if follow {
				args = append(args, "-f")
			}
			cmd := exec.CommandContext(r.Context(), "journalctl", args...)
			cmd.Stdout, cmd.Stderr = out, out
			err = cmd.Run()
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil && r.Context().Err() == nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("local socket", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// flushWriter sends every write on, so logs stream instead of buffering.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	_ = http.NewResponseController(f.w).Flush()
	return n, err
}

var _ io.Writer = flushWriter{}
