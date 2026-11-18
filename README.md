# nosana-host

The Nosana host agent. One command installs it on a GPU machine; from then on it runs the Nosana
node in its own container runtime, keeps it healthy, reports to host-manager and updates itself.
The owner watches and controls the host from the console at https://host.nosana.com.

## Install

In the console, add a host and copy the install command. It is:

```sh
curl -fsSL https://host.nosana.com/install.sh | sudo sh -s -- --token <token>
```

The token links the machine to your account; it is valid for one hour and one host. The script
reads the current release manifest, downloads the agent and checks its sha256, and runs
`nosana-host install`, which:

1. puts the binary in `/var/lib/nosana/agent/bin/` and links `/usr/local/bin/nosana-host` to it;
2. downloads the podman runtime bundle the release names into `/var/lib/nosana/runtime/<version>`
   (hosts never install podman; podman's firewall rules go through `iptables` when the host has
   it, so they live next to docker's and ufw's, otherwise through `nft`; `nftables` is installed
   when the host has neither);
3. creates the node wallet (`/var/lib/nosana/node/nosana_key.json`) and the `nosana` group for the CLI;
4. writes `/etc/nosana/host.json` (host-manager URL, network) and the two systemd units,
   `nosana-podman.service` and `nosana-host.service`, and starts them.

Running the command again repairs the install and keeps the wallet, the keys and the link.

`sudo nosana-host node import KEY` makes a key file the node wallet; another wallet the agent had
is backed up into the home of the user who ran sudo. A node that runs in docker with that key, from
`start.sh` or as a `nosana/nosana-node` container, is taken over and keeps its address and market:
the agent pulls the node image while the old node runs, asks the old node to stop before its next
job (a `drain` file in its config folder; an older node is stopped the moment it is seen between
jobs), stops its container (only one whose `/root/.nosana` holds this key) and drops its restart
policy, and starts its own node once the old one no longer answers at the node's public domain. From then on it
stops the old node whenever it runs again (the market queue holds one node per key).
`sudo nosana-host uninstall` and the `start.sh` command undo it. `nosana-host install` finds such a
node itself and, on a terminal, offers to move it; `--import-key KEY` moves it without asking, and
an install with a node in docker that is not moved stops, since two nodes would share the GPU. When
none runs, the key an earlier node left in `~/.nosana` (of the user who ran sudo, or of root) is
offered to an agent that has no wallet yet. With a key, the agent is installed held (`--hold`:
stopped and disabled, without a wallet, so it does not make one and link that to the account), and
the key is then imported. The install script only passes its arguments on.

Supported: Ubuntu 22.04/24.04/26.04 and Debian 12/13 on x86-64, with the NVIDIA driver (R550 or
newer), nvidia-container-toolkit and a CDI spec for the GPU; the console's health panel shows
what is missing.

```sh
nosana-host status                 # what the console's host page shows: network, link, wallet (SOL and NOS), node, GPU, disk, checks, update
nosana-host logs [node|agent|podman|job ID] -f   # the node container's log, a journal, or a job's container
nosana-host doctor [--fix ID|all] [--yes]   # the host checks; --fix applies the fix of one check, or every fix there is
nosana-host node start|stop|restart   # the node container, like the console's buttons
sudo nosana-host node attach       # the node's terminal with its keys (s: shut down after the job), once the node runs; Ctrl+C leaves, the node keeps running
nosana-host update [--check]       # install the current release now; --check only looks
nosana-host config [set auto-update on|off]   # the settings and the install's values
nosana-host register [--show]      # the node's email (and Discord, Twitter) and the Participation Agreement; shows and changes them later
nosana-host airdrop                # devnet only: ask the faucet for SOL
nosana-host runtime [df|prune|restart|reset]   # podman's storage: what it holds, free what nothing uses, restart podman, wipe it
nosana-host reboot [--cancel]      # reboot the machine once the node is between jobs
sudo nosana-host uninstall [--purge]   # --purge deletes state too, after backing up the wallet into your home
```

Everything but install and uninstall talks to the running agent over `/run/nosana/cli.sock`, which belongs
to root and the `nosana` group; the installer adds the user who ran it to that group (log in again
once), so only install and uninstall need sudo. While the agent is down, `status` shows what the
install wrote instead (with sudo also the node address and the link), `logs agent` and `logs podman`
read the journal directly, and `logs node` reads podman's socket (root's).

Install script: `host-console/static/install.sh`.

## What the agent does

- **Node.** Creates the `NOSANA_GATEWAY` network and the `nosana-node` container from the image the
  release names (`start --network <network>`, the node directory at `/root/.nosana`, the podman
  socket next to it, port 5001 on `127.0.0.1:15001` for `/node/info`), restarts it when it asks
  (exit 129) or with a backoff when it crashes, keeps it stopped when the owner said so, and swaps
  the image when the release changes it and the node may be interrupted (see Updates).
- **The node's terminal.** The container has a terminal, as under start.sh, so the node draws its
  spinners and progress bars and reads keys. The agent sizes the terminal (120x40) before the
  node's process starts, since on a terminal without a size the spinners flood the log with cursor
  moves, and a CLI that watches sends its own size. The container's log is therefore everything
  the node drew, every spinner frame included: some 3 to 4 kB a second, capped at 200 MB (half a
  day to a day), after which podman starts the file over. `logs node` writes that log as the lines
  the node left (a spinner or bar is its last drawing; bars redrawn together, as the layers of a
  pull, repeat); `logs node -f` on a terminal then shows what the node draws as it draws it.
  `sudo nosana-host node attach` is podman's own attach to that terminal, with the keyboard
  (`s` while a job runs: shut down after it; the node's own questions); Ctrl+C is its detach
  key, so it leaves and is not passed on, as it would stop the node. It needs sudo because
  podman's socket is root's. A node that is on its way is waited for, with what its start waits
  for as the agent knows it (a takeover from the old node, the image pull, a restart after a
  crash), so install.sh ends there; a node its owner stopped is not. A
  container from before this has no terminal: it is recreated once the node may be interrupted,
  or at once by `update`.
- **Heartbeat.** Every 15 s: agent version, the commands it accepts and its settings, update state,
  node state, GPU, disk, the host checks, the runtime, the network and the node wallet's SOL and NOS
  balances (read every minute from the Nosana kit's default RPC: Nosana's Ironforge endpoint on
  mainnet, the public one on devnet), signed with the node wallet like every node request.
  Commands come back in the
  answer and through a 25 s long-poll; each is answered done or failed with a message.
- **Commands.** `restart-node`, `stop-node`, `start-node`, `run-doctor`, `fix-check` (podman
  service restart, firewall tool install, nvidia-persistenced, NTP), `check-update`, `update` (also while
  the node is busy), `set-config` (`auto-update` on or off), `reboot-when-idle` and `cancel-reboot`,
  `prune-images`, `restart-runtime`, `reset-storage` and, on devnet, `request-airdrop`.
  The heartbeat lists them, so the console shows what this agent can do. The CLI's
  `node start|stop|restart`, `update`, `config set` and `airdrop` are the same commands: they wait
  in the same queue and run one at a time with the console's.
- **Node state.** Every 5 s the supervisor reads the node's own `/node/info` on its local port:
  the state category, the exact status with its details, and the recent state changes. What the
  node does now (a benchmark, the place in the market queue, the job it runs, a countdown to a
  restart) goes into the heartbeat as `node.activity`, a node waiting at its sign-up questions as
  `node.prompt`, and the state changes worth knowing (SOL and registration waits, benchmark
  verdicts, market and queue, jobs, restarts) become events of type node. Needs a node image with
  the richer `/node/info` (node MR !91); the agent does not read the node's log.
- **Reach.** Every 2 minutes the agent asks the node's public domain
  (`https://<address>.node.k8s.<prd|dev>.nos.ci/node/info`) whether it answers for this node, the
  way jobs reach it; the result is `node.reach` and the `node-domain` check.
- **Runtime storage.** Every minute podman's `system/df` is summed into `runtime.storage` (used,
  what prune would free, the images and containers). `prune-images` removes containers that are
  not running (the node's excepted) and images nothing uses; `reset-storage` stops the node, wipes
  everything and starts it again (its wallet and state live outside podman's storage).
- **Reboot.** `reboot-when-idle` is kept in `state.json`; the machine reboots once the node may be
  interrupted (the same rule as updates), and the heartbeat says the reboot waits
  (`system.rebootPending`). `system.rebootRequired` mirrors `/run/reboot-required`.
- **Settings.** `/etc/nosana/host.json` holds the install's values (host-manager URL, release URL,
  network) and `autoUpdate`. With auto-update off the agent still reads the release every 10
  minutes and reports a newer one (update state `available`), but installs it only on `update`.
- **Registration.** `register` reads the current contact details from host-manager
  (`GET /agent/registration`) and writes them back (`PUT`), signed with the node key, so the owner
  can change a Discord handle later from the machine or from the console.
- **Events.** What the agent does by itself or for its CLI goes to the console's Activity: its
  start, an automatic update and whether it was kept or rolled back, a runtime switch, a node crash,
  a new node container or image, and every command typed on the machine with its result. They wait
  in `state.json` (at most 200) until host-manager took them, so the events around an agent switch
  survive the exec.

- **Updates.** Every 10 minutes the agent reads `stable.json` from the release URL, verifies its signature with
  the release key built into it, and installs a newer agent right away: download, sha256, a
  `version` run, then `bin/current` is relinked and the process execs the new binary in place; the
  node keeps running. A new runtime bundle (switched back when podman does not come up within 45 s),
  a runtime configuration that no longer fits the host (its firewall driver) and a new node image
  stop the node, so they wait: always for a job or a benchmark, at most 24 h
  for a node in the queue, joining a market, or running without answering `/node/info`, and not at
  all for a node waiting for SOL or not running. `update` from the console applies them at once. The new version is kept once it has been up for two
  minutes; three failed starts before that roll back to the previous binary and block that version
  on this host.
- **Offline.** When host-manager is away the node keeps running and heartbeats retry with a backoff
  up to five minutes. A host that was unlinked in the console answers `unknown host`; the agent then
  waits for a new install command.

The machine makes outbound calls only; nothing listens on a port.

## Development

Go 1.27. `make test`, `make lint` (golangci-lint), `make build VERSION=x.y.z RELEASE_PUBLIC_KEY=...`.
A build without a release key cannot install or update.

The wire types in `internal/api/types.gen.go` are generated from host-manager's OpenAPI document:
`make api-types HOST_MANAGER=http://localhost:3005` (needs `jq`; the generator is a Go tool
dependency).

Layout: `cmd/nosana-host` (CLI), `cmd/nosana-host-publish` (release tool), `internal/manifest` (the release manifest), `internal/local` (the CLI socket), `internal/daemon`
(wiring and the command table), `node` (supervisor), `control` (host-manager client and request
signing), `updater` (self-update, rollback, runtime switch), `bundle` (the podman bundle),
`doctor` (checks, fixes, GPU detection), `install`, `service` (systemd units), `store` (atomic files,
state), `keys`, `config`.

## Releases

The agent is open source on GitHub (`github.com/nosana-ci/host-agent`), and releases are GitHub
Releases there. A tag `vX.Y.Z` runs the release pipeline:
`make build`, `make runtime` (the podman bundle from the GPG-verified podman-static release),
`nosana-host-publish` (writes the manifest and signs it with `RELEASE_SIGNING_KEY`), then
`scripts/github-release.sh` publishes the binary, the runtime bundle, `stable.json` and
`stable.json.sig` as the release's assets. Agents and `install.sh` read
`https://github.com/nosana-ci/host-agent/releases/latest/download/stable.json`, so the newest full
release is the stable one. To halt a release, mark it as a pre-release on GitHub: "latest" moves
back to the previous one, and agents never install a version lower than their own.

`go run ./cmd/nosana-host-publish keygen` makes a key pair: the seed goes into CI
(`RELEASE_SIGNING_KEY`), the public key into the build (`RELEASE_PUBLIC_KEY`).

Local: serve the artifacts and the manifest from a directory, then install from it:

```sh
make build VERSION=0.1.0 RELEASE_PUBLIC_KEY=... && make runtime
(cd bin && python3 -m http.server 8000) &
RELEASE_SIGNING_KEY=... go run ./cmd/nosana-host-publish --version 0.1.0 \
  --agent http://127.0.0.1:8000/nosana-host --agent-sha "$(sha256sum bin/nosana-host | cut -d' ' -f1)" \
  --runtime-version 5.8.8 --runtime http://127.0.0.1:8000/nosana-runtime-5.8.8-linux-amd64.tar.gz \
  --runtime-sha "$(sha256sum bin/nosana-runtime-5.8.8-linux-amd64.tar.gz | cut -d' ' -f1)" \
  --node-image docker.io/nosana/nosana-node:latest --out bin
sudo sh ../host-console-v1/static/install.sh --host-manager http://localhost:3005 \
  --release-url http://127.0.0.1:8000 --network devnet --token <token>
```
