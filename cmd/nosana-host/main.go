// nosana-host is the Nosana host agent: a root systemd service that runs the Nosana node in its
// own podman and reports to host-manager, plus the commands around it. install and uninstall need
// sudo; the others talk to the running agent and work for root and the nosana group.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/daemon"
	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/install"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/version"
)

const usage = `nosana-host runs the Nosana node on this machine and reports to host-manager.

Usage:
  nosana-host install --control-url URL --release-url URL --network mainnet|devnet [--token T]   install or repair (sudo)
  nosana-host uninstall [--purge]       remove the services; --purge also deletes state after backing the wallet up (sudo)
  nosana-host status                    node address, link, node and update state
  nosana-host logs [node|agent] [-f] [-n N]   the node container's log or the agent's journal
  nosana-host doctor [--fix ID]         run the host checks; --fix applies the fix of one check
  nosana-host daemon                    run the agent (what the service runs)
  nosana-host version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	if (cmd == "install" || cmd == "uninstall") && os.Geteuid() != 0 {
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
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
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
		slog.Error(err.Error())
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
	var s local.Status
	if err := agent(http.MethodGet, "/status", &s); err != nil {
		return err
	}
	linked := "no; run the install command from the console"
	switch {
	case s.Claimed:
		linked = "yes"
	case s.ClaimPending:
		linked = "not yet; the agent is claiming the host"
	}

	n := s.Status.Node
	nodeLine := n.Container + " (" + n.Image + ")"
	if n.State != nil {
		nodeLine += " " + *n.State
	}
	fmt.Printf("agent        %s\nhost-manager %s (%s)\nnode address %s\nlinked       %s\nnode         %s\nchecks       %s\nupdate       %s\n",
		s.Agent, s.ControlURL, s.Network, s.NodeAddress, linked, nodeLine, doctor.Summary(s.Status.Checks), s.Status.Agent.Update.State)
	if s.Update.LastRollback != "" {
		fmt.Printf("rollback     %s\n", s.Update.LastRollback)
	}
	return nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "keep following")
	tail := fs.Int("n", 100, "lines to show first")
	what := "node"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		what, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://agent/logs/%s?tail=%d&follow=%t", what, *tail, *follow), nil)
	resp, err := local.Client().Do(req)
	if err != nil {
		return local.Explain(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return errors.New(strings.TrimSpace(string(msg)))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fix := fs.String("fix", "", "apply the fix of this check")
	_ = fs.Parse(args)
	var checks []api.Check
	if err := agent(http.MethodPost, "/doctor?fix="+*fix, &checks); err != nil {
		return err
	}
	for _, c := range checks {
		line := fmt.Sprintf("%-4s %s", strings.ToUpper(string(c.Status)), c.Title)
		if c.Detail != nil && *c.Detail != "" {
			line += ": " + *c.Detail
		}
		fmt.Println(line)
		if c.Status != api.Ok && c.Fix != nil {
			fmt.Printf("     → %s", *c.Fix)
			if c.Fixable != nil && *c.Fixable {
				fmt.Printf("  (nosana-host doctor --fix %s)", c.Id)
			}
			fmt.Println()
		}
	}
	fmt.Println(doctor.Summary(checks))
	return nil
}

// agent calls the running agent over its socket and decodes the JSON answer into out.
func agent(method, path string, out any) error {
	req, _ := http.NewRequest(method, "http://agent"+path, nil)
	resp, err := local.Client().Do(req)
	if err != nil {
		return local.Explain(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return errors.New(strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
