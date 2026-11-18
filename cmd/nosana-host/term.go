package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/termlines"
)

// screenReset ends what the node drew on this terminal: its colors, and the cursor its spinners
// hide.
const screenReset = "\x1b[0m\x1b[?25h"

// nodeAttach joins this terminal to the node's with podman's own attach: it shows what the node
// draws, and the keys typed here are the node's (s while a job runs: shut down after it). Ctrl+C
// is the key that leaves, and is not passed on: on the node's own terminal it would stop the
// node. A node that is on its way is waited for (see waitForNode), so the install can end here.
// Podman's socket is root's, hence sudo.
func nodeAttach() error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("node attach needs a terminal; nosana-host logs node -f follows the log without one")
	}
	if err := waitForNode(os.Stdout); err != nil {
		return err
	}
	// The node only draws what changes from now on: its last lines first, for what it shows.
	lines := termlines.NewWriter(os.Stdout, true)
	lines.Open = true
	_ = readNodeLog(context.Background(), 20, time.Time{}, false, lines)
	_ = lines.Close()
	attach := exec.Command(config.RuntimePodman, "--remote", "--url", "unix://"+config.PodmanSocket,
		"attach", "--detach-keys", "ctrl-c", "--sig-proxy=false", node.ContainerName)
	var said bytes.Buffer // podman's own messages; what the node writes comes over its terminal
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, &said
	err := attach.Run()
	fmt.Println(screenReset)
	return attachEnd(err, said.String(), os.Stdout)
}

// attachEnd says how podman's attach ended: left with the detach key, which podman reports as
// an error, or because the node stopped; anything else podman said is the error.
func attachEnd(err error, said string, out io.Writer) error {
	switch said = strings.TrimSpace(said); {
	case strings.Contains(said, "detached from container"):
		fmt.Fprintln(out, "detached; the node keeps running")
	case err != nil && said != "":
		return errors.New("podman attach: " + strings.TrimPrefix(said, "Error: "))
	case err != nil:
		return fmt.Errorf("podman attach: %w", err)
	default:
		fmt.Fprintln(out, "the node's terminal closed: the node stopped")
	}
	return nil
}

// agentGrace is how long waitForNode waits for an agent that does not answer: an install has
// just started it. waitEvery is how often it asks.
var agentGrace, waitEvery = 15 * time.Second, 2 * time.Second

// waitForNode returns once the node runs. Until then it says what the agent says the node's
// start waits for (a takeover from the old node, the image pull, a restart after a crash), each
// thing once. A node its owner stopped is not waited for.
func waitForNode(out io.Writer) error {
	said := ""
	for begin := time.Now(); ; time.Sleep(waitEvery) {
		var s local.NodeStart
		err := agent(http.MethodGet, "/node/start", nil, &s)
		switch {
		case errors.Is(err, local.ErrNotRunning) && time.Since(begin) < agentGrace:
			s.Waiting = "the agent starts"
		case err != nil:
			return err
		case s.Running:
			return nil
		case s.Waiting == "":
			return errors.New("the node is stopped; start it with: nosana-host node start")
		}
		if said == "" {
			fmt.Fprintln(out, "waiting for the node to start, to show its terminal; Ctrl+C stops waiting, and the node starts either way")
		}
		if s.Waiting != said {
			said = s.Waiting
			fmt.Fprintln(out, "  "+said)
		}
	}
}

// followSize keeps the node's terminal the size of this one, so the node's spinners and bars
// fit it, until the returned function is called. It does nothing when the agent cannot do it.
func followSize(ctx context.Context) (stop func()) {
	resize := func() {
		if cols, rows, ok := termSize(os.Stdout); ok {
			if body, err := local.Call(ctx, http.MethodPost, fmt.Sprintf("/node/resize?w=%d&h=%d", cols, rows), nil); err == nil {
				_ = body.Close()
			}
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			resize()
		}
	}()
	return func() {
		signal.Stop(winch)
		close(winch)
	}
}

func termSize(f *os.File) (cols, rows int, ok bool) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return int(ws.Col), int(ws.Row), errno == 0 && ws.Col > 0 && ws.Row > 0
}

// nodeLog writes the node container's log, which is what the node drew on its terminal: every
// frame of its spinners and bars. What is there already is written as the lines the node left,
// in color on a terminal. A followed log on a terminal then shows what the node draws as it
// draws it, with the node's terminal sized to this one.
func nodeLog(ctx context.Context, tail int, follow bool) error {
	tty := isTerminal(os.Stdout)
	live := tty && follow
	if live {
		defer followSize(ctx)()
	}
	start := time.Now()
	lines := termlines.NewWriter(os.Stdout, tty)
	lines.Open = live
	err := readNodeLog(ctx, tail, time.Time{}, follow && !live, lines)
	_ = lines.Close()
	if err != nil || !live {
		return err
	}
	defer fmt.Println(screenReset)
	return readNodeLog(ctx, 0, start, true, os.Stdout)
}

// readNodeLog reads the node's log through the agent, or while the agent is down from podman's
// socket, which is root's: the last tail lines, or what the node wrote since a time.
func readNodeLog(ctx context.Context, tail int, since time.Time, follow bool, out io.Writer) error {
	path := fmt.Sprintf("/logs/node?tail=%d&follow=%t", tail, follow)
	if !since.IsZero() {
		path += fmt.Sprintf("&since=%d", since.UnixNano())
	}
	body, err := local.Call(ctx, http.MethodGet, path, nil)
	if errors.Is(err, local.ErrNotRunning) {
		err = podman.New(config.PodmanSocket).Logs(ctx, node.ContainerName, tail, since, follow, out)
		if errors.Is(err, os.ErrPermission) {
			err = errors.New("the agent is not running; podman's socket is root's: sudo nosana-host logs node")
		}
		return err
	}
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = io.Copy(out, body)
	return err
}
