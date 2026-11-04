// Package nodelog reads the nosana-node log for the owner: what the node does right now (a
// benchmark, its place in a market queue, a job, a countdown to a restart), whether it waits at
// its sign-up prompt, and the lines worth an event. The patterns follow what @nosana/node prints
// without a TTY: ora prefixes "- ", "✔ " or "✖ ", and the colours are stripped first.
package nodelog

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
)

// Event is what one log line means for the host owner, as an agent event of type node.
type Event struct {
	Level   string // info | warn | error
	Message string
}

// Line is one log line with the time podman recorded it.
type Line struct {
	At   time.Time
	Text string
}

type pattern struct {
	re    *regexp.Regexp
	level string
	msg   func(m []string) string
}

func text(s string) func([]string) string { return func([]string) string { return s } }

// The benchmark's own lines: ora writes "- COLLECTING METRICS  Ops: 0/0" once when it starts and
// "✔ COLLECTING METRICS Completed" when its operations are done.
var (
	benchStarted   = regexp.MustCompile(`^- +COLLECTING METRICS`)
	benchCompleted = regexp.MustCompile(`^✔ +COLLECTING METRICS +Completed`)
	queuedRe       = regexp.MustCompile(`QUEUED +In market (\S+) at position (\d+)/(\d+)`)
	restartRe      = regexp.MustCompile(`RESTARTING +In (\d+) seconds?`)
	jobBeginRe     = regexp.MustCompile(`^(?:✔ Node has claimed job (\S+)|Job (\S+) started successfully)`)
	jobEndRe       = regexp.MustCompile(`^(?:Job \S+ finished successfully|(?:✖ )?Error (?:claiming|starting|finishing) job|Error occur+ed running job|Job \S+ is already expired)`)
	// Failed threshold lines inside the feedback report ("  ✖ metric.key   value"); they are
	// folded into the failed-benchmark event as its reason.
	failedMetric = regexp.MustCompile(`^  ✖ (\S+)\s*(.*)$`)
)

// patterns is the whole table, one row per node line the agent reports on.
var patterns = []pattern{
	{benchStarted, "info", text("benchmark started: collecting metrics")},
	{benchCompleted, "info", text("benchmark run completed; results sent to host-manager")},
	{regexp.MustCompile(`^\s*Node currently meets all (\d+) market requirements for (.+?)\s*$`), "info",
		func(m []string) string {
			return fmt.Sprintf("benchmark passed: meets all %s requirements for %s", m[1], m[2])
		}},
	{regexp.MustCompile(`^\s*Node currently meets (\d+) of (\d+) market requirements for (.+?) — (\d+) still need improvement\s*$`), "warn",
		func(m []string) string {
			return fmt.Sprintf("benchmark failed: %s of %s requirements for %s met, %s still need improvement", m[1], m[2], m[3], m[4])
		}},
	{regexp.MustCompile(`^Node does not qualify for a market yet\. Next eligible retest at (\S+), retrying in ~(\d+) minutes`), "warn",
		func(m []string) string {
			return fmt.Sprintf("no market qualifies yet; next retest at %s (in about %s min)", m[1], m[2])
		}},
	{regexp.MustCompile(`Node does not meet the minimum requirements for any market\.(?: Next eligible retest at (\S+)\.)?`), "error",
		func(m []string) string {
			if m[1] != "" {
				return "benchmark failed: no market accepts this node; next retest at " + m[1]
			}
			return "benchmark failed: no market accepts this node"
		}},
	{regexp.MustCompile(`^Error executing benchmark: ?(.*)$`), "error", func(m []string) string { return strings.TrimSpace("benchmark failed: " + m[1]) }},
	{regexp.MustCompile(`^Grid recommended (\S+) market to node successfully`), "info", func(m []string) string { return "host-manager assigned market " + m[1] }},
	{regexp.MustCompile(`^Some thresholds are still below the target market requirements, but access was granted`), "info",
		text("market access granted although some thresholds are below the target")},
	{regexp.MustCompile(`Could not join market`), "warn", text("could not join the market")},
	{regexp.MustCompile(`could not join queue: (.*)$`), "warn", func(m []string) string { return "could not join the market queue: " + m[1] }},
	{queuedRe, "info", func(m []string) string {
		return fmt.Sprintf("queued in market %s at position %s of %s", m[1], m[2], m[3])
	}},
	{regexp.MustCompile(`^✔ Node has claimed job (\S+)`), "info", func(m []string) string { return "job " + m[1] + " claimed" }},
	{regexp.MustCompile(`^(?:✖ )?Error claiming job (\S+)`), "error", func(m []string) string { return "job " + m[1] + " could not be claimed" }},
	{regexp.MustCompile(`^Job (\S+) started successfully`), "info", func(m []string) string { return "job " + m[1] + " started" }},
	{regexp.MustCompile(`^Error starting job (\S+)`), "error", func(m []string) string { return "job " + m[1] + " failed to start" }},
	{regexp.MustCompile(`^Job (\S+) finished successfully`), "info", func(m []string) string { return "job " + m[1] + " finished" }},
	{regexp.MustCompile(`^Error finishing job (\S+)`), "error", func(m []string) string { return "job " + m[1] + " failed to finish" }},
	{regexp.MustCompile(`^Error occur+ed running job (\S+)`), "error", func(m []string) string { return "job " + m[1] + " failed while running" }},
	{regexp.MustCompile(`^Job (\S+) is already expired`), "warn", func(m []string) string { return "job " + m[1] + " had already expired" }},
}

// The node asks these on its terminal the first time it meets host-manager. Under the agent
// nobody answers, so it would wait forever; the console offers the same sign-up instead.
var registrationPrompts = []string{"Your Email Address", "Discord username:", "What is your Twitter username?", "Participation Agreement"}

const (
	remember = 400
	// restartLinger is how long past its time a countdown is still shown; by then the node has
	// restarted or printed something new.
	restartLinger = 30 * time.Second
	nodeStart     = "Starting Nosana Node"
)

// Tracker feeds a tail that is read again and again (the same lines come back until they scroll
// out) and reports each new line's event once. The first tail primes it: what the node printed
// before the agent started is history, not news, but it still says what the node does now.
type Tracker struct {
	seen   map[string]struct{}
	order  []string
	primed bool

	failed  []string // the failed thresholds of the report being printed
	bench   *api.NodeActivity
	queue   *api.NodeActivity
	job     *api.NodeActivity
	restart *api.NodeActivity
	inError bool   // between an ERROR block's header and its closing line
	reason  string // the Message of the newest ERROR block, until a countdown takes it
	prompt  bool   // the current run stopped at the sign-up questions
}

// Feed takes the newest tail, oldest line first, and returns the events of the lines it had not
// seen before.
func (t *Tracker) Feed(lines []Line) []Event {
	if t.seen == nil {
		t.seen = map[string]struct{}{}
	}
	var out []Event
	for _, l := range lines {
		key := l.At.Format(time.RFC3339Nano) + "|" + l.Text
		if _, ok := t.seen[key]; ok {
			continue
		}
		t.seen[key] = struct{}{}
		t.order = append(t.order, key)
		if len(t.order) > remember {
			delete(t.seen, t.order[0])
			t.order = t.order[1:]
		}
		t.follow(l)
		if e, ok := t.parse(l.Text); ok && t.primed {
			out = append(out, e)
		}
	}
	t.primed = true
	return out
}

// parse returns the event for one line, if it is one.
func (t *Tracker) parse(line string) (Event, bool) {
	if strings.Contains(line, "TARGET MARKET") {
		t.failed = nil
		return Event{}, false
	}
	if m := failedMetric.FindStringSubmatch(line); m != nil {
		reason := m[1]
		if v := strings.TrimSpace(m[2]); v != "" {
			reason += " (" + v + ")"
		}
		t.failed = append(t.failed, reason)
		return Event{}, false
	}
	for _, pt := range patterns {
		m := pt.re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		e := Event{Level: pt.level, Message: pt.msg(m)}
		if strings.HasPrefix(e.Message, "benchmark failed") && len(t.failed) > 0 {
			e.Message += "; below target: " + strings.Join(t.failed, ", ")
			t.failed = nil
		}
		return e, true
	}
	return Event{}, false
}

// follow keeps what the node does now up to date from one line.
func (t *Tracker) follow(l Line) {
	text := strings.TrimSpace(l.Text)
	if strings.Contains(text, nodeStart) {
		t.bench, t.queue, t.job, t.restart, t.reason, t.inError, t.prompt = nil, nil, nil, nil, "", false, false
		return
	}
	for _, p := range registrationPrompts {
		if strings.Contains(text, p) {
			t.prompt = true
		}
	}
	switch {
	case benchStarted.MatchString(text):
		t.bench = &api.NodeActivity{Kind: "benchmark", Detail: "collecting metrics", Since: l.At}
	case t.bench != nil && benchCompleted.MatchString(text):
		t.bench = &api.NodeActivity{Kind: "benchmark", Detail: "submitting results", Since: l.At}
	case t.bench != nil && strings.Contains(text, "TARGET MARKET"):
		t.bench = &api.NodeActivity{Kind: "benchmark", Detail: "checking market requirements", Since: l.At}
	case t.bench != nil && t.endsBenchmark(text):
		t.bench = nil
	}
	if m := queuedRe.FindStringSubmatch(text); m != nil {
		detail := fmt.Sprintf("position %s of %s in market %s", m[2], m[3], m[1])
		if t.queue == nil || t.queue.Detail != detail {
			t.queue = &api.NodeActivity{Kind: "queue", Detail: detail, Since: l.At}
		}
		t.job = nil
	}
	switch {
	case jobEndRe.MatchString(text):
		t.job = nil
	case jobBeginRe.MatchString(text):
		m := jobBeginRe.FindStringSubmatch(text)
		t.job, t.queue = &api.NodeActivity{Kind: "job", Detail: m[1] + m[2], Since: l.At}, nil
	}
	switch {
	case strings.Contains(text, "Node has restarted successfully"):
		t.restart, t.reason, t.inError = nil, "", false
	case strings.Contains(text, "========== ERROR =========="):
		t.inError, t.reason = true, ""
	case t.inError && strings.HasPrefix(text, "Message:"):
		t.reason = strings.TrimSpace(strings.TrimPrefix(text, "Message:"))
	case t.inError && strings.HasPrefix(text, "====="):
		t.inError = false
	case restartRe.MatchString(text) && !l.At.IsZero():
		sec, _ := strconv.Atoi(restartRe.FindStringSubmatch(text)[1])
		reason := t.reason
		if t.restart != nil && reason == "" {
			reason = strings.TrimPrefix(t.restart.Detail, "restart: ") // a later tick of the same countdown
		}
		at := l.At.Add(time.Duration(sec) * time.Second)
		detail := ""
		if reason != "" {
			detail = "restart: " + reason
		}
		t.restart, t.reason = &api.NodeActivity{Kind: "restart", Detail: detail, Since: at}, ""
	}
}

// endsBenchmark is true for every event line other than the benchmark's own start and
// completion: a verdict, an error, or the node moving on to the market, the queue or a job.
func (t *Tracker) endsBenchmark(line string) bool {
	for _, pt := range patterns {
		if pt.re != benchStarted && pt.re != benchCompleted && pt.re.MatchString(line) {
			return true
		}
	}
	return false
}

// Activity is what the node does now, the most pressing first: a restart it counts down to (its
// Since is when it fires), the job it runs, the benchmark it runs, or its place in the queue.
func (t *Tracker) Activity(now time.Time) *api.NodeActivity {
	var a *api.NodeActivity
	switch {
	case t.restart != nil && now.Before(t.restart.Since.Add(restartLinger)):
		a = t.restart
	case t.job != nil:
		a = t.job
	case t.bench != nil:
		a = t.bench
	case t.queue != nil:
		a = t.queue
	default:
		return nil
	}
	c := *a
	return &c
}

// Prompt reports "registration" when the node's current run stopped at the sign-up questions.
func (t *Tracker) Prompt() string {
	if t.prompt {
		return "registration"
	}
	return ""
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// Lines splits a timestamped podman log ("2026-11-04T10:00:00.123456789Z text" per line) into
// lines: colours stripped, and of a line redrawn with carriage returns only the last drawing.
func Lines(raw []byte) []Line {
	var out []Line
	for _, s := range strings.Split(string(raw), "\n") {
		if i := strings.LastIndexByte(s, '\r'); i >= 0 {
			s = s[:prefixEnd(s)] + s[i+1:]
		}
		s = ansi.ReplaceAllString(s, "")
		if strings.TrimSpace(s) == "" {
			continue
		}
		ts, rest, ok := strings.Cut(s, " ")
		if !ok {
			out = append(out, Line{Text: s})
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			out = append(out, Line{Text: s})
			continue
		}
		out = append(out, Line{At: at.UTC(), Text: rest})
	}
	return out
}

// prefixEnd is where the timestamp and its space end, so a carriage return keeps them.
func prefixEnd(s string) int {
	if i := strings.IndexByte(s, ' '); i >= 0 && i < len(s) {
		if _, err := time.Parse(time.RFC3339Nano, s[:i]); err == nil {
			return i + 1
		}
	}
	return 0
}
