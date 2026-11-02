// Package podman is the small libpod REST client the agent needs over the podman socket.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiPrefix = "/v5.0.0/libpod"

var (
	ErrNotFound    = errors.New("not found")
	ErrSubnetInUse = errors.New("subnet already used")
)

type Client struct {
	http *http.Client
}

func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 5 * time.Minute, // a stop may take the node's whole drain timeout
	}
	return &Client{http: &http.Client{Transport: tr}}
}

type Info struct {
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var info Info
	return &info, c.call(ctx, http.MethodGet, "/info", nil, nil, &info)
}

func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name)+"/exists", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CreateNetwork makes a bridge network with container DNS on: the node's API tunnel finds the
// node by name on it.
func (c *Client) CreateNetwork(ctx context.Context, name, subnet, gateway string) error {
	body := map[string]any{
		"name": name, "driver": "bridge", "dns_enabled": true,
		"subnets": []map[string]string{{"subnet": subnet, "gateway": gateway}},
	}
	err := c.call(ctx, http.MethodPost, "/networks/create", nil, body, nil)
	if err != nil && strings.Contains(err.Error(), "already used") {
		return fmt.Errorf("%w: %s", ErrSubnetInUse, subnet)
	}
	return err
}

type Image struct {
	ID string `json:"Id"`
}

func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	var img Image
	return &img, c.call(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img)
}

// Pull pulls ref and returns the image ID. Progress is streamed (not quiet), so the answer's
// headers arrive at once and a long pull does not run into the header timeout.
func (c *Client) Pull(ctx context.Context, ref string) (string, error) {
	q := url.Values{"reference": {ref}, "policy": {"always"}}
	resp, err := c.do(ctx, http.MethodPost, "/images/pull", q, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", apiError(resp)
	}
	var id string
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error  string   `json:"error"`
			ID     string   `json:"id"`
			Images []string `json:"images"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return id, nil
		} else if err != nil {
			return "", err
		}
		if msg.Error != "" {
			return "", fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
		if msg.ID != "" {
			id = msg.ID
		} else if len(msg.Images) > 0 {
			id = msg.Images[0]
		}
	}
}

type Container struct {
	State struct {
		Status    string    `json:"Status"`
		Running   bool      `json:"Running"`
		ExitCode  int       `json:"ExitCode"`
		StartedAt time.Time `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (c *Client) InspectContainer(ctx context.Context, name string) (*Container, error) {
	var ctr Container
	return &ctr, c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &ctr)
}

type Mount struct{ Source, Destination string }

type Port struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type Spec struct {
	Name       string
	Image      string
	Command    []string
	Env        map[string]string
	Labels     map[string]string
	Mounts     []Mount
	Network    string
	Ports      []Port
	LogSizeMax int64
}

func (c *Client) CreateContainer(ctx context.Context, s Spec) error {
	mounts := make([]map[string]any, 0, len(s.Mounts))
	for _, m := range s.Mounts {
		mounts = append(mounts, map[string]any{"type": "bind", "source": m.Source, "destination": m.Destination, "options": []string{"rbind"}})
	}
	body := map[string]any{
		"name": s.Name, "image": s.Image, "command": s.Command, "env": s.Env, "labels": s.Labels,
		"mounts": mounts, "portmappings": s.Ports,
		"netns":    map[string]string{"nsmode": "bridge"},
		"Networks": map[string]any{s.Network: map[string]any{}},
		// The agent supervises restarts itself.
		"restart_policy": "no",
		// k8s-file honors the size cap; journald ignores it.
		"log_configuration": map[string]any{"driver": "k8s-file", "size": s.LogSizeMax},
	}
	return c.call(ctx, http.MethodPost, "/containers/create", nil, body, nil)
}

func (c *Client) StartContainer(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil, nil)
}

func (c *Client) StopContainer(ctx context.Context, name string, timeout time.Duration) error {
	q := url.Values{"timeout": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop", q, nil, nil)
}

func (c *Client) RemoveContainer(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	u := "http://d" + apiPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusNotModified { // already stopped or already running
		return nil
	}
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return fmt.Errorf("podman: %s (HTTP %d)", e.Message, resp.StatusCode)
	}
	return fmt.Errorf("podman: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}
