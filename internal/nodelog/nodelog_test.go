package nodelog

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

func lines(texts ...string) []Line { return linesFrom(t0, texts...) }

func linesFrom(start time.Time, texts ...string) []Line {
	out := make([]Line, len(texts))
	for i, s := range texts {
		out[i] = Line{At: start.Add(time.Duration(i) * time.Second), Text: s}
	}
	return out
}

func TestFirstTailIsHistoryButSaysWhatTheNodeDoes(t *testing.T) {
	var tr Tracker
	events := tr.Feed(lines("Starting Nosana Node", "- COLLECTING METRICS  Ops: 0/0"))
	if len(events) != 0 {
		t.Fatalf("the first tail should not be news: %+v", events)
	}
	a := tr.Activity(t0)
	if a == nil || a.Kind != "benchmark" || a.Detail != "collecting metrics" {
		t.Fatalf("activity after the first tail: %+v", a)
	}
	events = tr.Feed(lines("Starting Nosana Node", "- COLLECTING METRICS  Ops: 0/0", "✔ COLLECTING METRICS  Completed"))
	if len(events) != 1 || events[0].Message != "benchmark run completed; results sent to host-manager" {
		t.Fatalf("events: %+v", events)
	}
	if a := tr.Activity(t0); a.Detail != "submitting results" {
		t.Fatalf("activity: %+v", a)
	}
}

func TestQueueJobAndVerdict(t *testing.T) {
	var tr Tracker
	tr.Feed(lines("Starting Nosana Node"))
	tr.Feed(lines("Starting Nosana Node", "-  QUEUED  In market 7xMarket at position 3/12"))
	if a := tr.Activity(t0); a == nil || a.Kind != "queue" || a.Detail != "position 3 of 12 in market 7xMarket" {
		t.Fatalf("queued: %+v", a)
	}
	events := tr.Feed(lines("Starting Nosana Node", "-  QUEUED  In market 7xMarket at position 3/12", "✔ Node has claimed job JobAddr1"))
	if a := tr.Activity(t0); a == nil || a.Kind != "job" || a.Detail != "JobAddr1" {
		t.Fatalf("job: %+v", a)
	}
	if len(events) != 1 || events[0].Message != "job JobAddr1 claimed" {
		t.Fatalf("events: %+v", events)
	}
	tr.Feed(lines("Job JobAddr1 finished successfully"))
	if a := tr.Activity(t0); a != nil {
		t.Fatalf("after the job: %+v", a)
	}
}

func TestFailedBenchmarkCarriesItsReasons(t *testing.T) {
	var tr Tracker
	tr.Feed(lines("Starting Nosana Node"))
	events := tr.Feed(lines("Starting Nosana Node", "TARGET MARKET  nvidia-3090", "  ✖ gpu.vram   20 GB", "Node currently meets 4 of 5 market requirements for nvidia-3090 — 1 still need improvement"))
	if len(events) != 1 || events[0].Level != "warn" || events[0].Message != "benchmark failed: 4 of 5 requirements for nvidia-3090 met, 1 still need improvement; below target: gpu.vram (20 GB)" {
		t.Fatalf("events: %+v", events)
	}
}

func TestRestartCountdownWithItsReasonAndThePrompt(t *testing.T) {
	var tr Tracker
	tr.Feed(lines("Starting Nosana Node", "========== ERROR ==========", "Message: Node API is detected offline", "============================", "-  RESTARTING  In 60 seconds"))
	a := tr.Activity(t0)
	if a == nil || a.Kind != "restart" || a.Detail != "restart: Node API is detected offline" || !a.Since.Equal(t0.Add(64*time.Second)) {
		t.Fatalf("restart: %+v", a)
	}
	if tr.Activity(t0.Add(2*time.Minute)) != nil {
		t.Fatal("a countdown long past should be gone")
	}
	tr.Feed(linesFrom(t0.Add(time.Hour), "Starting Nosana Node", "Your Email Address:")) // a new run
	if tr.Prompt() != "registration" || tr.Activity(t0) != nil {
		t.Fatalf("prompt %q, activity %+v", tr.Prompt(), tr.Activity(t0))
	}
}

func TestLinesStripColoursAndKeepTheLastDrawing(t *testing.T) {
	raw := []byte("2026-11-04T10:00:00.5Z \x1b[32m✔\x1b[39m done\n2026-11-04T10:00:01Z - working\r- worked\n\nno timestamp here\n")
	got := Lines(raw)
	if len(got) != 3 || got[0].Text != "✔ done" || !got[0].At.Equal(t0.Add(500*time.Millisecond)) || got[1].Text != "- worked" || got[2].Text != "no timestamp here" || !got[2].At.IsZero() {
		t.Fatalf("got %+v", got)
	}
}
