// Package manifest is the release manifest CI publishes next to the artifacts: stable.json and its
// detached signature stable.json.sig, signed with the release key built into every agent.
package manifest

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nosana-ci/nosana-host/internal/version"
)

type Artifact struct {
	Url    string `json:"url"`
	Sha256 string `json:"sha256"`
}

// Artifacts per platform; v1 ships one.
type Artifacts struct {
	LinuxAmd64 Artifact `json:"linux-amd64"`
}

type Manifest struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"publishedAt"`
	Agent       struct {
		Artifacts Artifacts `json:"artifacts"`
	} `json:"agent"`
	Runtime struct {
		Version   string    `json:"version"`
		Artifacts Artifacts `json:"artifacts"`
	} `json:"runtime"`
	Node struct {
		Image string `json:"image"`
	} `json:"node"`
}

var client = &http.Client{Timeout: 30 * time.Second}

// Fetch reads <releaseURL>/stable.json and its signature and checks it with the release key
// built into this binary.
func Fetch(ctx context.Context, releaseURL string) (*Manifest, error) {
	pub, err := base64.StdEncoding.DecodeString(version.ReleasePublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("this build has no release key; self-update is disabled")
	}
	base := strings.TrimRight(releaseURL, "/")
	raw, err := get(ctx, base+"/stable.json")
	if err != nil {
		return nil, err
	}
	sig, err := get(ctx, base+"/stable.json.sig")
	if err != nil {
		return nil, err
	}
	return Verify(raw, strings.TrimSpace(string(sig)), pub)
}

// Verify checks the base64 signature over the manifest text and parses it.
func Verify(raw []byte, signature string, pub ed25519.PublicKey) (*Manifest, error) {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || !ed25519.Verify(pub, raw, sig) {
		return nil, errors.New("release manifest signature does not match the release key")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse release manifest: %w", err)
	}
	return &m, nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return raw, nil
}
