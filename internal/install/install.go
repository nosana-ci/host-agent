// Package install sets the agent up as a root systemd service with its own container runtime, and
// removes it again. Running install again repairs the setup and keeps the wallet, keys and claim.
package install

import (
	"context"
	"encoding/json"
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
	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/legacy"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/manifest"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

// Options are install's flags besides the config.
type Options struct {
	Token string // links the host to an account
	// Hold installs without a node wallet and leaves the agent stopped and disabled, for a node
	// import to start it with its key: the agent would otherwise make a wallet, also at a reboot,
	// and link that one with Token.
	Hold bool
	// ImportKey is a key file that becomes the node wallet with this install: the agent is
	// installed held, and the key imported (see Import).
	ImportKey string
	// Ask asks a yes or no question on the terminal; nil without one. With it, a node found in
	// docker or the key of an earlier node is offered as ImportKey.
	Ask func(question string) bool
}

// Run installs or repairs the agent. Fields set in given replace the saved config.
func Run(ctx context.Context, given config.Config, opts Options, out io.Writer) (err error) {
	say := sayTo(out)

	cfg, loadErr := config.Load()
	if given.ControlURL != "" {
		cfg.ControlURL = given.ControlURL
	}
	if given.ReleaseURL != "" {
		cfg.ReleaseURL = given.ReleaseURL
	}
	if given.Network != "" {
		cfg.Network = given.Network
	}
	if cfg.ControlURL == "" || cfg.ReleaseURL == "" || (cfg.Network != "mainnet" && cfg.Network != "devnet") {
		return errors.New("--control-url, --release-url and --network (mainnet or devnet) are required on a first install")
	}
	// Before anything changes: which key the node gets. With one, the agent is installed without
	// starting it, as it would otherwise make a wallet of its own and link that one to the account.
	_, walletErr := os.Stat(config.WalletPath)
	key, err := chooseKey(opts, legacy.Find(ctx), earlierKey(), walletErr == nil, say)
	if err != nil {
		return err
	}
	opts.Hold = opts.Hold || key != ""
	held := false // the install is done and the key's import took over
	if loadErr == nil {
		say("found an existing install, repairing it")
		_ = service.Systemctl("stop", service.AgentUnit) // it would overwrite the state we change
		defer func() {
			if err != nil && !held {
				_ = service.Systemctl("start", service.AgentUnit)
			}
		}()
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
	if err := doctor.EnsureFirewallTool(ctx); err != nil {
		say("warning: %v", err)
	}
	// A podman restart can break a running job: only when what it runs changed.
	podmanAction := "start"
	if bundle.Current() == "" {
		podmanAction = "restart"
		ver, err := bundle.Install(ctx, m)
		if err != nil {
			return fmt.Errorf("install the container runtime: %w", err)
		}
		if err := bundle.Activate(ver); err != nil {
			return err
		}
		say("container runtime %s installed", ver)
	}
	if err := ensurePolicy(); err != nil {
		return err
	}
	_ = st.Update(func(s *store.State) { s.NodeImage = m.Node.Image })

	if _, err := os.Stat(config.WalletPath); err == nil || !opts.Hold {
		wallet, err := keys.Wallet(config.WalletPath)
		if err != nil {
			return err
		}
		say("node address %s", keys.Address(wallet))
	}
	if err := cliGroup(say); err != nil {
		say("warning: %v", err)
	}
	if opts.Token != "" {
		if err := st.Update(func(s *store.State) { s.ClaimToken = opts.Token }); err != nil {
			return err
		}
		say("claim token saved; the agent links this host to your account on start")
	}

	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := store.WriteFile(config.Path, append(raw, '\n'), 0o644); err != nil {
		return err
	}

	changed, err := service.WriteUnits()
	if err != nil {
		return err
	}
	if changed {
		podmanAction = "restart"
	}
	steps := [][]string{{"daemon-reload"}, {"enable", service.PodmanUnit}, {podmanAction, service.PodmanUnit}}
	if !opts.Hold {
		steps = append(steps, []string{"enable", service.AgentUnit}, []string{"restart", service.AgentUnit})
	}
	for _, args := range steps {
		if err := service.Systemctl(args...); err != nil {
			return err
		}
	}
	if key != "" {
		held = true // an import that fails leaves the agent stopped, not started on a wallet of its own
		return Import(ctx, key, out)
	}
	if opts.Hold {
		say("installed; the agent starts once it has its node key: sudo nosana-host node import KEY")
		return nil
	}
	say("services started")
	fmt.Fprintln(out, "\nDone. Follow the agent with: journalctl -u nosana-host -f")
	return nil
}

// chooseKey is the key file that becomes the node wallet with this install, or "" for the wallet
// the agent has or makes. The key given with --import-key goes first. A node that runs in docker
// on this machine has to move to the agent with its key, since two nodes would share the GPU:
// offered on a terminal when it is the only one, and otherwise the install stops. When none runs,
// the key an earlier node left in ~/.nosana is offered to an agent that has no wallet yet.
func chooseKey(opts Options, found []legacy.Node, earlier string, haveWallet bool, say func(string, ...any)) (string, error) {
	if opts.ImportKey != "" {
		if _, err := keys.Read(opts.ImportKey); err != nil {
			return "", fmt.Errorf("--import-key: %w", err)
		}
		return opts.ImportKey, nil
	}
	if len(found) > 0 {
		if len(found) == 1 && opts.Ask != nil {
			say("node %s runs in docker on this machine (key %s)", found[0].Address, found[0].KeyPath)
			if opts.Ask("Move it to the agent? It keeps its address and market; a job that runs is finished first.") {
				return found[0].KeyPath, nil
			}
		}
		var list strings.Builder
		for _, n := range found {
			fmt.Fprintf(&list, "\n  %s %s", n.Address, n.KeyPath)
		}
		return "", fmt.Errorf("a node runs in docker on this machine (address, key file):%s\nmove it with --import-key <key file>, or stop it before installing", list.String())
	}
	if earlier != "" && !haveWallet && opts.Ask != nil {
		say("found the key of an earlier node: %s", earlier)
		if opts.Ask("Use it for this host? The node keeps its address; otherwise the agent makes a new wallet.") {
			return earlier, nil
		}
	}
	return "", nil
}

// earlierKey is the key file an earlier node left in ~/.nosana of the user who ran sudo, or of
// root; "" when there is none that reads as a key.
func earlierKey() string {
	homes := []string{"/root"}
	if u, err := user.Lookup(os.Getenv("SUDO_USER")); err == nil {
		homes = []string{u.HomeDir, "/root"}
	}
	for _, home := range homes {
		path := filepath.Join(home, ".nosana", "nosana_key.json")
		if _, err := keys.Read(path); err == nil {
			return path
		}
	}
	return ""
}

// Uninstall stops and removes the services; purge also deletes the state after backing the
// wallet up into the home of the user who ran sudo.
func Uninstall(ctx context.Context, purge bool, out io.Writer) error {
	say := sayTo(out)

	_ = service.Systemctl("stop", service.AgentUnit)
	legacy.Undrain(ctx) // a move under way is undone: the old node keeps running
	removeNode(ctx, say)
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
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	reset := exec.CommandContext(ctx, config.RuntimePodman, "--root", config.PodmanRoot, "--runroot", config.PodmanRunRoot, "system", "reset", "--force")
	reset.Env = append(os.Environ(), runtimeEnv()...)
	_ = reset.Run()
	_ = os.RemoveAll(config.StateDir)
	_ = os.RemoveAll(config.RunDir)
	_ = os.Remove(config.Path)
	say("state and config deleted")
	return nil
}

// removeNode stops the agent's node, giving it time to leave the market queue, and removes its
// container.
func removeNode(ctx context.Context, say func(string, ...any)) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	pc := podman.New(config.PodmanSocket)
	if err := pc.StopContainer(ctx, node.ContainerName, 2*time.Minute); err == nil {
		say("node container stopped")
	}
	if err := pc.RemoveContainer(ctx, node.ContainerName); err != nil {
		say("warning: the node container was not removed (%v); remove it with podman", err)
	}
}

// ensurePolicy writes podman's image policy where it looks for it (there is no setting for the
// path) when the host has none.
func ensurePolicy() error {
	const path = "/etc/containers/policy.json"
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(config.RuntimeCurrent, "etc", "policy.json"))
	if err != nil {
		raw = []byte(`{"default":[{"type":"insecureAcceptAnything"}]}` + "\n")
	}
	return store.WriteFile(path, raw, 0o644)
}

// sayTo writes the "• " progress lines of install and uninstall.
func sayTo(out io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) { fmt.Fprintf(out, "• "+format+"\n", args...) }
}

// cliGroup lets the user who ran sudo use the CLI without it: the agent's socket belongs to the
// nosana group.
func cliGroup(say func(string, ...any)) error {
	if out, err := exec.Command("groupadd", "-f", local.Group).CombinedOutput(); err != nil {
		return fmt.Errorf("groupadd %s: %v: %s", local.Group, err, strings.TrimSpace(string(out)))
	}
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return nil
	}
	if out, err := exec.Command("usermod", "-aG", local.Group, name).CombinedOutput(); err != nil {
		return fmt.Errorf("usermod %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	say("%s may use the CLI without sudo after logging in again", name)
	return nil
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
