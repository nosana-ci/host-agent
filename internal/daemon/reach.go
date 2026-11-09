package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
)

// reachEvery is how often the agent asks the node's public domain for /node/info, the way jobs
// and the explorer reach the node.
const reachEvery = 2 * time.Minute

// NodeDomain is the public address the network reaches a node's API at.
func NodeDomain(network, address string) string {
	cluster := "prd"
	if network == "devnet" {
		cluster = "dev"
	}
	return fmt.Sprintf("https://%s.node.k8s.%s.nos.ci", address, cluster)
}

var reachClient = &http.Client{Timeout: 5 * time.Second}

type reach struct {
	url, address string

	mu   sync.Mutex
	last *api.ReachStatus
}

func (r *reach) run(ctx context.Context) {
	sleep(ctx, 20*time.Second) // the tunnel needs the node up first
	for ctx.Err() == nil {
		r.refresh(ctx)
		sleep(ctx, reachEvery)
	}
}

func (r *reach) refresh(ctx context.Context) {
	next := probe(ctx, r.url, r.address)
	r.mu.Lock()
	r.last = &next
	r.mu.Unlock()
}

func (r *reach) status() *api.ReachStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// probe asks url for /node/info and expects the node's own answer for this address. While the
// tunnel starts, something else answers with a placeholder page.
func probe(ctx context.Context, url, address string) api.ReachStatus {
	st := api.ReachStatus{Url: url, CheckedAt: time.Now().UTC()}
	fail := func(msg string) api.ReachStatus {
		st.Error = &msg
		return st
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/node/info", nil)
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	resp, err := reachClient.Do(req)
	if err != nil {
		return fail(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail("answered " + resp.Status)
	}
	var info struct {
		Node string `json:"node"`
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || json.NewDecoder(resp.Body).Decode(&info) != nil {
		return fail("answered, but not with the node's API (the tunnel is still starting)")
	}
	if info.Node != address {
		return fail("answers for another node: " + info.Node)
	}
	st.Ok = true
	return st
}

// check turns the probe into a doctor check; a node that is not running cannot be reached, so
// that is a warning rather than a failure.
func (r *reach) check(nodeRunning bool) *api.Check {
	st := r.status()
	if st == nil {
		return nil
	}
	group := "network"
	c := api.Check{Id: "node-domain", Group: &group, Title: "Node reachable at its public domain", Status: api.CheckStatusOk}
	detail := st.Url
	if !st.Ok {
		detail += ": " + *st.Error
		c.Status = api.CheckStatusWarn
		if nodeRunning {
			c.Status = api.CheckStatusFail
			fix := "The node runs but the network cannot reach it: look for tunnel errors in nosana-host logs node; a node restart usually sets it up again."
			c.Fix = &fix
		}
	}
	c.Detail = &detail
	return &c
}
