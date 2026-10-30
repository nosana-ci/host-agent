// Package control talks to host-manager: the claim, heartbeats, the command long-poll and command
// results. After the claim every request is signed with the device key.
package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/nosana-host/internal/api"
	"github.com/nosana-ci/nosana-host/internal/keys"
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
	base    string
	address string
	wallet  ed25519.PrivateKey
	device  ed25519.PrivateKey
	http    *http.Client
}

func New(controlURL string, wallet, device ed25519.PrivateKey) *Client {
	return &Client{
		base:    strings.TrimRight(controlURL, "/"),
		address: keys.Address(wallet),
		wallet:  wallet,
		device:  device,
		http:    &http.Client{Timeout: 40 * time.Second}, // above the 25 s long-poll
	}
}

// Claim links this machine to the account that made the token. The request is signed with the
// node wallet, the way the node itself talks to host-manager.
func (c *Client) Claim(ctx context.Context, token, hostname string) error {
	body := api.ClaimBody{Token: token, DevicePublicKey: keys.PublicBase64(c.device), Hostname: hostname}
	req, err := c.request(ctx, http.MethodPost, "/agent/claim", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", walletHeader(c.wallet, c.address))
	return c.do(req, nil)
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

func (c *Client) Report(ctx context.Context, id int, result api.CommandResult) error {
	return c.signed(ctx, http.MethodPost, "/agent/commands/"+strconv.Itoa(id)+"/result", result, nil)
}

// signed sends a request signed with the device key: method, path, time and body hash.
func (c *Client) signed(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ed25519.Sign(c.device, SignedMessage(method, path, ts, raw))
	req.Header.Set("X-Nosana-Node", c.address)
	req.Header.Set("X-Nosana-Timestamp", ts)
	req.Header.Set("X-Nosana-Signature", base64.StdEncoding.EncodeToString(sig))
	return c.do(req, out)
}

// SignedMessage is what host-manager verifies: METHOD, path, unix seconds and the sha256 of the
// body (hex), one per line.
func SignedMessage(method, path, timestamp string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:]))
}

// walletHeader is the SDK authorization header: the address, the base58 signature over it and
// the time in milliseconds.
func walletHeader(key ed25519.PrivateKey, address string) string {
	sig := ed25519.Sign(key, []byte(address))
	return address + ":" + keys.Base58(sig) + ":" + strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(req *http.Request, out any) error {
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
