package legacy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nosana-ci/host-agent/internal/keys"
)

// nodeDir makes a node directory with a new key and returns it with the key's address.
func nodeDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	key, err := keys.Wallet(filepath.Join(dir, "nosana_key.json"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, keys.Address(key)
}

// fake answers docker ps with the containers ("name image") and docker inspect with the node
// directory each one mounts; other calls succeed, or fail as fail says.
func fake(t *testing.T, containers map[string]string, mounts map[string]string, fail func(args []string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	old := docker
	docker = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "ps":
			var lines []string
			for name, image := range containers {
				lines = append(lines, name+" "+image)
			}
			return []byte(strings.Join(lines, "\n") + "\n"), nil
		case "inspect":
			return []byte(mounts[args[len(args)-1]] + "\n"), nil
		}
		if fail != nil {
			out, err := fail(args)
			return []byte(out), err
		}
		return nil, nil
	}
	t.Cleanup(func() { docker = old })
	return &calls
}

func TestRuns(t *testing.T) {
	dir, address := nodeDir(t)
	other, _ := nodeDir(t)
	for _, tc := range []struct {
		containers, mounts map[string]string
		want               string
	}{
		{map[string]string{"podman": "nosana/podman:v1.1.2"}, map[string]string{"podman": dir}, "podman"},
		{map[string]string{"my-node": "docker.io/nosana/nosana-node:latest"}, map[string]string{"my-node": dir}, "my-node"},
		{map[string]string{"second": "nosana/nosana-node:latest"}, map[string]string{"second": other}, ""}, // another key
		{map[string]string{"podman": "quay.io/podman/stable"}, map[string]string{"podman": dir}, ""},       // someone else's podman
		{map[string]string{"frpc-api-X": "nosana/frpc:v1.0.45"}, nil, ""},
	} {
		fake(t, tc.containers, tc.mounts, nil)
		got := strings.Join(Running(context.Background(), address), ",")
		if got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.containers, got, tc.want)
		}
	}
	old := docker
	docker = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("docker: not found") }
	defer func() { docker = old }()
	if Running(context.Background(), address) != nil {
		t.Error("an old node runs on a host without docker")
	}
}

func TestFind(t *testing.T) {
	dir, address := nodeDir(t)
	fake(t, map[string]string{"podman": "nosana/podman:v1.1.2", "web": "nginx"}, map[string]string{"podman": dir}, nil)
	got := Find(context.Background())
	if len(got) != 1 || got[0].Address != address || got[0].KeyPath != filepath.Join(dir, "nosana_key.json") {
		t.Fatalf("got %+v", got)
	}
}

func TestStop(t *testing.T) {
	dir, address := nodeDir(t)
	calls := fake(t, map[string]string{"podman": "nosana/podman:v1.1.2"}, map[string]string{"podman": dir}, nil)
	if err := Stop(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	var stops []string
	for _, c := range *calls {
		if strings.HasPrefix(c, "exec") || strings.HasPrefix(c, "stop") {
			stops = append(stops, c)
		}
	}
	if len(stops) != 2 || !strings.HasPrefix(stops[0], "exec podman podman stop") || stops[1] != "stop -t 10 podman" {
		t.Fatalf("the node must stop before its container: %q", stops)
	}
}

func TestStopWhenStartShsNodeAlreadyStopped(t *testing.T) {
	dir, address := nodeDir(t)
	fake(t, map[string]string{"podman": "nosana/podman:v1.1.2"}, map[string]string{"podman": dir}, func(args []string) (string, error) {
		if args[0] == "exec" {
			return "Error: container abc is not running", errors.New("exit 1")
		}
		return "", nil
	})
	if err := Stop(context.Background(), address); err != nil {
		t.Fatal(err)
	}
}

func TestStopGivesANodeContainerTimeToLeave(t *testing.T) {
	dir, address := nodeDir(t)
	calls := fake(t, map[string]string{"my-node": "nosana/nosana-node:latest"}, map[string]string{"my-node": dir}, nil)
	if err := Stop(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	n := len(*calls)
	if (*calls)[n-2] != "update --restart=no my-node" || (*calls)[n-1] != "stop -t 120 my-node" {
		t.Fatalf("docker must not restart the node, and the node gets time to leave: %q", *calls)
	}
}

func TestDrain(t *testing.T) {
	dir, address := nodeDir(t)
	fake(t, map[string]string{"podman": "nosana/podman:v1.1.2"}, map[string]string{"podman": dir}, nil)
	ctx := context.Background()
	if err := Drain(ctx, address); err != nil {
		t.Fatal(err)
	}
	if Drained(ctx, address) {
		t.Fatal("drained while the request is still there")
	}
	if err := os.Remove(filepath.Join(dir, "drain")); err != nil { // the node took it
		t.Fatal(err)
	}
	if !Drained(ctx, address) {
		t.Fatal("not drained after the node took the request")
	}
	_ = Drain(ctx, address)
	if err := Stop(ctx, address); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "drain")); err == nil {
		t.Fatal("stop left the request for the next start")
	}
}
