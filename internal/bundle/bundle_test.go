package bundle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nosana-ci/host-agent/internal/config"
)

func TestStaleFollowsConfigure(t *testing.T) {
	config.RuntimeDir = t.TempDir()
	dir := filepath.Join(config.RuntimeDir, "1.0.0")
	if !Stale("1.0.0") {
		t.Fatal("an unconfigured bundle is not stale")
	}
	if err := Reconfigure("1.0.0"); err != nil {
		t.Fatal(err)
	}
	if Stale("1.0.0") {
		t.Fatal("a freshly configured bundle is stale")
	}
	conf := filepath.Join(dir, "etc", "containers.conf")
	raw, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, append(raw, []byte("firewall_driver = \"other\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Stale("1.0.0") {
		t.Fatal("a changed containers.conf is not stale")
	}
	if err := Reconfigure("1.0.0"); err != nil {
		t.Fatal(err)
	}
	if Stale("1.0.0") {
		t.Fatal("reconfigure did not bring the bundle up to date")
	}
}
