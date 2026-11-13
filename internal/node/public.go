package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Domain is the public address the network reaches a node's API at.
func Domain(network, address string) string {
	cluster := "prd"
	if network == "devnet" {
		cluster = "dev"
	}
	return fmt.Sprintf("https://%s.node.k8s.%s.nos.ci", address, cluster)
}

var publicClient = &http.Client{Timeout: 5 * time.Second}

// Public asks the node's public domain for /node/info and expects the node's own answer for
// address. While the tunnel starts, something else answers with a placeholder page.
func Public(ctx context.Context, domain, address string) (*Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+"/node/info", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := publicClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("answered " + resp.Status)
	}
	var info Info
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || json.NewDecoder(resp.Body).Decode(&info) != nil {
		return nil, errors.New("answered, but not with the node's API (the tunnel is still starting)")
	}
	if info.Node != address {
		return nil, errors.New("answers for another node: " + info.Node)
	}
	return &info, nil
}

// Busy reports whether a node in state must not be stopped: a job or a benchmark would be lost.
func Busy(state string) bool { return state == "RUNNING_JOB" || state == "BENCHMARKING" }
