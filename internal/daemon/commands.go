package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/doctor"
)

// Every command the console can send, in the order the agent advertises them. A new verb is one
// entry here; the console shows what the heartbeat lists.
var commands = []struct {
	verb string
	run  func(d *daemon, ctx context.Context, args map[string]any) (string, error)
}{
	{"restart-node", func(d *daemon, _ context.Context, _ map[string]any) (string, error) {
		return "node restarting", d.sup.Restart()
	}},
	{"stop-node", func(d *daemon, _ context.Context, _ map[string]any) (string, error) {
		return "node stopping; it stays stopped until start-node", d.sup.Stop()
	}},
	{"start-node", func(d *daemon, _ context.Context, _ map[string]any) (string, error) {
		return "node starting", d.sup.Start()
	}},
	{"run-doctor", func(d *daemon, ctx context.Context, _ map[string]any) (string, error) {
		return summary(d.runDoctor(ctx)), nil
	}},
	{"fix-check", func(d *daemon, ctx context.Context, args map[string]any) (string, error) {
		id, _ := args["id"].(string)
		if err := doctor.Fix(ctx, id); err != nil {
			return "", err
		}
		return "fixed " + id + "; " + summary(d.runDoctor(ctx)), nil
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

func (d *daemon) runCommands(ctx context.Context, cmds []api.AgentCommand) {
	for _, c := range cmds {
		msg, err := d.run(ctx, c)
		result := api.CommandResult{Ok: err == nil, Message: msg}
		if err != nil {
			result.Message = err.Error()
		}
		slog.Info("command", "verb", c.Verb, "ok", result.Ok, "message", result.Message)
		if err := d.ctrl.Report(ctx, c.Id, result); err != nil {
			slog.Warn("report command result", "id", c.Id, "err", err)
		}
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

func summary(checks []api.Check) string {
	var failing []string
	for _, c := range checks {
		if c.Status != api.Ok {
			failing = append(failing, c.Title)
		}
	}
	if len(failing) == 0 {
		return fmt.Sprintf("%d checks passed", len(checks))
	}
	return fmt.Sprintf("%d of %d checks need attention: %s", len(failing), len(checks), strings.Join(failing, ", "))
}
