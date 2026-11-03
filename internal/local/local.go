// Package local is the agent's socket for the CLI: status, logs and the doctor without sudo, for
// root and the members of the nosana group.
package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/config"
)

const Group = "nosana"

var SocketPath = config.RunDir + "/cli.sock"

// Status is what the agent knows, as the CLI shows it.
type Status struct {
	Agent        string `json:"agent"`
	NodeAddress  string `json:"nodeAddress"`
	ControlURL   string `json:"controlUrl"`
	Network      string `json:"network"`
	Claimed      bool   `json:"claimed"`
	ClaimPending bool   `json:"claimPending"` // a saved token the agent still has to use

	LastRollback string          `json:"lastRollback,omitempty"`
	Status       api.AgentStatus `json:"status"`
}

// Listen opens the socket for root and the nosana group.
func Listen() (net.Listener, error) {
	if err := os.MkdirAll(config.RunDir, 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(SocketPath)
	l, err := net.Listen("unix", SocketPath)
	if err != nil {
		return nil, err
	}
	if g, err := user.LookupGroup(Group); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		_ = os.Chown(SocketPath, 0, gid)
	}
	return l, os.Chmod(SocketPath, 0o660)
}

// Client talks to the agent over the socket; the URL host is ignored.
func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath)
		},
	}}
}

// ErrNotRunning is Explain's answer when nothing listens on the socket.
var ErrNotRunning = errors.New("the agent is not running: systemctl status nosana-host")

// Explain turns a connection error into what the user should do.
func Explain(err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("the CLI is for root and the %s group; log in again if the install just added you, or use sudo", Group)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return ErrNotRunning
	}
	return err
}
