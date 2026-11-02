// Package install sets the agent up as a root systemd service with its own container runtime, and
// removes it again. Running install again repairs the setup and keeps the wallet, keys and claim.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nosana-ci/host-agent/internal/bundle"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/manifest"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

type Options struct {
	ControlURL string
	ReleaseURL string
	Network    string
	Token      string
}

func Run(ctx context.Context, o Options, out io.Writer) (err error) {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	say := sayTo(out)

	cfg, err := config.Load()
	if err == nil {
		say("found an existing install, repairing it")
		_ = service.Systemctl("stop", service.AgentUnit) // it would overwrite the state we change
		defer func() {
			if err != nil {
				_ = service.Systemctl("start", service.AgentUnit)
			}
		}()
	}
	if o.ControlURL != "" {
		cfg.ControlURL = o.ControlURL
	}
	if o.ReleaseURL != "" {
		cfg.ReleaseURL = o.ReleaseURL
	}
	if o.Network != "" {
		cfg.Network = o.Network
	}
	if cfg.ControlURL == "" || cfg.ReleaseURL == "" || (cfg.Network != "mainnet" && cfg.Network != "devnet") {
		return errors.New("--control-url, --release-url and --network (mainnet or devnet) are required on a first install")
	}
	for _, d := range []string{config.BinDir, config.NodeDir, config.PodmanRoot, config.RuntimeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// Traversable, not listable: the CLI symlink resolves for every user, the files inside stay private.
	_ = os.Chmod(config.AgentDir, 0o711)
	_ = os.Chmod(config.NodeDir, 0o700)

	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	slot := config.BinaryPath(version.Version)
	if self != slot {
		if err := store.CopyFile(self, slot, 0o755); err != nil {
			return err
		}
	}
	if err := store.Relink(config.CurrentBinary, slot); err != nil {
		return err
	}
	if err := store.Relink(config.CLIPath, config.CurrentBinary); err != nil {
		return err
	}
	say("installed agent %s", version.Version)

	st, err := store.Open(config.StatePath)
	if err != nil {
		return err
	}
	_ = st.Update(func(s *store.State) { s.Update.Pending = nil })

	m, err := manifest.Fetch(ctx, cfg.ReleaseURL)
	if err != nil {
		return fmt.Errorf("read the release from %s: %w", cfg.ReleaseURL, err)
	}
	if err := bundle.EnsureFirewallTool(ctx); err != nil {
		say("warning: %v", err)
	}
	// A podman restart can break a running job: only when what it runs changed.
	podman := "start"
	if bundle.Current() == "" {
		podman = "restart"
		ver, err := bundle.Install(ctx, m)
		if err != nil {
			return fmt.Errorf("install the container runtime: %w", err)
		}
		if err := bundle.Activate(ver); err != nil {
			return err
		}
		say("container runtime %s installed", ver)
	}
	if err := bundle.EnsurePolicy(); err != nil {
		return err
	}
	_ = st.Update(func(s *store.State) { s.NodeImage = m.Node.Image })

	wallet, err := keys.Wallet(config.WalletPath)
	if err != nil {
		return err
	}
	say("node address %s", keys.Address(wallet))
	if o.Token != "" {
		if err := store.WriteFile(config.ClaimTokenPath, []byte(o.Token), 0o600); err != nil {
			return err
		}
		say("claim token saved; the agent links this host to your account on start")

	}
	if err := cfg.Save(); err != nil {
		return err
	}
	changed, err := service.WriteUnits()
	if err != nil {
		return err
	}
	if changed {
		podman = "restart"
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", service.PodmanUnit, service.AgentUnit}, {podman, service.PodmanUnit}, {"restart", service.AgentUnit}} {
		if err := service.Systemctl(args...); err != nil {
			return err
		}
	}
	say("services started")
	fmt.Fprintln(out, "\nDone. Follow the agent with: journalctl -u nosana-host -f")
	return nil
}

// Uninstall stops and removes the services; purge also deletes the state after backing the
// wallet up into the home of the user who ran sudo.
func Uninstall(ctx context.Context, purge bool, out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	say := sayTo(out)
	_ = service.Systemctl("stop", service.AgentUnit)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	pc := podman.New(config.PodmanSocket)
	if err := pc.StopContainer(ctx, node.ContainerName, 2*time.Minute); err == nil {
		say("node container stopped")
	}
	if err := pc.RemoveContainer(ctx, node.ContainerName); err != nil {
		say("warning: the node container was not removed (%v); remove it with podman", err)
	}
	_ = service.Systemctl("disable", "--now", service.PodmanUnit, service.AgentUnit)
	_ = os.Remove(filepath.Join(service.UnitDir, service.AgentUnit))
	_ = os.Remove(filepath.Join(service.UnitDir, service.PodmanUnit))
	_ = os.Remove(config.CLIPath)
	_ = service.Systemctl("daemon-reload")
	say("services removed")
	if !purge {
		return nil
	}
	if _, err := os.Stat(config.WalletPath); err == nil {
		backup, err := backupWallet()
		if err != nil {
			return fmt.Errorf("the node wallet could not be backed up, so nothing was deleted: %w", err)
		}
		say("wallet backed up to %s", backup)
	}
	// Podman storage holds files owned by other uids; let podman clean it up.
	reset := exec.CommandContext(ctx, config.RuntimePodman, "--root", config.PodmanRoot, "--runroot", config.PodmanRunRoot, "system", "reset", "--force")
	reset.Env = append(os.Environ(), runtimeEnv()...)
	_ = reset.Run()
	_ = os.RemoveAll(config.StateDir)
	_ = os.RemoveAll(config.RunDir)
	_ = os.Remove(config.Path)
	say("state and config deleted")
	return nil
}

// sayTo writes the "• " progress lines of install and uninstall.
func sayTo(out io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) { fmt.Fprintf(out, "• "+format+"\n", args...) }
}

func backupWallet() (string, error) {
	home, uid, gid := "/root", -1, -1
	if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
		if u, err := user.Lookup(name); err == nil {
			home = u.HomeDir
			uid, _ = strconv.Atoi(u.Uid)
			gid, _ = strconv.Atoi(u.Gid)
		}
	}
	backup := filepath.Join(home, fmt.Sprintf("nosana_key.backup-%d.json", time.Now().Unix()))
	if err := store.CopyFile(config.WalletPath, backup, 0o600); err != nil {
		return "", err
	}
	if uid >= 0 {
		if err := os.Chown(backup, uid, gid); err != nil {
			return "", err
		}
	}
	return backup, nil
}

func runtimeEnv() []string {
	raw, _ := os.ReadFile(config.RuntimeEnv)
	var env []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			env = append(env, l)
		}
	}
	return env
}
