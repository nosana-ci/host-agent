package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// Event is something the agent did by itself or for its CLI: an automatic update, a rollback, a
// node crash, a command typed on the machine. It waits in the state until host-manager took it,
// so the events around an agent switch survive the exec.
type Event struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Type    string    `json:"type"`  // agent, update, runtime, node or command
	Level   string    `json:"level"` // info, warn or error
	Message string    `json:"message"`
	Verb    string    `json:"verb,omitempty"` // the command, for type command
}

const (
	// maxEvents is how many unsent events are kept; the oldest go first.
	maxEvents = 200
	// maxMessage is host-manager's limit on an event's message, in characters.
	maxMessage = 1000
)

func (s *Store) Info(typ, format string, args ...any)  { s.Record(typ, "info", "", format, args...) }
func (s *Store) Warn(typ, format string, args ...any)  { s.Record(typ, "warn", "", format, args...) }
func (s *Store) Error(typ, format string, args ...any) { s.Record(typ, "error", "", format, args...) }

// Record keeps an event for host-manager. A nil store (in tests) records nothing.
func (s *Store) Record(typ, level, verb, format string, args ...any) {
	if s == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if r := []rune(msg); len(r) > maxMessage {
		msg = string(r[:maxMessage-1]) + "…"
	}
	e := Event{ID: newID(), At: time.Now().UTC(), Type: typ, Level: level, Message: msg, Verb: verb}
	if err := s.Update(func(st *State) {
		st.Events = append(st.Events, e)
		if n := len(st.Events) - maxEvents; n > 0 {
			st.Events = st.Events[n:]
		}
	}); err != nil {
		slog.Warn("event not kept", "message", msg, "err", err)
		return
	}
	select {
	case s.added <- struct{}{}:
	default:
	}
}

// EventsAdded fires after an event was recorded.
func (s *Store) EventsAdded() <-chan struct{} { return s.added }

// Delivered drops the events host-manager took.
func (s *Store) Delivered(ids []string) error {
	sent := make(map[string]bool, len(ids))
	for _, id := range ids {
		sent[id] = true
	}
	return s.Update(func(st *State) {
		kept := st.Events[:0]
		for _, e := range st.Events {
			if !sent[e.ID] {
				kept = append(kept, e)
			}
		}
		st.Events = kept
	})
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
