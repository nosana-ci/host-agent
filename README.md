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
   (hosts never install podman; `nftables` is installed when the host has no firewall tool);
3. creates the node wallet (`/var/lib/nosana/node/nosana_key.json`);
4. writes `/etc/nosana/host.json` (host-manager URL, network) and the two systemd units,
   `nosana-podman.service` and `nosana-host.service`, and starts them.

Running the command again repairs the install and keeps the wallet, the keys and the link.
Supported: Ubuntu 22.04/24.04/26.04 and Debian 12/13 on x86-64, with the NVIDIA driver (R550 or
newer), nvidia-container-toolkit and a CDI spec for the GPU; the console's health panel shows
what is missing.

```sh
sudo nosana-host status            # node address, link, node container
journalctl -u nosana-host -f       # the agent's log
sudo nosana-host uninstall [--purge]   # --purge deletes state too, after backing up the wallet into your home
```

Install script: `host-console/static/install.sh`.

## What the agent does

- **Node.** Creates the `NOSANA_GATEWAY` network and the `nosana-node` container from the image the
  release names (`start --network <network>`, the node directory at `/root/.nosana`, the podman
  socket next to it, port 5001 on `127.0.0.1:15001` for `/node/info`), restarts it when it asks
  (exit 129) or with a backoff when it crashes, keeps it stopped when the owner said so, and swaps
  the image when the release changes it and the node may be interrupted (see Updates).
- **Heartbeat.** Every 15 s: agent version and the commands it accepts, update state, node state,
  GPU, disk, the host checks, the runtime and the node wallet's SOL balance (read every minute from the
  Nosana kit's default RPC: Nosana's Ironforge endpoint on mainnet, the public one on devnet), signed with the node wallet like every node request.
  Commands come back in the
  answer and through a 25 s long-poll; each is answered done or failed with a message.
- **Commands.** `restart-node`, `stop-node`, `start-node`, `run-doctor`, `fix-check` (podman
  service restart, nftables install, nvidia-persistenced), `update` (also while the node is busy).
  The heartbeat lists them, so the console shows what this agent can do.

- **Updates.** Every 10 minutes the agent reads `stable.json` from the release URL, verifies its signature with
  the release key built into it, and installs a newer agent right away: download, sha256, a
  `version` run, then `bin/current` is relinked and the process execs the new binary in place; the
  node keeps running. A new runtime bundle (switched back when podman does not come up within 45 s)
  and a new node image stop the node, so they wait: always for a job or a benchmark, at most 24 h
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

Layout: `cmd/nosana-host` (CLI), `cmd/nosana-host-publish` (release tool), `internal/manifest` (the release manifest), `internal/daemon`
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
