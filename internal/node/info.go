package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
)

// Info is the part of the node's /node/info the agent uses: the state category, the exact
// status with its details and since when, and the state changes the node went through.
type Info struct {
	Node    string         `json:"node"`
	State   string         `json:"state"`
	Status  string         `json:"status"`
	Details map[string]any `json:"details"`
	Since   time.Time      `json:"since"`
	History []struct {
		Status  string         `json:"status"`
		Details map[string]any `json:"details"`
		At      time.Time      `json:"at"`
	} `json:"history"`
}

var infoClient = &http.Client{Timeout: 3 * time.Second}

// readInfo asks the node's own API on its local port; nil when it does not answer.
func readInfo(ctx context.Context) *Info {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/node/info", apiPort), nil)
	if err != nil {
		return nil
	}
	resp, err := infoClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var info Info
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&info) != nil {
		return nil
	}
	return &info
}

// Activity is what the node does now, for the owner: a restart it counts down to (Since is when
// it fires), the job it runs, its benchmark, or its place in the queue.
func (i *Info) Activity() *api.NodeActivity {
	d := i.Details
	switch i.State {
	case "RESTARTING":
		if i.Status != "node-restarting" {
			return nil
		}
		at, err := time.Parse(time.RFC3339, str(d["at"]))
		if err != nil {
			at = i.Since
		}
		return &api.NodeActivity{Kind: "restart", Since: at}
	case "RUNNING_JOB":
		return &api.NodeActivity{Kind: "job", Detail: str(d["job"]), Since: i.Since}
	case "BENCHMARKING":
		return &api.NodeActivity{Kind: "benchmark", Detail: strings.TrimPrefix(i.Status, "benchmark-"), Since: i.Since}
	case "QUEUED":
		if pos := str(d["position"]); pos != "" {
			return &api.NodeActivity{Kind: "queue", Detail: fmt.Sprintf("position %s of %s in market %s", pos, str(d["count"]), str(d["market"])), Since: i.Since}
		}
	}
	return nil
}

// Opened reports whether the node opened its public tunnel (api-started) at or after since.
func (i *Info) Opened(since time.Time) bool {
	for _, h := range i.History {
		if h.Status == "api-started" && !h.At.Before(since) {
			return true
		}
	}
	return false
}

// Prompt is "registration" while the node waits at its sign-up questions, which nobody answers
// under the agent; the console offers the same sign-up instead.
func (i *Info) Prompt() string {
	if i.State == "WAITING_FOR_REGISTRATION" {
		return "registration"
	}
	return ""
}

// Event is a state change worth telling the owner, as an agent event of type node.
type Event struct {
	At      time.Time
	Level   string
	Message string
}

// events are the statuses the owner hears about, with their level and message; the details fill
// the %s placeholders in the order named.
var events = map[string]struct {
	level, format string
	keys          []string
}{
	"waiting-for-sol":            {"warn", "the node waits for SOL in its wallet", nil},
	"sol-funded":                 {"info", "the node wallet holds enough SOL; the node goes on", nil},
	"waiting-for-registration":   {"warn", "the node waits for its registration (email and the Participation Agreement)", nil},
	"registered":                 {"info", "the node is registered", nil},
	"registration-failed":        {"error", "the registration failed: %s", []string{"error"}},
	"node-starting-failed":       {"error", "the node could not start: %s", []string{"error"}},
	"benchmark-running":          {"info", "benchmark started", nil},
	"benchmark-passed":           {"info", "benchmark passed", nil},
	"benchmark-failed":           {"warn", "benchmark failed", nil},
	"benchmark-error":            {"error", "benchmark failed: %s", []string{"error"}},
	"queueing-in-market-success": {"info", "queued in market %s", []string{"market"}},
	"queueing-in-market-failed":  {"warn", "could not join market %s", []string{"market"}},
	"claiming-job-success":       {"info", "job %s claimed", []string{"job"}},
	"claiming-job-failed":        {"error", "job %s could not be claimed", []string{"job"}},
	"job-starting-failed":        {"error", "job %s failed to start", []string{"job"}},
	"job-running-success":        {"info", "job %s finished", []string{"job"}},
	"job-running-failed":         {"error", "job %s failed", []string{"job"}},
	"node-restarting":            {"info", "the node restarts itself in %s s", []string{"delay"}},
}

// Events returns the state changes after the watermark that the owner hears about, oldest first,
// and the new watermark. The first read primes it: what happened before the agent looked is
// history, not news.
func (i *Info) Events(after time.Time) ([]Event, time.Time) {
	var out []Event
	last := after
	for _, h := range i.History {
		if !h.At.After(after) {
			continue
		}
		if h.At.After(last) {
			last = h.At
		}
		if after.IsZero() {
			continue
		}
		e, ok := events[h.Status]
		if !ok {
			continue
		}
		args := make([]any, len(e.keys))
		for n, k := range e.keys {
			args[n] = str(h.Details[k])
		}
		out = append(out, Event{At: h.At, Level: e.level, Message: fmt.Sprintf(e.format, args...)})
	}
	return out, last
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprintf("%v", x)
	}
	return fmt.Sprint(v)
}
