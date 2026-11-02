package store

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
)

// State is what the agent remembers between runs, in agent/state.json.
type State struct {
	Claimed     bool   `json:"claimed"`
	NodeStopped bool   `json:"nodeStopped"`
	NodeImage   string `json:"nodeImage,omitempty"` // from the release manifest
	Update      Update `json:"update"`
}

// Update is the self-update bookkeeping: the version that must still prove itself, the ones
// that failed here, and the runtime bundles that failed here.
type Update struct {
	Pending        *Pending `json:"pending,omitempty"`
	Blocked        []string `json:"blocked,omitempty"`
	BlockedRuntime []string `json:"blockedRuntime,omitempty"`
	LastRollback   string   `json:"lastRollback,omitempty"`
}

// Pending is an agent version that was switched to but has not stayed up long enough yet.
type Pending struct {
	Version  string `json:"version"`
	Previous string `json:"previous"` // the binary to go back to
	Attempts int    `json:"attempts"` // starts counted by Preflight
}

// Store is the state file with a lock around it.
type Store struct {
	path  string
	mu    sync.Mutex
	state State
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		// Starting over beats a restart loop in which Preflight never runs.
		_ = os.Rename(path, path+".corrupt")
		s.state = State{}
		slog.Warn("state file unreadable; starting over", "err", err, "kept", path+".corrupt")
	}
	return s, nil
}

func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Update applies fn to a copy of the state and keeps the copy once it is written.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next State
	raw, _ := json.Marshal(s.state)
	_ = json.Unmarshal(raw, &next) // a deep copy: fn may change Pending in place
	fn(&next)
	raw, _ = json.MarshalIndent(next, "", "  ")
	if err := WriteFile(s.path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	s.state = next
	return nil
}
