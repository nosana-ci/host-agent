package updater

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

// maxStarts is how often a pending version may start without staying up before it is rolled back.
const maxStarts = 3

// Preflight runs first thing in a new process: it counts the start of a pending version and
// rolls back once it has failed too often. It may exec the previous binary and not return.
func Preflight(st *store.Store) {
	p := st.State().Update.Pending
	if p == nil || p.Version != version.Version {
		return
	}
	if p.Attempts+1 > maxStarts {
		rollback(st, fmt.Sprintf("version %s failed to start %d times", p.Version, maxStarts))
	}
	_ = st.Update(func(s *store.State) { s.Update.Pending.Attempts++ })
}

// rollback points bin/current back at the previous binary, blocks the failed version and execs
// the previous binary in place.
func rollback(st *store.Store, reason string) {
	p := st.State().Update.Pending
	slog.Warn("rolling back", "to", p.Previous, "reason", reason)
	if err := store.Relink(config.CurrentBinary, p.Previous); err != nil {
		slog.Error("rollback failed", "err", err)
		return
	}
	_ = st.Update(func(s *store.State) {
		s.Update.Blocked = append(s.Update.Blocked, p.Version)
		s.Update.LastRollback = fmt.Sprintf("%s: back on %s", reason, binVersion(p.Previous))
		s.Update.Pending = nil
	})
	execSelf()
}

// execSelf replaces this process with bin/current; the node keeps running.
func execSelf() {
	if err := syscall.Exec(config.CurrentBinary, os.Args, os.Environ()); err != nil {
		slog.Error("exec failed", "err", err)
		os.Exit(1)
	}
}

// pruneBinaries deletes agent binaries other than the current and previous ones.
func pruneBinaries(previous string) {
	current, _ := filepath.EvalSymlinks(config.CurrentBinary)
	entries, _ := os.ReadDir(config.BinDir)
	for _, e := range entries {
		p := filepath.Join(config.BinDir, e.Name())
		if strings.HasPrefix(e.Name(), "nosana-host-") && p != current && p != previous {
			_ = os.Remove(p)
		}
	}
}

func binVersion(path string) string { return strings.TrimPrefix(filepath.Base(path), "nosana-host-") }
