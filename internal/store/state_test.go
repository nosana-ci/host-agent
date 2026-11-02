package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFailedWriteKeepsTheOldState(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(filepath.Join(dir, "state.json"))
	if err := st.Update(func(s *State) { s.Update.Pending = &Pending{Version: "1.0.0"} }); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o500)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := st.Update(func(s *State) { s.Update.Pending.Attempts++; s.Claimed = true }); err == nil {
		t.Skip("the directory is writable anyway (running as root?)")
	}
	if s := st.State(); s.Claimed || s.Update.Pending.Attempts != 0 {
		t.Fatalf("state changed without being written: %+v", s)
	}
}
