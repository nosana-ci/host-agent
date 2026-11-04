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
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/config"
	"github.com/nosana-ci/host-agent/internal/daemon"
	"github.com/nosana-ci/host-agent/internal/doctor"
	"github.com/nosana-ci/host-agent/internal/install"
	"github.com/nosana-ci/host-agent/internal/keys"
	"github.com/nosana-ci/host-agent/internal/local"
	"github.com/nosana-ci/host-agent/internal/node"
	"github.com/nosana-ci/host-agent/internal/podman"
	"github.com/nosana-ci/host-agent/internal/service"
	"github.com/nosana-ci/host-agent/internal/store"
	"github.com/nosana-ci/host-agent/internal/version"
)

const usage = `nosana-host runs the Nosana node on this machine and reports to host-manager.

Usage:
  nosana-host install --control-url URL --release-url URL --network mainnet|devnet [--token T]   install or repair (sudo)
  nosana-host uninstall [--purge]       remove the services; --purge also deletes state after backing the wallet up (sudo)
  nosana-host status                    link, wallet, node, GPU, disk, checks and update state
  nosana-host logs [node|agent|podman|job ID] [-f] [-n N]   the node container's log, a journal, or a job's container
  nosana-host doctor [--fix ID|all] [--yes]   run the host checks; --fix applies the fix of one check, or of every check that has one
  nosana-host node start|stop|restart   the node container; a stopped node stays stopped until started
  nosana-host update [--check]          install the current release now, also while the node is busy; --check only looks
  nosana-host config [get KEY | set KEY on|off]   the settings (auto-update) and the install's values
  nosana-host register [--show] [--email E] [--discord D] [--twitter T] [--accept-agreement]   the node's contact details; asks on a terminal
  nosana-host airdrop                   ask the devnet faucet for SOL (devnet only)
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
		err = cmdUpdate(os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "airdrop":
		err = cmdCommand("request-airdrop")
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
	err := agent(http.MethodGet, "/status", nil, &s)
	if errors.Is(err, local.ErrNotRunning) {
		statusFromFiles()
	}
	if err != nil {
		return err
	}
	st := s.Status
	row("agent", s.Agent)
	row("network", s.Network)
	row("host-manager", s.ControlURL)
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
	row("update", updateLine(st.Agent))
	if s.LastRollback != "" {
		row("rollback", s.LastRollback)
	}
	return nil
}

func updateLine(a api.AgentInfo) string {
	u := a.Update
	line := u.State
	if u.LatestVersion != nil {
		line += ", newest " + *u.LatestVersion
	}
	if u.CheckedAt != nil {
		line += ", checked " + ago(*u.CheckedAt)
	}
	if u.Detail != nil {
		line += ": " + *u.Detail
	}
	if a.Settings != nil && !a.Settings.AutoUpdate {
		line += " (auto-update off)"
	}
	return line
}

// ago says how long ago t was, in the coarse way a status line needs.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
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
	if w.Nos != nil {
		line += fmt.Sprintf(", %.2f NOS", *w.Nos)
	}
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
	row("network", cfg.Network)
	row("host-manager", cfg.ControlURL)
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

// cmdLogs streams a log through the agent. While the agent is down, a journal is read directly,
// and a container's log from podman's socket, which is root's.
func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "keep following")
	tail := fs.Int("n", 100, "lines to show first; 0 for all")
	what, path := "node", "/logs/node"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		what, args = args[0], args[1:]
		path = "/logs/" + what
		if what == "job" {
			if len(args) == 0 || strings.HasPrefix(args[0], "-") {
				return errors.New("usage: nosana-host logs job ID")
			}
			path, args = "/logs/job/"+url.PathEscape(args[0]), args[1:]
		}
	}
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	body, err := local.Call(ctx, http.MethodGet, fmt.Sprintf("%s?tail=%d&follow=%t", path, *tail, *follow), nil)
	switch {
	case errors.Is(err, local.ErrNotRunning) && what == "agent": // the journal says why it is down
		err = service.Journal(ctx, service.AgentUnit, *tail, *follow, os.Stdout)
	case errors.Is(err, local.ErrNotRunning) && what == "podman":
		err = service.Journal(ctx, service.PodmanUnit, *tail, *follow, os.Stdout)
	case errors.Is(err, local.ErrNotRunning) && what == "node":
		err = podman.New(config.PodmanSocket).Logs(ctx, node.ContainerName, *tail, *follow, os.Stdout)
		if errors.Is(err, os.ErrPermission) {
			err = errors.New("the agent is not running; podman's socket is root's: sudo nosana-host logs node")
		}
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

// cmdDoctor runs the checks; --fix ID applies one fix, --fix all every fix there is, after a
// question on a terminal (--yes answers it).
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fix := fs.String("fix", "", "apply the fix of this check, or of all")
	yes := fs.Bool("yes", false, "apply the fixes without asking")
	_ = fs.Parse(args)
	var checks []api.Check
	if *fix == "all" {
		if err := agent(http.MethodPost, "/doctor", nil, &checks); err != nil {
			return err
		}
		printChecks(checks)
		ids := fixable(checks)
		if len(ids) == 0 {
			fmt.Println("nothing to fix")
			return nil
		}
		fmt.Printf("%d fixes to apply: %s\n", len(ids), strings.Join(ids, ", "))
		if !*yes {
			if !isTerminal(os.Stdin) {
				return errors.New("add --yes to apply them without a terminal")
			}
			if !strings.EqualFold(ask(bufio.NewReader(os.Stdin), "Apply them? [y/N] "), "y") {
				return nil
			}
		}
		for _, id := range ids {
			fmt.Println("fixing " + id)
			if err := agent(http.MethodPost, "/doctor?"+url.Values{"fix": {id}}.Encode(), nil, &checks); err != nil {
				fmt.Println("  " + err.Error())
			}
		}
		fmt.Println()
	} else if err := agent(http.MethodPost, "/doctor?"+url.Values{"fix": {*fix}}.Encode(), nil, &checks); err != nil {
		return err
	}
	printChecks(checks)
	return nil
}

func printChecks(checks []api.Check) {
	for _, c := range checks {
		line := fmt.Sprintf("%-4s %s", strings.ToUpper(string(c.Status)), c.Title)
		if c.Detail != nil && *c.Detail != "" {
			line += ": " + *c.Detail
		}
		fmt.Println(line)
		if c.Status != api.CheckStatusOk && c.Fix != nil {
			fmt.Printf("     → %s", *c.Fix)
			if c.Fixable != nil && *c.Fixable {
				fmt.Printf("  (nosana-host doctor --fix %s)", c.Id)
			}
			fmt.Println()
		}
	}
	fmt.Println(doctor.Summary(checks))
}

func fixable(checks []api.Check) []string {
	var ids []string
	for _, c := range checks {
		if c.Status != api.CheckStatusOk && c.Fixable != nil && *c.Fixable {
			ids = append(ids, c.Id)
		}
	}
	return ids
}

// cmdUpdate installs the current release, or with --check only says whether there is one.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only check for a new release")
	_ = fs.Parse(args)
	if *check {
		return cmdCommand("check-update")
	}
	fmt.Println("checking the release; a download can take a few minutes")
	return cmdCommand("update")
}

// cmdConfig shows the config, one value, or changes a setting through the agent.
func cmdConfig(args []string) error {
	var c local.Config
	if err := agent(http.MethodGet, "/config", nil, &c); err != nil {
		return err
	}
	values := map[string]string{"control-url": c.ControlURL, "release-url": c.ReleaseURL, "network": c.Network, "auto-update": onOff(c.AutoUpdate)}
	switch {
	case len(args) == 0:
		for _, key := range []string{"auto-update", "network", "control-url", "release-url"} {
			row(key, values[key])
		}
		fmt.Println("\nnosana-host config set auto-update on|off changes it; the others are set by install")
		return nil
	case args[0] == "get" && len(args) == 2:
		v, ok := values[args[1]]
		if !ok {
			return fmt.Errorf("unknown key %q", args[1])
		}
		fmt.Println(v)
		return nil
	case args[0] == "set" && len(args) == 3:
		return result("/commands/set-config", map[string]string{"key": args[1], "value": args[2]})
	}
	return errors.New("usage: nosana-host config [get KEY | set KEY VALUE]")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
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
// the owner's acceptance of the Participation Agreement, or changes the details of a registered
// node. On a terminal it asks for what the flags did not give, with the current values as the
// defaults; a flag not given keeps the current value.
func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	show := fs.Bool("show", false, "only show the current details")
	email := fs.String("email", "", "where Nosana can reach you about this node")
	discord := fs.String("discord", "", "your Discord username (optional)")
	twitter := fs.String("twitter", "", "your Twitter username (optional)")
	accept := fs.Bool("accept-agreement", false, "you read the Participation Agreement and agree to its terms")
	_ = fs.Parse(args)
	var current api.AgentRegistration
	if err := agent(http.MethodGet, "/registration", nil, &current); err != nil {
		return err
	}
	reg := current.Registration
	if reg == nil {
		reg = &api.Registration{}
		fmt.Println("the node is not registered yet")
	} else {
		row("email", reg.Email)
		row("discord", orNone(reg.Discord))
		row("twitter", orNone(reg.Twitter))
	}
	if *show {
		return nil
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	registered := reg.Email != ""
	in := bufio.NewReader(os.Stdin)
	if isTerminal(os.Stdin) && !given["email"] {
		if registered {
			fmt.Println("\nEnter keeps a value; a dash clears it")
		}
		*email = askDefault(in, "Email", reg.Email)
		if !given["discord"] {
			*discord = askDefault(in, "Discord username (optional)", orEmpty(reg.Discord))
		}
		if !given["twitter"] {
			*twitter = askDefault(in, "Twitter username (optional)", orEmpty(reg.Twitter))
		}
	} else {
		if !given["email"] {
			*email = reg.Email
		}
		if !given["discord"] {
			*discord = orEmpty(reg.Discord)
		}
		if !given["twitter"] {
			*twitter = orEmpty(reg.Twitter)
		}
	}
	if !registered && !*accept && isTerminal(os.Stdin) {
		fmt.Println("Participation Agreement: " + agreementURL)
		*accept = strings.EqualFold(ask(in, "Have you read it, and do you agree to its terms? [y/N] "), "y")
	}
	if *email == "" {
		return errors.New("usage: nosana-host register --email ADDRESS [--discord NAME] [--twitter NAME] --accept-agreement")
	}
	if !registered && !*accept {
		return errors.New("the Participation Agreement has to be accepted: " + agreementURL)
	}
	q := url.Values{"email": {*email}, "discord": {*discord}, "twitter": {*twitter}, "accept": {"1"}}
	return result("/register?"+q.Encode(), nil)
}

func orNone(s *string) string {
	if s == nil || *s == "" {
		return "none"
	}
	return *s
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// askDefault asks with the current value as the default; Enter keeps it and a dash clears it.
func askDefault(in *bufio.Reader, prompt, current string) string {
	if current != "" {
		prompt += " [" + current + "]"
	}
	switch answer := ask(in, prompt+": "); answer {
	case "":
		return current
	case "-":
		return ""
	default:
		return answer
	}
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
	return result("/commands/"+verb, nil)
}

// result posts to the agent and prints the message of the command result it answers with.
func result(path string, body any) error {
	var r api.CommandResult
	if err := agent(http.MethodPost, path, body, &r); err != nil {
		return err
	}
	if !r.Ok {
		return errors.New(r.Message)
	}
	fmt.Println(r.Message)
	return nil
}

// agent calls the running agent, with body as JSON when given, and decodes its JSON answer into
// out when given.
func agent(method, path string, body, out any) error {
	answer, err := local.Call(context.Background(), method, path, body)
	if err != nil {
		return err
	}
	defer answer.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(answer).Decode(out)
}
