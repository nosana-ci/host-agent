package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/control"
	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
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
			Claimed: s.Claimed, ClaimPending: s.ClaimToken != "", LastRollback: s.Update.LastRollback, Status: d.status(r.Context()),
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
			checks := d.runDoctor(r.Context())
			d.recordCLI("run-doctor", api.CommandResult{Ok: true, Message: doctor.Summary(checks)})
			writeJSON(w, checks)
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
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, d.configView())
	})
	// The node's registration as host-manager has it, for the CLI to show and edit.
	mux.HandleFunc("GET /registration", func(w http.ResponseWriter, r *http.Request) {
		reg, err := d.ctrl.Registration(r.Context())
		if err != nil {
			http.Error(w, "registration: "+linkedErr(err).Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, api.AgentRegistration{Registration: reg})
	})
	// The node's registration from the CLI, which had the owner accept the Participation Agreement.
	// It does not touch the node, so it skips the command queue.
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		reg := api.Registration{Email: strings.TrimSpace(q.Get("email")), Discord: optional(q.Get("discord")), Twitter: optional(q.Get("twitter"))}
		switch {
		case q.Get("accept") != "1":
			http.Error(w, "the Participation Agreement has to be accepted first", http.StatusBadRequest)
		case !emailRe.MatchString(reg.Email):
			http.Error(w, fmt.Sprintf("%q is not an email address", reg.Email), http.StatusBadRequest)
		default:
			if err := d.ctrl.Register(r.Context(), reg); err != nil {
				err = linkedErr(err)
				d.recordCLI("register", api.CommandResult{Message: "register: " + err.Error()})
				http.Error(w, "register: "+err.Error(), http.StatusBadGateway)
				return
			}
			slog.Info("registered the node", "email", reg.Email)
			result := api.CommandResult{Ok: true, Message: "registered the node as " + reg.Email}
			d.recordCLI("register", result)
			writeJSON(w, result)
		}
	})
	mux.HandleFunc("GET /logs/{what}", func(w http.ResponseWriter, r *http.Request) {
		d.logs(w, r, r.PathValue("what"), "")
	})
	mux.HandleFunc("GET /logs/job/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.logs(w, r, "job", r.PathValue("id"))
	})
	return mux
}

// logs streams one log: the node container, the agent's or podman's journal, or a job's main
// container, which the node names "<job id>-0" after the operation index.
func (d *daemon) logs(w http.ResponseWriter, r *http.Request, what, jobID string) {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail")) // 0 or missing: everything
	follow := r.URL.Query().Get("follow") == "true"
	out := flushWriter{w}
	var err error
	switch what {
	case "node":
		err = d.pc.Logs(r.Context(), node.ContainerName, tail, follow, out)
	case "job":
		err = d.pc.Logs(r.Context(), jobID+"-0", tail, follow, out)
	case "agent":
		err = service.Journal(r.Context(), service.AgentUnit, tail, follow, out)
	case "podman":
		err = service.Journal(r.Context(), service.PodmanUnit, tail, follow, out)
	default:
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, podman.ErrNotFound) {
		http.Error(w, "no such container; a job's log is kept only while its container exists", http.StatusNotFound)
		return
	}
	if err != nil && r.Context().Err() == nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}

// linkedErr explains host-manager not knowing the host: the registration goes through the link.
func linkedErr(err error) error {
	if errors.Is(err, control.ErrUnknownHost) {
		return errors.New("this host is not linked to an account yet; run the install command from the console first")
	}
	return err
}

func optional(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

// runQueued hands a CLI command to commandLoop, so it runs one at a time with the console's and is
// answered before the agent execs a new version. ok is false when the CLI went away first.
func (d *daemon) runQueued(ctx context.Context, c api.AgentCommand) (result api.CommandResult, ok bool) {
	answer := make(chan api.CommandResult, 1)
	select {
	case d.cmds <- []job{{AgentCommand: c, answer: func(result api.CommandResult) { answer <- result }, cli: true}}:
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

// emailRe is the node's own rule for an email address.
var emailRe = regexp.MustCompile(`\S+@\S+\.\S+`)

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
