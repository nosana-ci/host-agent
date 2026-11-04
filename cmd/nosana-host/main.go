// nosana-host is the Nosana host agent: a root systemd service that runs the Nosana node in its
// own podman and reports to host-manager, plus the commands around it. install and uninstall need
// sudo; the others talk to the running agent and work for root and the nosana group.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/daemon"
	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/install"
	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

const usage = `nosana-host runs the Nosana node on this machine and reports to host-manager.

Usage:
  nosana-host install --control-url URL --release-url URL --network mainnet|devnet [--token T]   install or repair (sudo)
  nosana-host uninstall [--purge]       remove the services; --purge also deletes state after backing the wallet up (sudo)
  nosana-host status                    link, wallet, node, GPU, disk, checks and update state
  nosana-host logs [node|agent] [-f] [-n N]   the node container's log or the agent's journal
  nosana-host doctor [--fix ID]         run the host checks; --fix applies the fix of one check
  nosana-host node start|stop|restart   the node container; a stopped node stays stopped until started
  nosana-host update                    install the current release now, also while the node is busy
  nosana-host register [--email E] [--discord D] [--twitter T] [--accept-agreement]   the node's contact details; asks on a terminal
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
	case "node":
		err = cmdNode(os.Args[2:])
	case "register":
		err = cmdRegister(os.Args[2:])
	case "update":
		fmt.Println("checking the release; a download can take a few minutes")
		err = cmdCommand("update")
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

// cmdStatus shows what the console's host page shows, a row per item; a row with an empty label
// continues the one above.
func cmdStatus() error {
	var s local.Status
	err := agent(http.MethodGet, "/status", &s)
	if errors.Is(err, local.ErrNotRunning) {
		statusFromFiles()
	}
	if err != nil {
		return err
	}
	st := s.Status
	row("agent", s.Agent)
	row("host-manager", s.ControlURL+" ("+s.Network+")")
	row("node address", s.NodeAddress)
	row("linked", linked(s.Claimed, s.ClaimPending))
	row("wallet", walletLine(st.Wallet))

	n := st.Node
	node := n.Container + " (" + n.Image + ")"
	if n.State != nil {
		node += ", reports " + *n.State
	}
	row("node", node)
	if n.Desired == api.Stopped {
		row("", "stopped by the owner; nosana-host node start runs it again")
	}
	if n.Restarts > 0 {
		row("", fmt.Sprintf("%d restarts after crashes", n.Restarts))
	}
	if n.LastError != nil {
		row("", "last error: "+*n.LastError)
	}

	row("gpu", gpuLine(st.Gpu))
	runtime := "podman not answering"
	if st.Runtime.PodmanVersion != nil {
		runtime = "podman " + *st.Runtime.PodmanVersion
	}
	row("runtime", runtime)
	row("disk", fmt.Sprintf("%.0f GB free of %.0f GB on %s", st.Disk.FreeGb, st.Disk.TotalGb, st.Disk.Mount))
	row("checks", doctor.Summary(st.Checks))
	u := st.Agent.Update
	update := u.State
	if u.LatestVersion != nil {
		update += ", latest " + *u.LatestVersion
	}
	if u.Detail != nil {
		update += ": " + *u.Detail
	}
	row("update", update)
	if s.LastRollback != "" {
		row("rollback", s.LastRollback)
	}
	return nil
}

func row(label, value string) {
	fmt.Printf("%-12s %s\n", label, value)
}

func walletLine(w *api.WalletStatus) string {
	switch {
	case w == nil || w.Sol == nil && w.Error == nil:
		return "balance not read yet"
	case w.Sol == nil:
		return "balance unknown: " + *w.Error
	}
	line := fmt.Sprintf("%.4f SOL", *w.Sol)
	if *w.Sol < daemon.MinSOL {
		line += fmt.Sprintf("; the node needs %v SOL to start", daemon.MinSOL)
	}
	if w.Error != nil {
		line += " (the last read failed: " + *w.Error + ")"
	}
	return line
}

func gpuLine(g api.GpuStatus) string {
	if !g.Detected {
		return "none detected"
	}
	names := make([]string, 0, len(g.Gpus))
	for _, gpu := range g.Gpus {
		names = append(names, fmt.Sprintf("%s (%d GB)", gpu.Name, gpu.MemoryMb/1024))
	}
	line := strings.Join(names, ", ")
	if g.Driver != nil {
		line += ", driver " + *g.Driver
	}
	return line
}

// statusFromFiles shows, while the agent is down, what the install wrote; the link and the node
// address only for root, as their files are root's.
func statusFromFiles() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Println("nosana-host is not installed")
		return
	}
	row("host-manager", cfg.ControlURL+" ("+cfg.Network+")")
	if os.Geteuid() != 0 {
		row("", "sudo nosana-host status also shows the node address and the link")
		return
	}
	if _, err := os.Stat(config.WalletPath); err == nil { // keys.Wallet would create a missing one
		if w, err := keys.Wallet(config.WalletPath); err == nil {
			row("node address", keys.Address(w))
		}
	}
	if st, err := store.Open(config.StatePath); err == nil {
		s := st.State()
		row("linked", linked(s.Claimed, s.ClaimToken != ""))
		if s.Update.LastRollback != "" {
			row("rollback", s.Update.LastRollback)
		}
	}
}

func linked(claimed, pending bool) string {
	switch {
	case claimed:
		return "yes"
	case pending:
		return "not yet; the agent is claiming the host"
	}
	return "no; run the install command from the console"
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "keep following")
	tail := fs.Int("n", 100, "lines to show first; 0 for all")
	what := "node"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		what, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	body, err := local.Call(ctx, http.MethodGet, fmt.Sprintf("/logs/%s?tail=%d&follow=%t", what, *tail, *follow))
	switch {
	case errors.Is(err, local.ErrNotRunning) && what == "agent": // the journal says why it is down
		err = service.Journal(ctx, *tail, *follow, os.Stdout)
	case err != nil:
		return err
	default:
		defer body.Close()
		_, err = io.Copy(os.Stdout, body)
	}
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
	if err := agent(http.MethodPost, "/doctor?"+url.Values{"fix": {*fix}}.Encode(), &checks); err != nil {
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

// cmdNode runs the console's start-node, stop-node or restart-node.
func cmdNode(args []string) error {
	if len(args) != 1 || (args[0] != "start" && args[0] != "stop" && args[0] != "restart") {
		return errors.New("usage: nosana-host node start|stop|restart")
	}
	return cmdCommand(args[0] + "-node")
}

// agreementURL is the Participation Agreement the node asks a host to accept on its first start.
const agreementURL = "https://drive.google.com/file/d/1dFWCT5Zon08pCPrftdxB9ByvbuDafTwy/view"

// cmdRegister registers the node with the contact details the node would ask on its terminal and
// the owner's acceptance of the Participation Agreement. Flags it does not get it asks for when
// stdin is a terminal.
func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	email := fs.String("email", "", "where Nosana can reach you about this node")
	discord := fs.String("discord", "", "your Discord username (optional)")
	twitter := fs.String("twitter", "", "your Twitter username (optional)")
	accept := fs.Bool("accept-agreement", false, "you read the Participation Agreement and agree to its terms")
	_ = fs.Parse(args)
	in := bufio.NewReader(os.Stdin)
	if isTerminal(os.Stdin) {
		if *email == "" {
			*email = ask(in, "Email: ")
			*discord = ask(in, "Discord username (optional): ")
			*twitter = ask(in, "Twitter username (optional): ")
		}
		if !*accept {
			fmt.Println("Participation Agreement: " + agreementURL)
			*accept = strings.EqualFold(ask(in, "Have you read it, and do you agree to its terms? [y/N] "), "y")
		}
	}
	if *email == "" {
		return errors.New("usage: nosana-host register --email ADDRESS [--discord NAME] [--twitter NAME] --accept-agreement")
	}
	if !*accept {
		return errors.New("the Participation Agreement has to be accepted: " + agreementURL)
	}
	q := url.Values{"email": {*email}, "discord": {*discord}, "twitter": {*twitter}, "accept": {"1"}}
	return result("/register?" + q.Encode())
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func ask(in *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// cmdCommand runs one of the console's commands; the agent queues it with the console's.
func cmdCommand(verb string) error {
	return result("/commands/" + verb)
}

// result posts to the agent and prints the message of the command result it answers with.
func result(path string) error {
	var r api.CommandResult
	if err := agent(http.MethodPost, path, &r); err != nil {
		return err
	}
	if !r.Ok {
		return errors.New(r.Message)
	}
	fmt.Println(r.Message)
	return nil
}

// agent calls the running agent and decodes its JSON answer into out.
func agent(method, path string, out any) error {
	body, err := local.Call(context.Background(), method, path)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(out)
}
