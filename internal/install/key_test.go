package install

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/legacy"
)

func TestChooseKey(t *testing.T) {
	given := filepath.Join(t.TempDir(), "nosana_key.json")
	if _, err := keys.Wallet(given); err != nil {
		t.Fatal(err)
	}
	docker := []legacy.Node{{Container: "podman", Address: "NodeA", KeyPath: "/home/me/.nosana/nosana_key.json"}}
	two := append(docker[:1:1], legacy.Node{Container: "node2", Address: "NodeB", KeyPath: "/srv/b/nosana_key.json"})
	const earlier = "/root/.nosana/nosana_key.json"
	answer := func(yes bool, asked *int) func(string) bool {
		return func(string) bool { *asked++; return yes }
	}
	for name, c := range map[string]struct {
		key        string // --import-key
		found      []legacy.Node
		earlier    string
		haveWallet bool
		terminal   bool
		yes        bool
		want       string
		wantErr    string
		wantAsked  int
	}{
		"the key given goes first, without a question":   {key: given, found: docker, earlier: earlier, terminal: true, yes: true, want: given},
		"a key file that is none stops the install":      {key: "/nonexistent/key.json", wantErr: "--import-key"},
		"a node in docker is offered and moved":          {found: docker, terminal: true, yes: true, want: docker[0].KeyPath, wantAsked: 1},
		"a node in docker that is not moved stops it":    {found: docker, terminal: true, yes: false, wantErr: "NodeA /home/me/.nosana/nosana_key.json", wantAsked: 1},
		"without a terminal a node in docker stops it":   {found: docker, wantErr: "--import-key <key file>"},
		"two nodes in docker are not chosen between":     {found: two, terminal: true, yes: true, wantErr: "NodeB /srv/b/nosana_key.json"},
		"the earlier key is offered to a new agent":      {earlier: earlier, terminal: true, yes: true, want: earlier, wantAsked: 1},
		"saying no makes a new wallet":                   {earlier: earlier, terminal: true, yes: false, wantAsked: 1},
		"an agent with a wallet keeps it":                {earlier: earlier, haveWallet: true, terminal: true, yes: true},
		"without a terminal the earlier key is not used": {earlier: earlier},
		"a node in docker goes before the earlier key":   {found: docker, earlier: earlier, terminal: true, yes: true, want: docker[0].KeyPath, wantAsked: 1},
		"nothing found: the agent's own wallet":          {terminal: true, yes: true},
	} {
		asked := 0
		opts := Options{ImportKey: c.key}
		if c.terminal {
			opts.Ask = answer(c.yes, &asked)
		}
		got, err := chooseKey(opts, c.found, c.earlier, c.haveWallet, func(string, ...any) {})
		switch {
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: got error %v, want one with %q", name, err, c.wantErr)
		case c.wantErr == "" && (err != nil || got != c.want):
			t.Errorf("%s: got %q, %v; want %q", name, got, err, c.want)
		case asked != c.wantAsked:
			t.Errorf("%s: asked %d questions, want %d", name, asked, c.wantAsked)
		}
	}
}
