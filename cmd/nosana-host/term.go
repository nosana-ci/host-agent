package main

import (
	"bytes"
	"cmp"
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

// screenReset restores the colors, and the cursor the node's spinners hide.
const screenReset = "\x1b[0m\x1b[?25h"

// nodeAttach joins this terminal to the node's with podman's attach, once the node runs. Ctrl+C
// detaches: passed on, it would stop the node. Podman's socket is root's, hence sudo.
func nodeAttach() error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("node attach needs a terminal; nosana-host logs node -f follows the log without one")
	}
	waitForNode()
	lines := termlines.NewWriter(os.Stdout, true) // the node only draws what changes: its last lines first
	lines.Open = true
	_ = readNodeLog(context.Background(), 20, time.Time{}, false, lines)
	_ = lines.Close()
	attach := exec.Command(config.RuntimePodman, "--remote", "--url", "unix://"+config.PodmanSocket,
		"attach", "--detach-keys", "ctrl-c", "--sig-proxy=false", node.ContainerName)
	var said bytes.Buffer
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, &said
	err := attach.Run()
	fmt.Println(screenReset)
	switch msg := strings.TrimSpace(said.String()); {
	case strings.Contains(msg, "detached from container"): // podman reports the detach key as an error
		fmt.Println("detached; the node keeps running")
	case err != nil:
		return errors.New("podman attach: " + cmp.Or(strings.TrimPrefix(msg, "Error: "), err.Error()))
	default:
		fmt.Println("the node stopped")
	}
	return nil
}

// waitForNode returns once the node container runs: after an install that takes a while.
func waitForNode() {
	pc := podman.New(config.PodmanSocket)
	for said := false; ; time.Sleep(2 * time.Second) {
		if ctr, err := pc.InspectContainer(context.Background(), node.ContainerName); err == nil && ctr.State.Running {
			return
		}
		if !said {
			said = true
			fmt.Println("waiting for the node to start (a first start downloads its image; nosana-host status shows more). Ctrl+C stops waiting, not the node")
		}
	}
}

// followSize keeps the node's terminal the size of this one, so its spinners fit, until stop.
func followSize() (stop func()) {
	resize := func() {
		var ws struct{ Row, Col, X, Y uint16 }
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
		if errno == 0 && ws.Col > 0 && ws.Row > 0 {
			_ = agent(http.MethodPost, fmt.Sprintf("/node/resize?w=%d&h=%d", ws.Col, ws.Row), nil, nil)
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
	return func() { signal.Stop(winch) }
}

// nodeLog writes the node's log, which is what it drew on its terminal: first as the lines it
// left, then, when followed on a terminal, live as the node draws, sized to this terminal.
func nodeLog(ctx context.Context, tail int, follow bool) error {
	tty := isTerminal(os.Stdout)
	live := tty && follow
	if live {
		defer followSize()()
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

// readNodeLog reads the node's log through the agent, or from podman's socket while it is down.
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
