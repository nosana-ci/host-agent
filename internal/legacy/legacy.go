// Package legacy is the node setups the agent replaces, in docker: start.sh's privileged podman
// container (image nosana/podman) with the node inside it, and a node container run straight from
// the nosana/nosana-node image.
package legacy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nosana-ci/host-agent/internal/keys"
)

// docker runs the docker CLI; tests replace it.
var docker = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

// Node is a node that runs in docker on this machine.
type Node struct {
	Container string
	Address   string
	KeyPath   string // on this machine: the key file in the directory mounted at /root/.nosana
	startSh   bool
}

// Find lists the nodes that run in docker, with the key each one mounts. Without docker there are
// none.
func Find(ctx context.Context) []Node { return find(ctx, "ps") }

// find lists the node containers docker ps shows with args (-a includes stopped ones).
func find(ctx context.Context, args ...string) []Node {
	out, err := docker(ctx, append(args, "--format", "{{.Names}} {{.Image}}")...)
	if err != nil {
		return nil
	}
	var found []Node
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, image, _ := strings.Cut(line, " ")
		startSh := name == "podman" && strings.Contains(image, "nosana/podman")
		if !startSh && !strings.Contains(image, "nosana/nosana-node") {
			continue
		}
		if path := keyPath(ctx, name); path != "" {
			if key, err := keys.Read(path); err == nil {
				found = append(found, Node{Container: name, Address: keys.Address(key), KeyPath: path, startSh: startSh})
			}
		}
	}
	return found
}

// keyPath is the key file in the container's node directory, or "" when it mounts none.
func keyPath(ctx context.Context, name string) string {
	out, err := docker(ctx, "inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/root/.nosana"}}{{.Source}}{{end}}{{end}}`, name)
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "nosana_key.json")
}

// running is the nodes of address. A node with another key, a second node on this machine, is not
// the old one.
func running(ctx context.Context, address string) []Node {
	var nodes []Node
	for _, n := range Find(ctx) {
		if n.Address == address {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Running names the docker containers that run the node of address.
func Running(ctx context.Context, address string) []string {
	var names []string
	for _, n := range running(ctx, address) {
		names = append(names, n.Container)
	}
	return names
}

// drainFile asks a node to stop before its next job, never during one; the node removes it when it
// stops. Nodes from before this request ignore it.
func drainFile(n Node) string { return filepath.Join(filepath.Dir(n.KeyPath), "drain") }

// Drain asks the node of address to stop before its next job. The request is the file itself, so it
// holds across agent restarts.
func Drain(ctx context.Context, address string) error {
	for _, n := range running(ctx, address) {
		if err := os.WriteFile(drainFile(n), nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Drained reports whether the node of address took the drain request: it stopped by itself.
func Drained(ctx context.Context, address string) bool {
	nodes := running(ctx, address)
	for _, n := range nodes {
		if _, err := os.Stat(drainFile(n)); err == nil {
			return false
		}
	}
	return len(nodes) > 0
}

// Undrain withdraws the drain requests of every node container, stopped ones too, so none stops
// at a later start for a move that was undone.
func Undrain(ctx context.Context) {
	for _, n := range find(ctx, "ps", "-a") {
		_ = os.Remove(drainFile(n))
	}
}

// Stop stops the node of address wherever it runs in docker, and drops the container's restart
// policy so docker does not start it again at boot. In start.sh's container the node goes first, so
// it leaves the market queue, then the container; a node container gets the same time to leave.
func Stop(ctx context.Context, address string) error {
	for _, n := range running(ctx, address) {
		if out, err := docker(ctx, "update", "--restart=no", n.Container); err != nil {
			return fmt.Errorf("disable the restart of %s: %s", n.Container, strings.TrimSpace(string(out)))
		}
		grace := "120"
		if n.startSh {
			if out, err := docker(ctx, "exec", n.Container, "podman", "stop", "-t", grace, "nosana-node"); err != nil &&
				!strings.Contains(string(out), "not running") && !strings.Contains(string(out), "no such container") {
				return fmt.Errorf("stop the node in %s: %s", n.Container, strings.TrimSpace(string(out)))
			}
			grace = "10" // the node is out; podman's service inside ignores SIGTERM
		}
		if out, err := docker(ctx, "stop", "-t", grace, n.Container); err != nil {
			return fmt.Errorf("stop %s: %s", n.Container, strings.TrimSpace(string(out)))
		}
		_ = os.Remove(drainFile(n)) // a node from before drain left it; it would stop the next start
	}
	return nil
}
