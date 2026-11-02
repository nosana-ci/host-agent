// Package manifest is the release manifest CI publishes next to the artifacts: stable.json and its
// detached signature stable.json.sig, signed with the release key built into every agent.
package manifest

import (
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/version"
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

var (
	client         = &http.Client{Timeout: 30 * time.Second}
	downloadClient = &http.Client{Timeout: 15 * time.Minute}
)

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

// Download fetches the artifact to dst; dst appears only when the sha256 matched.
func (a Artifact) Download(ctx context.Context, dst string, mode os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Url, nil)
	if err != nil {
		return err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", a.Url, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".download"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, a.Sha256) {
		_ = os.Remove(tmp)
		return fmt.Errorf("checksum mismatch for %s: got %s, the manifest says %s", a.Url, got, a.Sha256)
	}
	return os.Rename(tmp, dst)
}
