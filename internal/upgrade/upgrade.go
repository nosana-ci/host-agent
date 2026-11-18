// Package upgrade opens a request on a unix socket that the server turns into a raw two-way byte
// stream (HTTP 101): podman's attach, and the agent's own attach for the CLI.
package upgrade

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
)

// StatusError is the server's answer when it did not switch to a stream.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return e.Body }

// Dial posts to path on the socket and returns the stream; ctx only limits the handshake.
func Dial(ctx context.Context, socket, path string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	req, _ := http.NewRequest(http.MethodPost, "http://d"+path, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	r := bufio.NewReader(conn)
	var resp *http.Response
	if err = req.Write(conn); err == nil {
		resp, err = http.ReadResponse(r, req)
	}
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = conn.Close()
		return nil, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return &stream{Conn: conn, r: r}, nil
}

// stream reads through the handshake's buffer, which may hold the stream's first bytes.
type stream struct {
	net.Conn
	r *bufio.Reader
}

func (s *stream) Read(p []byte) (int, error) { return s.r.Read(p) }
