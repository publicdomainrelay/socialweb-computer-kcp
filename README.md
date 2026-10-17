# socialweb-computer-kcp

socialweb-computer-kcp serves virtual Kubernetes clusters. A tenant cluster's k3s nodes are virtual machines rented on the atproto compute market, and the nodes scale to zero while the control plane keeps serving. One atproto account can own several clusters.

One binary, `socialweb-computer`, runs every part of the software. Each running application is a separate instance with its own kcp control plane, a manifest set, and a profile: a requester (`run request`), a bidder, the gateway, a market network, or a local development instance. Application state lives in that instance's kcp as objects such as `ComputeVM`, `ComputeContract`, `NodeClaim`, `DenoPod`, and `HostProcess`.

- Design and decisions: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- Working rules for this repository: [`AGENTS.md`](AGENTS.md)
- Plans and their status: [`docs/plans/`](docs/plans/), [`NEXT.yaml`](NEXT.yaml)

## Prerequisites

- A 64-bit Linux host with `/dev/kvm`, systemd 253 or newer (Ubuntu 24.04 ships 255), cgroup v2 and `/dev/net/tun`.
- To run VMs on the host: an account whose `/etc/subuid` and `/etc/subgid` each reserve one range of `N * 65536` ids, which `socialweb-computer install bidder --vm-slots N` writes (default 256). One running VM holds one slot of 65536 ids, so `N` is how many VMs the account runs at once. The account hands its slots out under one lock, `/run/user/<uid>/socialweb-vmslots.lock`.
- To rent a VM: a bidder to rent from, and an atproto account to post the records. A single-host market network runs its bidders as children of the same instance, so run every live command as the VM account (`test-0001`): the VM units, their slots and their `/dev/kvm` access belong to that account.
- To run a release binary: `curl` and `sha256sum` (coreutils). The binary embeds every asset it runs, so it needs neither this checkout nor Go and Deno.
- To build from source or run the scripts: Go and Deno on `PATH`, and this repository checked out, since the scripts run from their own checkout.

### Prepare a bare host

A bare Ubuntu 24.04 or Fedora host needs these OS packages and settings for the market network and `run request` below. `dev up` for a network with a bidder checks the host first, and `run request` checks for `ssh`; each stops at once, naming what is missing and its fix; `socialweb-computer check-host -role bidder -state-dir <dir>` lists every check.

| Need | Ubuntu / Debian package | Fedora / RHEL package |
|---|---|---|
| `newuidmap` and `newgidmap`, so each VM runs in its own user namespace | `uidmap` | `shadow-utils` |
| `ssh` and `ssh-keygen`, which `run request` runs | `openssh-client` | `openssh-clients` |
| `curl`, to install the binary and fetch the guest image | `curl` | `curl` |
| `/usr/share/zoneinfo`, for the embedded postgres of the gateway | `tzdata` | `tzdata` |
| `jq`, used by the examples below (optional) | `jq` | `jq` |

As root, once:

```bash
apt-get install -y uidmap openssh-client curl tzdata jq
useradd -m -s /bin/bash -G kvm -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 test-0001
usermod --add-subuids 100000-1148575 --add-subgids 100000-1148575 test-0001
loginctl enable-linger test-0001
sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
echo kernel.apparmor_restrict_unprivileged_userns=0 > /etc/sysctl.d/60-socialweb.conf
```

- `-K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0` stops `useradd` from adding its own 65536-id range; the account must have exactly one range in each file. `100000-1148575` is 16 VM slots.
- Linger keeps the account's `systemd --user` manager running. Log in as the account with `ssh test-0001@<host>` or `machinectl shell test-0001@`; `sudo -u` and `runuser` do not create the `/run/user/<uid>` session that `XDG_RUNTIME_DIR` names.
- The `sysctl` lets the embedded pasta create a user namespace on Ubuntu 24.04 and newer, where AppArmor restricts it. On a host that has no such sysctl, skip those two lines. A production bidder loads an AppArmor profile through `install bidder` instead.
- `/dev/kvm` is read-write for the `kvm` group; `/dev/net/tun` is open on current kernels, otherwise run `modprobe tun`.
- The account's cgroup delegation (`cpu memory pids` in `user@<uid>.service`) is the default on Ubuntu 24.04; `dev up` names it if the host lacks it.

## Install a release binary

Every tagged release publishes Linux binaries and the guest image to a public bucket. The bucket needs no credentials.

| Path | Holds |
| --- | --- |
| `releases/<version>/binaries/socialweb-computer-linux-amd64` | The host binary for Linux amd64. |
| `releases/<version>/binaries/socialweb-computer-linux-arm64` | The host binary for Linux arm64. |
| `releases/<version>/binaries/SHA256SUMS` | The sha256 of both binaries. |
| `releases/<version>/binaries/release.json` | The commit, the build mode and one entry per platform. |
| `releases/<version>/binaries/version-linux-<arch>.txt` | The pinned version and sha256 of every asset in that binary. |
| `releases/<version>/guest-image/firecracker-x86_64/` | `vmlinux`, `rootfs.ext4` and `image.json`: the guest image a bidder's VMs boot. |
| `releases/<version>/manifest.json` | Every file under the version, with its size and sha256. |

`<version>` is a git tag, for example `v0.0.0-rc3`. Everything in the bucket is readable without credentials: `releases/<version>/` holds one tag's build, `main/<commit>/` holds the build of each commit on `main`, and `main/latest/` is a copy of the newest one.

Take the newest release, verify it, and put it on your PATH:

```bash
BUCKET=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
V=$(curl -s "$BUCKET/?list-type=2&prefix=releases/&delimiter=/" \
  | grep -o 'releases/v[^<]*' | sed 's|releases/||; s|/$||' | sort -V | tail -1)
ARCH=linux-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
mkdir -p ~/.tmp/swc-release && cd ~/.tmp/swc-release
curl -fLO --progress-bar "$BUCKET/releases/$V/binaries/socialweb-computer-$ARCH"
curl -fLO --progress-bar "$BUCKET/releases/$V/binaries/SHA256SUMS"
grep " socialweb-computer-$ARCH\$" SHA256SUMS | sha256sum -c -
mkdir -p ~/.local/bin && install -m 0755 "socialweb-computer-$ARCH" ~/.local/bin/socialweb-computer
socialweb-computer version
```

If `~/.local/bin` is not on your PATH, add it, or install to `/usr/local/bin` instead.

`socialweb-computer version` prints the build, the commit and the sha256 of every asset in the binary. The release tag is in `binaries/release.json` and `manifest.json`.

To pin a version instead of taking the newest, set `V` yourself: `V=v0.0.0-rc3`.

`socialweb-computer install gateway`, `install bidder` and `upgrade --binary --sha256` take this same file and its sha256: `SHA=$(grep " socialweb-computer-$ARCH\$" SHA256SUMS | cut -d' ' -f1)`.

On a bidder host, `--guest-image-source` takes the guest image you name. It accepts a `file://` or `https://` base, fetches `image.json`, `vmlinux` and `rootfs.ext4` from `<base>/<file>`, and verifies each against the sha256 pinned in the bidder's build, so a guest image that does not match those pins is refused:

`BUCKET` and `V` are the variables set in the block above; set them again in a new shell.

```bash
HANDLE=bobvmbuilder.bsky.social
sudo ~/.local/bin/socialweb-computer install bidder --handle "$HANDLE" \
  --guest-image-source "$BUCKET/releases/$V/guest-image/firecracker-x86_64"
```

`install bidder` fetches and verifies it there and then, so an unreachable or mismatched image is an install failure rather than a surprise at the first RFP. It also prints the download's progress.

To place the files yourself instead, download them and pass a `file://` base:

```bash
for f in vmlinux rootfs.ext4 image.json; do
  curl -fLO --progress-bar "$BUCKET/releases/$V/guest-image/firecracker-x86_64/$f"
done
sudo install -d -m 0755 /var/lib/socialweb-computer/guest-image
sudo install -m 0644 vmlinux rootfs.ext4 image.json /var/lib/socialweb-computer/guest-image/
```

Only `firecracker-x86_64` is published. An arm64 host builds its own with `scripts/build-guest-image.sh`. See [Run and install a bidder](#run-and-install-a-bidder) for the rest of the install.

A binary installed from this bucket needs no image flag at all: it looks for the image pinned into its own build under `releases/<its version>` first and under `main/<its commit>` second, then falls back to `main/latest` and to the newest `releases/<v>` in the bucket, which is what a locally built binary with a dev stamp relies on. `--guest-image-source` takes precedence when given, and `--guest-image-release-base` searches another base. Every candidate is verified against the sha256 pinned in the binary, so a candidate whose bytes are not this build's is skipped for the next one, and only when every published candidate differs is the image refused rather than trusted.

## The RFP flow

A requester rents a VM by running the RFP flow. The requester posts a `compute.vm` record and a signed `market.rfp`; bidders bid; the requester accepts the winning bid; the winning bidder boots the guest from cloud-init `user_data` that installs the guest agent (sshd, the tunnel subscriber, and the client that reaches the guest). The requester reaches the guest only through the relay, over an SSH `ProxyCommand` on the websocket tunnel. The relay is the registry.

```
requester --market.rfp--> relay/PDS <--bid-- bidder (own kcp: MarketBidder, Bid, ComputeVM)
    | accept                                    | cloud-init user_data
    v                                            v
gateway (own kcp: Gateway, Cluster, NodeClaim)   firecracker VM = k3s node
    tenant k3s apiserver (kine + front) <-------- node joins over the iroh link
```

Every listener behind the edge binds port 0 and reports its bound address into the status of its kcp object, and peers find each other by service name through kcp. Only the stable public entry points (caddy on 443, an application's front) keep a fixed address.

## Build the socialweb-computer binary

Build from source when you change the code. To run a published release instead, see [Install a release binary](#install-a-release-binary).

Build a development binary from your working tree, uncommitted changes included. This is the binary for live runs.

```bash
scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
```

The build writes the host binary to `/var/tmp/swc/bin/socialweb-computer`, mirrors the working tree to `/var/tmp/swc/src`, and runs the TypeScript from that mirror. Put it on your PATH and it is the `socialweb-computer` in every command below:

```bash
export PATH=/var/tmp/swc/bin:$PATH
```

A development binary prints `dev build, not reproducible` with its commit in `socialweb-computer version`.

Build a release binary of one commit, offline and cached by input hash:

```bash
C=$(git rev-parse HEAD)
scripts/build-socialweb-computer.sh --platform linux/amd64 --commit "$C"
B=~/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64
```

`$B` and its sha256 are what `socialweb-computer install` and `socialweb-computer upgrade --binary --sha256` take.

CI publishes the same outputs to the public bucket: `releases/<tag>/` on a `v*` tag, and `main/<commit>/` with a copy at `main/latest/` on `main`. See [Install a release binary](#install-a-release-binary).

Build script options:

| Option | Effect |
|---|---|
| `--dev` | Build the working tree for the host platform; this is the binary for live runs. |
| `--publish DIR` | With `--dev`, copy the host binary to `DIR/bin/socialweb-computer` and mirror the working tree to `DIR/src`. |
| `--sync-only` | With `--publish`, mirror the working tree again without rebuilding. |
| `--platform P` | A release platform (`linux/amd64`, `linux/arm64`); repeat for more. Default: every release platform. |
| `--commit REV` | The commit to build. Default `HEAD`; uncommitted changes are not built. |
| `--build-root DIR` | Checkouts, caches, and outputs. Default `$HOME/.tmp/socialweb-computer-build`. |
| `--verify-reproducible` | Build the commit twice from two clean checkouts and refuse a platform whose builds differ. |
| `--no-cache` | With `--verify-reproducible`, build both from scratch. |

## Run a market network on one host

A market network is one instance, profile `market-network`, with one kcp. It runs the shared atproto services (fake PLC in `root:global`, relay and dispatchers in `root:relay`, PDSes in `root:alice` and `root:bob`) and composes child applications, each in its own workspace of that kcp: one bidder per `--bidders` (`root:bidder-<n>`), with `--with-gateway` one gateway (`root:gateway`), and one workspace per `run request --network` invocation. A child's components are DenoPods in `root:<app>:app`. A production bidder or gateway runs the same manifest set in a kcp of its own.

```bash
socialweb-computer dev up --instance mn1 --profile market-network --bidders 1 --with-gateway --vm-host-backend systemd-user
```

Run it as the VM account, on a host prepared as in [Prepare a bare host](#prepare-a-bare-host). With no `--guest-image-source`, `dev up` downloads and verifies the guest image of this build from the public bucket the first time and reuses it after. On a bare 8 vCPU host the first `dev up` took 92 seconds. Pass `--guest-image-source file:///var/lib/socialweb-computer/guest-image` to use a directory you placed yourself.

Inspect the network, use it, and stop it:

```bash
socialweb-computer dev status --instance mn1 | jq '.network.status.apps'
socialweb-computer status --instance mn1
eval "$(socialweb-computer dev env --instance mn1)"
socialweb-computer dev logs core --instance mn1 --tail 50
socialweb-computer dev down --instance mn1
```

- `dev status` prints the `MarketNetwork` object and each child application's roots and contracts; `status` prints the market counters and contract steps.
- `dev env` exports `KUBECONFIG` and the ports of the network's kcp.
- `dev logs` takes one log name; `core` is the log of the `socialweb-computer` process that runs kcp and supervises the DenoPods and HostProcesses.
- Run the network as the VM-slot account, since its bidders run the VMs.

`dev up` options for a market network:

| Option | Meaning | Default |
|---|---|---|
| `--instance` | The instance id. | derived from the worktree |
| `--profile` | `market-network`. | |
| `--bidders N` | How many `MarketBidder` children to compose (`bidder-1` .. `bidder-N`). | 1 |
| `--with-gateway` | Also compose a `Gateway` child. | |
| `--guest-image-source` | The `file://` or `https://` base the pinned guest image is fetched from and verified against by sha256. Unset, the image of this build is downloaded from the published artifacts. | |
| `--guest-image-release-base` | The base the published artifacts are searched under when `--guest-image-source` is unset. | the public bucket |
| `--vm-host-backend` | The placement backend of the VM units: `systemd-user` or `proc`. | the instance's own |
| `--app-values` | A YAML file merged over the market-network values. | |
| `--network-timeout` | How long `dev up` waits for the network and every child to report Ready. A cold first start can need longer than the default. | `3m` |

## Rent a VM through the RFP flow

`run request` posts a `compute.vm` record and a signed `market.rfp`, waits for bids, accepts one, boots the guest, opens an SSH session over the relay, and deletes the VM. Pass `--network` to use a running market network.

A secrets bundle is a JSON array of `{ "path": "/abs/path", "value": ... }`. The secrets are written only into the invocation's own OpenBao; the guest fetches them at boot, and they never enter a kcp object, an argument, a log, or a record.

```bash
echo '[{"path":"/root/x","value":"hello"}]' > secrets.json
socialweb-computer run request --network mn1 --secrets secrets.json --exec 'cat /root/x'
socialweb-computer run request --network mn1 --secrets secrets.json --hold --exec 'uname -a'
socialweb-computer run request list
socialweb-computer run request resume <name>
socialweb-computer run request release <name>
```

`run request` options:

| Option | Meaning |
|---|---|
| `--network` | The market network instance to run under. Unset: a standalone instance under `<state-dir>/requests`. |
| `--name` | The invocation's name, a DNS label. Default `req-<UTC time>`. |
| `--secrets` | The secrets bundle file. |
| `--exec` | Run this program over SSH on the guest instead of an interactive session. |
| `--hold` | Keep the contract when this process ends; `release` or `resume <name>` starts it again. |
| `--skip-ssh` | Do not open an SSH session once the guest is on the network. |
| `--contract-timeout` | How long to wait for the guest's `onNetwork` and host keys, and for a release. Default `3m`. |

SSH runs with `StrictHostKeyChecking=yes` against the host keys the guest reported (the guest's `vm.hostKeys`) and reaches the guest only through the relay.

## Run and install a bidder

Run a bidder inside a market network as a child application. Without `--guest-image-source`, `run bidder` downloads the guest image of its own build from the published artifacts, so a binary from the bucket needs no image flag.

- An installed bidder with the pinned image in its store boots it without asking the published artifacts. Without it, it asks them, and a miss is a refusal.
- A `run bidder` with `--host-backend proc` (the market-network mode above) always asks the published artifacts.
- A fetch prints its progress: a bar on a terminal, a line every few seconds otherwise, lines for a `file://` source. A file under 8 MiB prints nothing. In a market network, the progress goes to the network's log.

```bash
socialweb-computer run bidder --host-backend proc --profile market-network --network mn1 --handle b2.mn1.bob.test \
  --guest-image-source file:///var/lib/socialweb-computer/guest-image --vm-host-backend systemd-user
```

Install a bidder on a host (one explicit root step), then it runs as its own account, `swc-bidder`. A first install puts the binary you invoke at `<state-dir>/bin/current/socialweb-computer`:

```bash
HANDLE=bobvmbuilder.bsky.social
sudo /var/tmp/swc/bin/socialweb-computer install bidder --handle "$HANDLE" --adopt-account --allow-dev-binary \
  --guest-image-source file:///var/lib/socialweb-computer/guest-image
```

`--allow-dev-binary` accepts a binary built from a working tree. A release binary needs no such flag.

The install writes and enables one unit, `socialweb-computer-bidder@<handle>.service`, in the account's own systemd manager, and returns only once the stack is up. It fetches the guest image with progress and prints how long `enable --now` and the `/readyz` wait took. The first kcp start is a cold bootstrap, inside the install. `--ready-timeout` (default `1m`) bounds its `/readyz` wait; raise it before the first install on a slow host. On a failure, the line above it names what the install waited for, and the step table ends at the failed step. To inspect the unit afterwards:

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user status 'socialweb-computer-bidder@<handle>.service'
sudo journalctl _SYSTEMD_USER_UNIT='socialweb-computer-bidder@<handle>.service' --since -10min
sudo journalctl -t kcp --since -10min
```

`swc-bidder` is a system account (uid below 1000), so systemd writes its user units' log lines to the system journal, not to a per-user journal. `journalctl --user` as that account finds nothing and reports "insufficient permissions"; read the system journal as root with the `_SYSTEMD_USER_UNIT` filter above.

- The unit is `Type=notify` with a watchdog. Its start timeout is `--ready-timeout` plus six minutes; a core that never becomes ready is killed and restarted.
- Every run writes `ready-timeout` and `network-timeout` to the account's `config.json`, passed or not; the installed core reads its budgets there. The file changes only when a value changes.
- The kcp is at `/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp`, one slot under `slot-a`. See [Inspect the kcp of an application](#inspect-the-kcp-of-an-application).
- A re-run is safe: every step is idempotent and a completed step is skipped. A stored guest image is re-verified; one that no longer matches its pin is fetched again when a source resolves.
- A re-run that changes `config.json` or a unit file restarts the core units that read it. A re-run that changes nothing restarts nothing.
- Install refuses when `bin/current` is not the binary you invoked. Run `upgrade --binary <path> --sha256 <digest>` as the account, then the same `install` again; the refusal prints that command.

Upgrade an installed bidder as its account, staging the new binary from a release or a local build:

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=/var/lib/socialweb-computer/swc-bidder/config.json \
  /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer \
  upgrade --binary <new binary> --sha256 <its sha256>
```

- A production bidder gates which RFPs it answers with `bidder-policy` in its `config.json`, which the unit passes through `CONFIG_PATH_SOCIALWEB_COMPUTER`. Unset, the policy is this repository's default, `open` (`internal/appctl/bidder.go`), so a production bidder must name `tangled-vouch` there; `install bidder` does not write that key.
- The guest image is pinned by sha256 in [`internal/versions/versions.json`](internal/versions/versions.json); the bidder verifies every file against that pin.
- A production bidder uses the `prod-qr` profile. It has its own PDS, the DenoPod `pds` in its own kcp, and writes every market record there, never to the handle's own repo. The handle's account is written once: the QR code associates the bidder's DID with it. See [Sign in with OAuth](#sign-in-with-oauth).

`install bidder` options:

| Option | Meaning | Default |
|---|---|---|
| `--handle` | The atproto handle(s) of the bidder instances, comma-separated. A re-run with another handle adds that instance. | |
| `--account` | The system account the bidder runs as. | `swc-bidder` |
| `--adopt-account` | Take over an existing account that no install record names. | |
| `--allow-dev-binary` | Accept a binary built from a working tree. A release binary needs no such flag. | |
| `--guest-image-source` | The guest image source, written to `config.json`. Unset, the bidder downloads the image of this build from the published artifacts, which this install also does. | |
| `--guest-image-release-base` | The base the published artifacts are searched under. An image already in the store is re-verified and used without touching the network, unless its bytes no longer match its pin, which sends it back to the base. Clearing this option (an empty flag or env value) is a refusal when no `--guest-image-source` and no stored source names an image. | the public bucket |
| `--ready-timeout D` | How long the install waits for the kcp slot to answer `/readyz`, and the budget the installed core is given. It also sizes the installed unit's start timeout and watchdog, at this plus six minutes, so a large value means a long start timeout. Refused above `10m`, because past that a wedged core waits longer to be restarted, not a cold start for more room. | `1m` |
| `--network-timeout D` | How long the guest image fetch may take, and a third of it, at least 30s, is the budget for searching the release base when no source names an image. Refused under `30s` and above `30m`. | `3m` |
| `--vm-slots N` | One range of `N * 65536` subordinate ids reserved in `/etc/subuid` and `/etc/subgid`; each running VM holds one slot of 65536 ids. | 256 |
| `--dry-run` | Print the computed step list and change nothing. | |
| `--root DIR` | Install into a staged host under `DIR` as the invoking user, without root. | |

## Run the gateway and its web app

A development gateway is a child of a market network: start the network with `--with-gateway`.

A production gateway is built as a release, shipped, and installed or upgraded with `scripts/deploy.sh`, which also writes the caddy sites.

```bash
BINARY=<release binary> SHA256=<its sha256> SSH_KEY=~/.ssh/id_ed25519_no_password \
  GATEWAY_ENV_FILE=~/.tmp/deploy/gateway.env GATEWAY_SPEC_FILE=~/.tmp/deploy/gateway-spec.yaml scripts/deploy.sh
```

[Deploy a release to production](#deploy-a-release-to-production) builds that binary and computes its sha256.

`scripts/deploy.sh` reads these environment variables:

| Variable | Meaning | Default |
|---|---|---|
| `BINARY` | The release binary to ship. Unset: the script builds the current commit. | |
| `SHA256` | The published sha256 of `BINARY`; a mismatch stops the deploy. | |
| `SSH_KEY` | The SSH key for the deploy host. | |
| `SSH_TARGET` | The deploy host. | `root@socialweb.computer` |
| `SSH_PORT` | The deploy host's SSH port. | `1997` |
| `ACCOUNT` | The account the gateway runs as. | `socialweb` |
| `STATE_DIR` | The gateway's state directory. | `/var/lib/socialweb-computer/socialweb` |
| `DOMAIN` | The base domain. | `socialweb.computer` |
| `ORIGIN` | The web app's host. | `k8s.<DOMAIN>` |
| `KCP_ORIGIN` | The kcp host. | `kcp.<DOMAIN>` |
| `GATEWAY_ENV_FILE` | The gateway's environment file, shipped as `gateway.env`. | |
| `GATEWAY_SPEC_FILE` | A YAML `GatewaySpec`, shipped as the Gateway spec. | |

The script installs the gateway on its first run. On every later run it upgrades the gateway, then runs `install gateway` again with the new `bin/current`, which re-renders the unit files and restarts a core whose unit changed. The web app (`socialweb-computer-k8s`) serves `k8s.<domain>`, and caddy proxies the web app and the `/xrpc/computer.socialweb.k8s.*` routes to the gateway.

After a fresh deploy the requester account has no web app session, so cluster claims fail with a `no oauth session stored` message. Sign in with [Sign in with OAuth](#sign-in-with-oauth) and `--url https://k8s.socialweb.computer/`.

## Manage a tenant cluster over XRPC

The gateway serves the cluster API over atproto XRPC. Every method takes an atproto service JWT: `iss` is your DID, `aud` is the gateway DID, and `lxm` is the method NSID.

Methods (prefix `com.publicdomainrelay.temp.cluster.`):

| Method | What it does |
|---|---|
| `createCluster` | Create the named cluster if it is missing and leave it running, in one call. Idempotent. |
| `getKubeconfig` | Return a kubeconfig. Creates the cluster if it is missing but does not start the control plane. |
| `listClusters` | List the account's clusters. |
| `controlPlaneUp` | Start the control plane. |
| `controlPlaneDown` | Stop the control plane. |
| `deleteCluster` | Delete the cluster. |
| `subscribeClusters` | Stream cluster state as an event stream. |
| `revokeIdentity` | Revoke a cluster identity. |

### Create a cluster and get a kubeconfig with goat

```bash
go install github.com/bluesky-social/goat@latest
( . ~/.aliceoa_bsky_social_source; GOAT_USERNAME="$ALICE_ATPROTO_HANDLE" GOAT_PASSWORD="$ALICE_ATPROTO_PASSWORD" goat account login )
GW=https://k8s.socialweb.computer AUD=did:web:k8s.socialweb.computer N=com.publicdomainrelay.temp.cluster
call() { local m=$1 nsid=$N.$2; shift 2; goat xrpc $m $GW $nsid "$@" "Authorization:Bearer $(goat account service-auth --aud $AUD --lxm $nsid)"; }
call procedure createCluster name=demo                       # create if missing and start the control plane (idempotent)
call query listClusters | jq '.clusters[] | {name, phase, running}'
call procedure getKubeconfig name=demo ttlSeconds:=28800 | jq -r .kubeconfig > demo.kubeconfig; chmod 600 demo.kubeconfig
KUBECONFIG=demo.kubeconfig kubectl get ns
call procedure controlPlaneDown name=demo; call procedure deleteCluster name=demo
```

`goat xrpc` sends `key=value` as a JSON string and `key:=value` as a JSON number or boolean, so `ttlSeconds` and `noExpiry` take `:=`; `ttlSeconds=28800` is a string and the gateway answers `InvalidRequest`. The kubeconfig holds a client key and an x509 client certificate from the cluster's own OpenBao PKI (CN your DID, O `socialweb.computer:cluster-owners`, default 8 h, at most 24 h via `ttlSeconds`). Plain `kubectl`, no helper. Keep it `0600`, never commit it. `getKubeconfig` on a stopped cluster answers `ClusterNotRunning`: call `createCluster` or `controlPlaneUp` first. `revokeIdentity` and `deleteCluster` end every certificate at once. For a certificate that never expires, call `getKubeconfig name=demo noExpiry:=true`: it lives until you revoke it, delete the cluster, or the cluster CA expires. Cluster phases are `Unavailable`, `Initializing`, `Ready`, and `Draining`.

### Watch cluster state with subscribeClusters

`com.publicdomainrelay.temp.cluster.subscribeClusters` is an atproto event stream: a `GET` with a websocket upgrade, binary frames, each frame a DRISL-CBOR header then a DRISL-CBOR payload. Authenticate on the upgrade request with the same service JWT (`Authorization: Bearer`, `lxm` = `subscribeClusters`). The account is the token's `iss`. Pass `?name=<cluster>` to watch one cluster.

On connect the stream sends a snapshot (one `#cluster` per cluster, one `#member` per control-plane member, one `#nodeClaim` per claim, each with `snapshot:true`), then one `#info` named `SnapshotComplete`, then live changes. There is no cursor: reconnecting starts a fresh snapshot. The token is checked once at connect, and the stream lives until the client closes, the gateway restarts, or the 1 hour stream bound is reached (`TokenExpired`). An error frame is followed by a close.

| Message | Fields | Phase values |
|---|---|---|
| `#cluster` | name, phase, running, reason, message, server, instance, deleted | Unavailable, Initializing, Ready, Draining |
| `#member` | cluster, member, phase, reason, message | Pending, Starting, Running, Ready, Sealed, Stopping, Stopped, Deleting, Failed, Lost |
| `#nodeClaim` | cluster, name, nodeName, phase, message, contractPhase, outcome, bidder, fqdn, deleted | Pending, Bidding, Provisioning, Joining, Running, Failed, Released, Lost, Adopting |

Error frames: `InvalidRequest`, `Unauthorized`, `TokenExpired`, `ClusterNotFound`, `IdentityRevoked`, `ClusterUnavailable`, `ConsumerTooSlow`.

Decode the stream with the bundled client, or check connectivity with `websocat`:

```bash
T=$(goat account service-auth --aud $AUD --lxm $N.subscribeClusters)
# one JSON line per event; exit 0 on --until, 2 on --fail-on, 1 on an error frame
deno run -A --no-lock --node-modules-dir=none scripts/subscribe-clusters.ts --gateway $GW --token "$T" --name demo \
  --until cluster=Ready --fail-on nodeClaim=Failed
# every cluster of the account, run forever
deno run -A --no-lock --node-modules-dir=none scripts/subscribe-clusters.ts --gateway $GW --token "$T"
# raw connectivity check (CBOR bytes, not text)
websocat -b -n -H="Authorization: Bearer $T" "wss://k8s.socialweb.computer/xrpc/$N.subscribeClusters?name=demo" < /dev/null | xxd
```

`subscribe-clusters.ts` options: `--token`, `--gateway` (default `https://k8s.socialweb.computer`), `--name`, `--until <type>=<phase>`, `--fail-on <type>=<phase>`.

### Apply objects and run Jobs without a kubeconfig

`computer.socialweb.k8s.kubectlApplyServerSide` (procedure) applies one object as `kubectl apply --server-side` would. `computer.socialweb.k8s.getJob` (query) reads a Job's status, its pods, and the tail of each container's log. Neither needs a kubeconfig, `socialweb-credential`, or `kubectl`. Both authenticate with the same service JWT (`aud` = gateway DID, `lxm` = the method), and the account is the token's `iss`.

`kubectlApplyServerSide` inputs:

| Field | Meaning |
|---|---|
| `cluster` | The account's cluster label. Required. |
| `manifest` | One Kubernetes object as JSON, with `apiVersion`, `kind`, and `metadata.name`. Required. A `List` kind or `generateName` is refused. |
| `namespace` | A namespaced object's namespace. Order: `metadata.namespace`, then this field, then `default`; both set must match. A cluster-scoped kind takes none. |
| `name` | Optional check; must equal `metadata.name`. |
| `fieldManager` | The server-side apply field manager. Default `socialweb-computer`. The value `kubectl` shares field ownership with `kubectl apply --server-side`. |
| `force` | Take ownership of fields another field manager owns. Without it such a field is an `ApplyConflict`. |
| `dryRun` | Run admission and validation and return the object without storing it. |

Output: `apiVersion`, `kind`, `namespace`, `name`, `uid`, `resourceVersion`, `generation`, and `object` (as the apiserver stored it, `managedFields` included).

Errors: `InvalidRequest`, `Unauthorized`, `IdentityRevoked`, `ClusterNotFound`, `ClusterNotRunning` (409, call `controlPlaneUp`), `ClusterInitializing` (429, retry), `ClusterDraining` (409), `ClusterUnavailable` (503), `KindNotFound` (400), `ApplyConflict` (409), `KubernetesError` (the apiserver's status and message).

`getJob` parameters: `cluster`, `namespace`, `name`, and `tailLines` (default 100, maximum 5000, `0` returns no logs; each log is at most 64 KiB). Output: `phase` (Pending, Running, Succeeded, Failed), `active`, `succeeded`, `failed`, `conditions`, and `pods[].containers[] { state, exitCode, reason, log }`. Errors include `ObjectNotFound` (404). A Job on a cluster at zero nodes first rents a market VM for its pod.

Apply a ConfigMap:

```bash
K=computer.socialweb.k8s
curl -sS -X POST "$GW/xrpc/$K.kubectlApplyServerSide" \
  -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.kubectlApplyServerSide)" \
  -H "Content-Type: application/json" -H "Accept: application/json" \
  -d '{
    "cluster": "demo",
    "namespace": "default",
    "name": "app-config",
    "fieldManager": "kubectl",
    "manifest": {
      "apiVersion": "v1", "kind": "ConfigMap",
      "metadata": {"name": "app-config", "namespace": "default"},
      "data": {"environment": "production", "log_level": "info"}
    }
  }' | jq '{kind, namespace, name, uid, resourceVersion}'
```

Apply a Job and poll it until it finishes:

```bash
jq -n '{cluster: "demo", manifest: {apiVersion: "batch/v1", kind: "Job",
  metadata: {name: "hello", namespace: "default"},
  spec: {backoffLimit: 0, template: {spec: {restartPolicy: "Never",
    containers: [{name: "run", image: "docker.io/library/busybox:1.36.1", command: ["sh", "-c", "echo hello from $(uname -m)"]}]}}}}}' |
  curl -sS -X POST "$GW/xrpc/$K.kubectlApplyServerSide" -H "Content-Type: application/json" \
    -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.kubectlApplyServerSide)" -d @- | jq -c '{kind, name, uid}'
# poll until Succeeded or Failed (each call needs a fresh token: service-auth tokens are short-lived)
until s=$(curl -sS "$GW/xrpc/$K.getJob?cluster=demo&namespace=default&name=hello&tailLines=50" \
    -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.getJob)"); \
  echo "$s" | jq -c '{phase, active, succeeded, failed}'; echo "$s" | jq -e '.phase == "Succeeded" or .phase == "Failed"' >/dev/null; do sleep 5; done
echo "$s" | jq -r '.pods[].containers[] | "\(.state) exit=\(.exitCode) \(.log)"'
```

## Sign in with OAuth

`scripts/oauth-signin.ts` drives a headless `/usr/bin/google-chrome` (puppeteer-core) through the flow: the page, the PDS authorize page, the password, then "Authorize". The script reads the handle and password from the credential file itself, so they never appear on the command line or in a log. It writes one screenshot per step to `~/.tmp/oauth-signin/<ts>/` (mode 0700).

The credential file (mode 0600) exports `<ACCOUNT>_ATPROTO_HANDLE` and `<ACCOUNT>_ATPROTO_PASSWORD`.

```bash
# the web app user (alice) signs in to the gateway; do this after every fresh gateway deploy
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.aliceoa_bsky_social_source --account ALICE --url https://k8s.socialweb.computer/
# the bidder's handle associates the bidder's own DID once, through the QR url its MarketIdentity reports;
# K is the bidder's kcp kubeconfig
U=$(kubectl --kubeconfig $K get marketidentity bidder -o jsonpath='{.status.pendingAction.url}')
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source --account BOB --url "$U" --click '^Yes, this is my bidder$'
```

The bidder's url is `<qrOrigin>/#bdr=<bidder DID>`. After the sign-in, `--click` presses the confirm button: the page calls the bidder's `associateConfirm` through the account's PDS, the bidder's `pds` DenoPod records the association as the AtprotoRecord object `association-...` in the bidder's kcp, and the page writes one `badgeBlueKeys` record into the account's repo. That is the only write to the account. The MarketIdentity then reads `Ready` with the account in `status.associations`, and a restart or an `upgrade` keeps the DID: the key is in the bidder's OpenBao and the repo in `<handle state>/pds`.

For the bidder, read the current association url from the `MarketIdentity` object's `status.pendingAction.url` in the bidder's kcp (see [Inspect the kcp of an application](#inspect-the-kcp-of-an-application)). On an installed bidder host the kcp kubeconfig is readable by root only, so run `kubectl` through `sudo` with `K=/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp/admin.kubeconfig`; the `MarketIdentity` appears a few seconds after `install bidder` returns, so retry on `NotFound`. Run `oauth-signin.ts` on a machine with Chrome, passing the url.

`oauth-signin.ts` options: `--creds`, `--account`, and `--url` (all required), `--out` (the screenshot directory), `--done "<text>"` (wait for this text after consent), `--chrome` (the browser path, default `/usr/bin/google-chrome`), `--chrome-arg` (an extra browser flag; repeat it), `--sign-in-as` (type this identifier instead of the handle in the credential file) and `--cookie-file` (write the browser cookies to this file, mode 0600).

A QR url expires after 5 minutes, so read the current one from the MarketIdentity status right before running the script.

The association url does not expire; the session-transfer QR of the old `oauthQr` mode did. To force the bidder to sign in again, for example after the OAuth client scope changes on qr.fedfork.com (only an `oauthQr` identity has such a session):

1. Delete `bidder/metadata/identity/bidder/session` in the bidder's OpenBao: log in with the certificate at `<handle state>/openbaoservers/bidder/openbao/tls` as the client `bidder` (its certificate is under `clients/bidder`).
2. Restart the bidder unit, as its account: `sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) systemctl --user restart 'socialweb-computer-bidder@<handle>.service'`. The `MarketIdentity` reports `AwaitingScan` with a new url.
3. Run `oauth-signin.ts` against that url.

Check the result with `kubectl get marketidentity bidder -o jsonpath='{.status.phase}'` on the bidder's kcp: `Ready`.

Symptoms: a gateway log line `no oauth session stored for did:...` means the web app user must sign in. A bidder log line `ScopeMissingError` means the OAuth client scope must be deployed to qr.fedfork.com (a separate host) and the bidder re-signed.

## Deploy a release to production

> Warning: a production deployment preserves nothing. Losing sessions, VMs, clusters, claims, or receipts during a deploy is acceptable, so deploy fresh.

```bash
# 1. confirm the commit's run is green on GitHub (.github/workflows/ci.yml)
# 2. build the release
C=$(git rev-parse HEAD)
scripts/build-socialweb-computer.sh --platform linux/amd64 --commit $C
B=~/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64; SHA=$(sha256sum $B | cut -d' ' -f1)
# 3. gateway (socialweb.computer): ship, install the first time or upgrade and install again after, write the caddy sites
BINARY=$B SHA256=$SHA SSH_KEY=~/.ssh/id_ed25519_no_password \
  GATEWAY_ENV_FILE=~/.tmp/deploy/gateway.env GATEWAY_SPEC_FILE=~/.tmp/deploy/gateway-spec.yaml scripts/deploy.sh
# 4. bidder: stage the binary, then upgrade it as the account
R=/var/tmp/swc-release/socialweb-computer-${C:0:8}; sudo install -m 0755 $B $R
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=/var/lib/socialweb-computer/swc-bidder/config.json $R upgrade --binary $R --sha256 $SHA
```

Verify the deployment by scaling a tenant cluster from zero and back:

```bash
GW=https://k8s.socialweb.computer AUD=did:web:k8s.socialweb.computer K=computer.socialweb.k8s
# apply the Job below to the canary cluster, then poll it (see Manage a tenant cluster over XRPC)
jq -n '{cluster: "canary", manifest: {apiVersion: "batch/v1", kind: "Job",
  metadata: {name: "scale-check", namespace: "default"},
  spec: {backoffLimit: 0, template: {spec: {restartPolicy: "Never",
    containers: [{name: "run", image: "docker.io/library/busybox:1.36.1", command: ["sh", "-c", "echo up"]}]}}}}}' |
  curl -sS -X POST "$GW/xrpc/$K.kubectlApplyServerSide" -H "Content-Type: application/json" \
    -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.kubectlApplyServerSide)" -d @- | jq -c '{kind, name}'
curl -sS "$GW/xrpc/$K.getJob?cluster=canary&namespace=default&name=scale-check&tailLines=50" \
  -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.getJob)" | jq -c '{phase, succeeded}'
```

A tenant cluster scales from zero when you apply a pod to it: the pod raises a `NodeClaim`, the cluster runs the RFP flow for a market VM, the node joins, and the pod runs. Deleting the pod returns the cluster to zero nodes and deletes the VM. Applying a Job on a cluster at zero nodes works the same way; poll `getJob` or watch `#nodeClaim` on `subscribeClusters`.

Roll back as the account with `socialweb-computer rollback`.

Once per host, or when the item changes:

```bash
# guest image on a bidder host (pinned sha256 in internal/versions/versions.json)
scripts/build-guest-image.sh --target firecracker-x86_64 --out /var/tmp/guest-image
sudo install -d -m 0755 /var/lib/socialweb-computer/guest-image && sudo install -m 0644 /var/tmp/guest-image/firecracker-x86_64/{vmlinux,rootfs.ext4,image.json} /var/lib/socialweb-computer/guest-image/
# OAuth client scope on qr.fedfork.com (the did-key-associator frontend, a separate host); needed when a bidder writes a new record type
cd ../did-key-associator && SSH_KEY=~/.ssh/id_ed25519_no_password scripts/deploy.sh --static-only
```

The deploy inputs (gateway.env, gateway-spec.yaml) live on the orphan branch `swc-deploy`, in the worktree `~/.tmp/deploy`. The credential files `~/.aliceoa_bsky_social_source` and `~/.bobvmbuilder_bsky_social_source` are mode 0600; never print, log, or commit their values.

## Inspect the kcp of an application

A production application has its own kcp; the applications of a market network share the network's kcp, one workspace each (`root:<app>`). A kcp admin kubeconfig's server URL ends in `/clusters/<workspace>`; swap the workspace to walk the kcp. Define two helpers:

```bash
kc() { local k=$1 ws=$2; shift 2; local b; b=$(kubectl --kubeconfig $k config view -o jsonpath='{.clusters[0].cluster.server}' | sed 's#/clusters/.*##'); kubectl --kubeconfig $k --server "$b/clusters/$ws" "$@"; }
tree() { local k=$1 ws=${2:-root}; echo "$ws"; for c in $(kc $k $ws get workspaces -o name 2>/dev/null | sed 's#.*/##'); do tree $k "$ws:$c"; done; }
```

Find every kcp and every tenant cluster kubeconfig under a state root:

```bash
STATE=/var/tmp/swcit/<run>                                 # integration runs; a dev instance defaults to $HOME/.local/state/socialweb-computer
find $STATE /var/lib/socialweb-computer -name admin.kubeconfig -path '*kcp/admin.kubeconfig'   # every kcp front kubeconfig
find $STATE /var/lib/socialweb-computer -path '*server/cred/admin.kubeconfig'                   # every tenant k3s kubeconfig
for K in $(find $STATE -path '*kcp/admin.kubeconfig' -not -path '*slot-*' -not -path '*shared*'); do echo "== $K"; tree $K; done
kc $K root api-resources --api-group=socialweb.computer    # the kinds this layer serves
```

- `dev env` prints the network's kcp kubeconfig and ports.
- A child application of a market network has no kcp of its own: walk `root:<app>` of the network's kcp. Its state (component state dirs, its OpenBao) is under the network's state dir, in `apps/`; the run directories of every DenoPod of the network are under `<network state>/denokcp/runs/`, and each names its pod in `service.txt`.
- A production bidder's kcp is under `/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp`, and a production gateway's kcp is under its state dir (default `/var/lib/socialweb-computer/socialweb/kcp`). A claim lives in the cluster's workspace: `kc $K root:<acct>:<cluster> get nodeclaims`.
- A production host has no `kubectl`; use the gateway's embedded k3s: `K3S=$(find /var/lib/socialweb-computer/socialweb/runtime -name k3s -type f | head -1); $K3S kubectl ...`.

DenoPod logs live under the run directory (`runs/`), one directory per pod, with `stdout.txt` and `stderr.txt` (gone on restart). The `socialweb-computer` process logs to `journalctl --user -u <unit>`, or use `dev logs core`.

SSH to a production host: `ssh -i ~/.ssh/id_ed25519_no_password -p 1997 root@socialweb.computer`.

## Run the integration tests

The integration tests are Go tests under `test/integration` with the `integration` build tag. Each test starts its own market network as the VM account, drives it with the `socialweb-computer` verbs, and tears it down. Run them as an account that may `sudo -n -u test-0001`.

```bash
go test -tags integration -count=1 -v -timeout 60m ./test/integration/
# or one test
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestRequestSecretsReachTheGuestThroughTheRFPFlow
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestTenantClusterScalesFromZeroOnMarketVMsAndBack
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestClusterEventsStreamFromSubscribeClusters
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestKubeconfigClientCertificateWithoutCredentialHelper
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestWebAppSignInThroughThePluginKeepsTheCookieAcrossARestartAndIssuesACertificate
```

The web app sign-in test needs `deno` and `/usr/bin/google-chrome` for the user that runs `go test`: it runs `scripts/oauth-signin.ts` against the network's gateway, which it fronts with a TLS listener of its own on a port it picks.

Environment variables:

| Variable | Meaning | Default |
|---|---|---|
| `SOCIALWEB_IT_ACCOUNT` | The VM account the tests run as. | `test-0001` |
| `SOCIALWEB_IT_GUEST_IMAGE` | The guest image source. | `file:///var/lib/socialweb-computer/guest-image` |
| `SOCIALWEB_IT_PUBLISH` | Where the built binary is published for the run. | |
| `SOCIALWEB_IT_SKIP_BUILD` | Reuse the published binary instead of rebuilding. | |
| `SOCIALWEB_IT_RUN_ROOT` | The run root. Keep it short, since a tenant cluster's front socket path must fit in 107 bytes. | `/var/tmp/swcit` |
| `SOCIALWEB_IT_ARTIFACTS` | The directory for logs. | `~/.tmp/swc-it/<run>` |
| `SOCIALWEB_IT_LIVE_SLOT` | A host live-slot script the run takes before it starts; `none` turns it off. | the host's script, when one is configured |

Each integration test takes VM slots of that account and brings up its own market network.

## License

Unlicense (public domain).
