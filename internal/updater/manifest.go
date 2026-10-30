// Package updater keeps the host on the stable release: the agent binary (switched in place,
// rolled back when the new one does not come up), the runtime bundle and the node image.
package updater

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

	"github.com/nosana-ci/nosana-host/internal/api"
	"github.com/nosana-ci/nosana-host/internal/version"
)

var manifestClient = &http.Client{Timeout: 30 * time.Second}

// FetchManifest reads host-manager's current release and checks its signature with the release
// key built into this binary.
func FetchManifest(ctx context.Context, controlURL string) (*api.ReleaseManifest, error) {
	pub, err := base64.StdEncoding.DecodeString(version.ReleasePublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("this build has no release key; self-update is disabled")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(controlURL, "/")+"/releases/stable", nil)
	if err != nil {
		return nil, err
	}
	resp, err := manifestClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release manifest: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return Verify(raw, resp.Header.Get("X-Release-Signature"), pub)
}

// Verify checks the base64 signature over the manifest text and parses it.
func Verify(raw []byte, signature string, pub ed25519.PublicKey) (*api.ReleaseManifest, error) {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || !ed25519.Verify(pub, raw, sig) {
		return nil, errors.New("release manifest signature does not match the release key")
	}
	var m api.ReleaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse release manifest: %w", err)
	}
	return &m, nil
}
