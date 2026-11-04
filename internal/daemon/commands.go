package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/doctor"
)

// Every command the console can send, in the order the agent advertises them. The CLI sends the
// same verbs over the socket. A new verb is one entry here; the console shows what the heartbeat
// lists.
var commands = []struct {
	verb string
	run  func(d *daemon, ctx context.Context, args map[string]any) (string, error)
}{
	{"restart-node", func(d *daemon, _ context.Context, _ map[string]any) (string, error) {
		return "node restarting", d.sup.Restart()
	}},
	{"stop-node", func(d *daemon, _ context.Context, _ map[string]any) (string, error) {
		return "node stopping; it stays stopped until it is started again", d.sup.Stop()
	}},
	{"start-node", func(d *daemon, _ context.Context, _ map[string]any) (string, error) {
		return "node starting", d.sup.Start()
	}},
	{"run-doctor", func(d *daemon, ctx context.Context, _ map[string]any) (string, error) {
		return doctor.Summary(d.runDoctor(ctx)), nil
	}},
	{"fix-check", func(d *daemon, ctx context.Context, args map[string]any) (string, error) {
		id, _ := args["id"].(string)
		if err := doctor.Fix(ctx, id); err != nil {
			return "", err
		}
		return "fixed " + id + "; " + doctor.Summary(d.runDoctor(ctx)), nil
	}},
	{"update", func(d *daemon, ctx context.Context, _ map[string]any) (string, error) {

		return d.upd.Check(ctx, true)
	}},
}

func verbs() []string {
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		out = append(out, c.verb)
	}
	return out
}

// job is a command and where its answer goes: host-manager for the console's, the socket for the
// CLI's.
type job struct {
	api.AgentCommand
	answer func(api.CommandResult)
}

func (d *daemon) runJobs(ctx context.Context, jobs []job) {
	for _, j := range jobs {
		msg, err := d.run(ctx, j.AgentCommand)
		result := api.CommandResult{Ok: err == nil, Message: msg}
		if err != nil {
			result.Message = err.Error()
		}
		slog.Info("command", "verb", j.Verb, "ok", result.Ok, "message", result.Message)
		j.answer(result)
	}
}

func (d *daemon) run(ctx context.Context, c api.AgentCommand) (string, error) {
	for _, cmd := range commands {
		if cmd.verb == c.Verb {
			return cmd.run(d, ctx, c.Args)
		}
	}
	return "", fmt.Errorf("unknown command %q", c.Verb)
}
