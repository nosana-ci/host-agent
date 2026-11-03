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

	"github.com/nosana-ci/host-agent/internal/api"
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
	go func() {
		<-ctx.Done()
		_ = d.cli.Close()
	}()
	if err := d.cli.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("local socket", "err", err)
	}
}

// closeLocal lets the CLI requests in flight finish, so a command that leads to an exec is still
// answered. A followed log does not end by itself, hence the limit.
func (d *daemon) closeLocal() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.cli.Shutdown(ctx)
}

func (d *daemon) localHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		s := d.st.State()
		writeJSON(w, local.Status{
			Agent: version.Version, NodeAddress: d.address, ControlURL: d.cfg.ControlURL, Network: d.cfg.Network,
			Claimed: s.Claimed, ClaimPending: s.ClaimToken != "", Status: d.status(r.Context()), Update: s.Update,
		})
	})
	mux.HandleFunc("POST /commands/{verb}", func(w http.ResponseWriter, r *http.Request) {
		c := api.AgentCommand{Verb: r.PathValue("verb")}
		if err := json.NewDecoder(r.Body).Decode(&c.Args); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "args: "+err.Error(), http.StatusBadRequest)
			return
		}
		if result, ok := d.runQueued(r.Context(), c); ok {
			writeJSON(w, result)
		}
	})
	// A fix is the console's fix-check, queued like it; the checks it reran come back in full.
	mux.HandleFunc("POST /doctor", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("fix")
		if id == "" {
			writeJSON(w, d.runDoctor(r.Context()))
			return
		}
		result, ok := d.runQueued(r.Context(), api.AgentCommand{Verb: "fix-check", Args: map[string]any{"id": id}})
		switch {
		case !ok:
		case !result.Ok:
			http.Error(w, result.Message, http.StatusBadRequest)
		default:
			d.mu.Lock()
			checks := d.checks
			d.mu.Unlock()
			writeJSON(w, checks)
		}
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
	return mux
}

// runQueued hands a CLI command to commandLoop, so it runs one at a time with the console's and is
// answered before the agent execs a new version. ok is false when the CLI went away first.
func (d *daemon) runQueued(ctx context.Context, c api.AgentCommand) (result api.CommandResult, ok bool) {
	answer := make(chan api.CommandResult, 1)
	select {
	case d.cmds <- []job{{c, func(result api.CommandResult) { answer <- result }}}:
	case <-ctx.Done():
		return result, false
	}
	select {
	case result = <-answer:
		return result, true
	case <-ctx.Done():
		return result, false
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
