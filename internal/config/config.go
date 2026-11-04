// Package config is the agent's configuration file and the fixed layout under /var/lib/nosana.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nosana-ci/host-agent/internal/api"
)

const (
	Path     = "/etc/nosana/host.json"
	StateDir = "/var/lib/nosana"
	RunDir   = "/run/nosana"
	CLIPath  = "/usr/local/bin/nosana-host"
)

type Config struct {
	ControlURL string `json:"controlUrl"` // host-manager
	ReleaseURL string `json:"releaseUrl"` // where stable.json and stable.json.sig live
	Network    string `json:"network"`    // mainnet | devnet
	// AutoUpdate unset means on: the agent installs new releases by itself. Off, it only reports
	// them, and the owner installs one with nosana-host update or the console.
	AutoUpdate *bool `json:"autoUpdate,omitempty"`
}

func Load() (Config, error) {
	var c Config
	raw, err := os.ReadFile(Path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c)
}

// Save writes the config file; the directory exists since the install.
func Save(c Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path)
}

func (c Config) AutoUpdateOn() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

// Settings is what the owner may change while the agent runs, as the heartbeat reports it.
func (c Config) Settings() api.AgentSettings { return api.AgentSettings{AutoUpdate: c.AutoUpdateOn()} }

var (
	AgentDir      = filepath.Join(StateDir, "agent")
	BinDir        = filepath.Join(AgentDir, "bin")
	CurrentBinary = filepath.Join(BinDir, "current")
	StatePath     = filepath.Join(AgentDir, "state.json")
	NodeDir       = filepath.Join(StateDir, "node") // /root/.nosana inside the node container

	WalletPath     = filepath.Join(NodeDir, "nosana_key.json")
	PodmanRoot     = filepath.Join(StateDir, "containers")
	RuntimeDir     = filepath.Join(StateDir, "runtime")
	RuntimeCurrent = filepath.Join(RuntimeDir, "current")
	RuntimePodman  = filepath.Join(RuntimeCurrent, "bin", "podman")
	RuntimeEnv     = filepath.Join(RuntimeCurrent, "env")

	PodmanRunRoot   = filepath.Join(RunDir, "containers")
	PodmanTmpDir    = filepath.Join(RunDir, "libpod-tmp")
	PodmanSocketDir = filepath.Join(RunDir, "podman")
	PodmanSocket    = filepath.Join(PodmanSocketDir, "podman.sock")
)

// BinaryPath is where an agent version is kept; bin/current points at one of them.
func BinaryPath(version string) string { return filepath.Join(BinDir, "nosana-host-"+version) }
