// Package control talks to host-manager: the claim, heartbeats, the command long-poll, command
// results and the agent's events. Every request is signed with the node wallet, the way the node itself talks to host-manager.
package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/keys"
)

// ErrUnknownHost is host-manager's answer when the node is not linked to an account: the host was
// unlinked and needs a new claim.
var ErrUnknownHost = errors.New("unknown host")

// HTTPError is an answer with a status outside 2xx and host-manager's message.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

type Client struct {
	base string
	auth string // address:signature, the fixed part of the authorization header
	http *http.Client
}

func New(controlURL string, wallet ed25519.PrivateKey) *Client {
	address := keys.Address(wallet)
	return &Client{
		base: strings.TrimRight(controlURL, "/"),
		auth: address + ":" + keys.Base58(ed25519.Sign(wallet, []byte(address))),
		http: &http.Client{Timeout: 40 * time.Second}, // above the 25 s long-poll
	}
}

// Claim links this node to the account that made the token.
func (c *Client) Claim(ctx context.Context, token, hostname string) error {
	return c.signed(ctx, http.MethodPost, "/agent/claim", api.ClaimBody{Token: token, Hostname: hostname}, nil)
}

func (c *Client) Heartbeat(ctx context.Context, status api.AgentStatus) ([]api.AgentCommand, error) {
	var out api.AgentCommands
	err := c.signed(ctx, http.MethodPost, "/agent/heartbeat", api.HeartbeatBody{Status: status}, &out)
	return out.Commands, err
}

// WaitCommands returns queued commands, waiting up to 25 seconds for one.
func (c *Client) WaitCommands(ctx context.Context) ([]api.AgentCommand, error) {
	var out api.AgentCommands
	err := c.signed(ctx, http.MethodGet, "/agent/commands", nil, &out)
	return out.Commands, err
}

// Registration is the node's contact details, as the node asks them on its first start.
type Registration struct {
	Email   string `json:"email"`
	Discord string `json:"discord,omitempty"`
	Twitter string `json:"twitter,omitempty"`
}

// Register is the node's own registration with host-manager, signed with the node wallet. For a
// host the console linked it fills in the contact details the link left empty.
func (c *Client) Register(ctx context.Context, r Registration) error {
	return c.signed(ctx, http.MethodPost, "/nodes/register", r, nil)
}

func (c *Client) Report(ctx context.Context, id int, result api.CommandResult) error {
	return c.signed(ctx, http.MethodPost, "/agent/commands/"+strconv.Itoa(id)+"/result", result, nil)
}

// Events reports what the agent did by itself or for its CLI; host-manager stores an event it got
// before once.
func (c *Client) Events(ctx context.Context, events []api.AgentEvent) error {
	return c.signed(ctx, http.MethodPost, "/agent/events", api.AgentEventsBody{Events: events}, nil)
}

// signed sends a request with the SDK authorization header: the address, the base58 signature
// over it with the node wallet, and the time in milliseconds.
func (c *Client) signed(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", c.auth+":"+strconv.FormatInt(time.Now().UnixMilli(), 10))

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e api.Error
		_ = json.Unmarshal(raw, &e)
		if resp.StatusCode == http.StatusNotFound && e.Message == ErrUnknownHost.Error() {
			return ErrUnknownHost
		}
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return &HTTPError{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
