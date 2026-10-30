// nosana-host-publish writes a release manifest for the artifacts CI uploaded, signs it with the
// release key and publishes it to host-manager. The key is the base64 ed25519 seed in
// RELEASE_SIGNING_KEY.
//
//	nosana-host-publish --version 1.2.0 --agent URL --agent-sha SHA --runtime-version 5.8.8 \
//	    --runtime URL --runtime-sha SHA --node-image IMAGE --host-manager URL
//	nosana-host-publish keygen        prints a new seed and public key
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nosana-ci/nosana-host/internal/api"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "keygen" {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		fmt.Printf("RELEASE_SIGNING_KEY=%s\nRELEASE_PUBLIC_KEY=%s\n", base64.StdEncoding.EncodeToString(key.Seed()), base64.StdEncoding.EncodeToString(pub))
		return nil
	}
	fs := flag.NewFlagSet("nosana-host-publish", flag.ExitOnError)
	var m api.ReleaseManifest
	fs.StringVar(&m.Version, "version", "", "agent version")
	fs.StringVar(&m.Agent.Artifacts.LinuxAmd64.Url, "agent", "", "URL of the linux-amd64 agent binary")
	fs.StringVar(&m.Agent.Artifacts.LinuxAmd64.Sha256, "agent-sha", "", "its sha256")
	fs.StringVar(&m.Runtime.Version, "runtime-version", "", "runtime bundle version")
	fs.StringVar(&m.Runtime.Artifacts.LinuxAmd64.Url, "runtime", "", "URL of the runtime bundle")
	fs.StringVar(&m.Runtime.Artifacts.LinuxAmd64.Sha256, "runtime-sha", "", "its sha256")
	fs.StringVar(&m.Node.Image, "node-image", "", "node image, e.g. docker.io/nosana/nosana-node:1.2.3")
	hostManager := fs.String("host-manager", "", "host-manager URL to publish to")
	_ = fs.Parse(args)
	for name, v := range map[string]string{"version": m.Version, "agent": m.Agent.Artifacts.LinuxAmd64.Url, "agent-sha": m.Agent.Artifacts.LinuxAmd64.Sha256,
		"runtime-version": m.Runtime.Version, "runtime": m.Runtime.Artifacts.LinuxAmd64.Url, "runtime-sha": m.Runtime.Artifacts.LinuxAmd64.Sha256,
		"node-image": m.Node.Image, "host-manager": *hostManager} {
		if v == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("RELEASE_SIGNING_KEY must be a base64 ed25519 seed")
	}
	m.PublishedAt = time.Now().UTC().Truncate(time.Second)
	manifest, err := json.Marshal(m)
	if err != nil {
		return err
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), manifest))
	body, _ := json.Marshal(api.PublishReleaseBody{Manifest: string(manifest), Signature: signature})
	resp, err := http.Post(strings.TrimRight(*hostManager, "/")+"/releases/", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("host-manager answered %d: %s", resp.StatusCode, answer)
	}
	fmt.Printf("published %s: %s\n", m.Version, answer)
	return nil
}
