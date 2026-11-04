package node

import (
	"encoding/json"
	"testing"
	"time"
)

const infoJSON = `{
  "node": "Addr", "state": "QUEUED", "status": "queueing-in-market-position",
  "details": {"node": "Addr", "market": "7xMarket", "position": 3, "count": 12},
  "since": "2026-11-04T10:00:05Z",
  "history": [
    {"status": "waiting-for-sol", "details": {}, "at": "2026-11-04T10:00:00Z"},
    {"status": "sol-funded", "details": {"sol": 0.01}, "at": "2026-11-04T10:00:03Z"},
    {"status": "queueing-in-market-success", "details": {"market": "7xMarket"}, "at": "2026-11-04T10:00:04Z"},
    {"status": "queueing-in-market-position", "details": {"position": 3}, "at": "2026-11-04T10:00:05Z"}
  ]
}`

func TestActivityAndEventsFromNodeInfo(t *testing.T) {
	var info Info
	if err := json.Unmarshal([]byte(infoJSON), &info); err != nil {
		t.Fatal(err)
	}
	a := info.Activity()
	if a == nil || a.Kind != "queue" || a.Detail != "position 3 of 12 in market 7xMarket" || !a.Since.Equal(time.Date(2026, 11, 4, 10, 0, 5, 0, time.UTC)) {
		t.Fatalf("activity: %+v", a)
	}
	if info.Prompt() != "" {
		t.Fatal("no prompt while queued")
	}

	// The first read primes the watermark without reporting history as news.
	events, at := info.Events(time.Time{})
	if len(events) != 0 || !at.Equal(time.Date(2026, 11, 4, 10, 0, 5, 0, time.UTC)) {
		t.Fatalf("first read: %d events, watermark %s", len(events), at)
	}
	events, _ = info.Events(time.Date(2026, 11, 4, 10, 0, 2, 0, time.UTC))
	if len(events) != 2 || events[0].Message != "the node wallet holds enough SOL; the node goes on" || events[1].Message != "queued in market 7xMarket" || events[1].Level != "info" {
		t.Fatalf("events: %+v", events)
	}

	info.State, info.Status, info.Details = "RESTARTING", "node-restarting", map[string]any{"delay": "60", "at": "2026-11-04T10:01:05Z"}
	if a = info.Activity(); a == nil || a.Kind != "restart" || !a.Since.Equal(time.Date(2026, 11, 4, 10, 1, 5, 0, time.UTC)) {
		t.Fatalf("restart: %+v", a)
	}
	info.State = "WAITING_FOR_REGISTRATION"
	if info.Prompt() != "registration" || info.Activity() != nil {
		t.Fatalf("prompt %q, activity %+v", info.Prompt(), info.Activity())
	}
}
