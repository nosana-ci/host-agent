package store

import (
	"encoding/json"
	"errors"
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

func (u Update) IsBlocked(version string) bool {
	for _, b := range u.Blocked {
		if b == version {
			return true
		}
	}
	return false
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
	return s, json.Unmarshal(raw, &s.state)
}

func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Update changes the state and writes it.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	raw, _ := json.MarshalIndent(s.state, "", "  ")
	return WriteFile(s.path, append(raw, '\n'), 0o600)
}
