// nosana-host is the Nosana host agent: a root systemd service that runs the Nosana node in its
// own podman and reports to host-manager, plus the install and status commands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/daemon"
	"github.com/nosana-ci/host-agent/internal/install"
	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

const usage = `nosana-host runs the Nosana node on this machine and reports to host-manager.

Usage:
  nosana-host install --control-url URL --release-url URL --network mainnet|devnet [--token T]   install or repair (sudo)
  nosana-host uninstall [--purge]       remove the services; --purge also deletes state after backing the wallet up
  nosana-host status                    what the agent knows: node address, link, node container (sudo)
  nosana-host daemon                    run the agent (what the service runs)
  nosana-host version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	if (cmd == "install" || cmd == "uninstall" || cmd == "status") && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "nosana-host: run with sudo")
		os.Exit(1)
	}
	var err error
	switch cmd {
	case "install":
		err = cmdInstall(os.Args[2:])
	case "uninstall":
		fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also delete state and config")
		_ = fs.Parse(os.Args[2:])
		err = install.Uninstall(context.Background(), *purge, os.Stdout)
	case "status":
		err = cmdStatus()
	case "daemon":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		err = daemon.Run(ctx)
	case "version", "--version", "-v":
		fmt.Printf("nosana-host %s %s\n", version.Version, version.Commit)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nosana-host:", err)
		os.Exit(1)
	}
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	var cfg config.Config
	fs.StringVar(&cfg.ControlURL, "control-url", "", "host-manager URL")
	fs.StringVar(&cfg.ReleaseURL, "release-url", "", "where stable.json and stable.json.sig live")
	fs.StringVar(&cfg.Network, "network", "", "mainnet or devnet")
	token := fs.String("token", "", "claim token from the console's install command")
	_ = fs.Parse(args)
	return install.Run(context.Background(), cfg, *token, os.Stdout)

}

func cmdStatus() error {
	cfg, err := config.Load()

	if err != nil {
		return errors.New("nosana-host is not installed")
	}
	if _, err := os.Stat(config.WalletPath); err != nil {
		return errors.New("no node wallet yet; the install did not finish")
	}
	st, err := store.Open(config.StatePath)
	if err != nil {
		return err
	}
	wallet, err := keys.Wallet(config.WalletPath)
	if err != nil {
		return err
	}
	s := st.State()
	fmt.Printf("agent       %s\nhost-manager %s (%s)\nnode address %s\n", version.Version, cfg.ControlURL, cfg.Network, keys.Address(wallet))
	if s.Claimed {
		fmt.Println("linked      yes")
	} else {
		fmt.Println("linked      no; run the install command from the console")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctr, err := podman.New(config.PodmanSocket).InspectContainer(ctx, node.ContainerName)
	switch {
	case errors.Is(err, podman.ErrNotFound):
		fmt.Println("node        no container yet")
	case err != nil:
		fmt.Printf("node        podman not reachable (%v)\n", err)
	default:
		fmt.Printf("node        %s (%s)\n", ctr.State.Status, ctr.Config.Labels[node.LabelImage])
	}
	if s.Update.LastRollback != "" || s.Update.Pending != nil {
		raw, _ := json.Marshal(s.Update)
		fmt.Printf("update      %s\n", raw)
	}
	return nil
}
