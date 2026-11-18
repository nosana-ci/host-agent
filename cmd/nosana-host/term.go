package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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

const ctrlC = 0x03

// nodeAttach joins this terminal to the node's: it shows what the node draws, and the keys typed
// here are the node's (s while a job runs: shut down after it). Ctrl+C leaves, and is not
// passed on: on the node's own terminal it would stop the node.
func nodeAttach() error {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("node attach needs a terminal; nosana-host logs node -f follows the log without one")
	}
	ctx := context.Background()
	conn, err := local.Attach(ctx, "/node/attach")
	if err != nil {
		return err
	}
	defer conn.Close()
	defer followSize(ctx)()
	// The node only draws what changes from now on: its last lines first, for what it shows.
	lines := termlines.NewWriter(os.Stdout, true)
	lines.Open = true
	_ = readNodeLog(ctx, 20, time.Time{}, false, lines)
	_ = lines.Close()
	restore, err := rawMode(os.Stdin)
	if err != nil {
		return err
	}
	done := make(chan string, 2)
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		done <- "the node's terminal closed: the node or the agent stopped"
	}()
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if i := bytes.IndexByte(buf[:n], ctrlC); i >= 0 || err != nil {
				_, _ = conn.Write(buf[:max(i, 0)])
				done <- "detached; the node keeps running"
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return // the other copy says why
			}
		}
	}()
	msg := <-done
	restore()
	fmt.Println(screenReset + "\n" + msg)
	return nil
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

// rawMode hands every key to the reader as it is typed, unechoed and uninterpreted, until
// restore is called.
func rawMode(f *os.File) (restore func(), err error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	return func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
	}, nil
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
