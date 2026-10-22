# Contributing

Read [AGENTS.md](AGENTS.md) first. It states the working rules, the edit disciplines and the live-run
conventions. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) holds the decisions and the invariants;
[docs/plans/](docs/plans/) and [NEXT.yaml](NEXT.yaml) hold the plan status and the backlog.

Work in a plain git worktree or branch of this repository. Commit small logical commits. Do not push.
Run live work as the VM account (`test-0001`) from a development build and a mirror that account can
read.

## Run a development build

Build the working tree, uncommitted changes included:

```bash
cd "$(git rev-parse --show-toplevel)"
scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
export PATH=/var/tmp/swc/bin:$PATH
socialweb-computer version
```

The build writes the host binary to `/var/tmp/swc/bin/socialweb-computer` and mirrors the working tree
to `/var/tmp/swc/src`. A development binary prints `dev build, not reproducible` with its commit in
`socialweb-computer version`.

Build script options:

| Option | Effect |
|---|---|
| `--dev` | Build the working tree for the host platform; this is the binary for live runs. |
| `--publish DIR` | With `--dev`, copy the host binary to `DIR/bin/socialweb-computer` and mirror the working tree to `DIR/src`. |
| `--sync-only` | With `--publish`, mirror the working tree again without rebuilding. |
| `--platform P` | A release platform (`linux/amd64`, `linux/arm64`); repeat for more. Default: every release platform. |
| `--commit REV` | The commit to build. Default `HEAD`; uncommitted changes are not built. |
| `--build-root DIR` | Checkouts, caches, and outputs. Default `$HOME/.tmp/socialweb-computer-build`. |
| `--update-pins` | Rebuild the assets built from this repository and write their new pins to `internal/versions/versions.json`. Downloaded assets only verify their pins. `make` passes this. |

## Build with make

```bash
cd "$(git rev-parse --show-toplevel)"
make
make pins
make check-pins
make dev
```

`make` builds the release binary for both linux release platforms, rebuilding the assets this
repository owns and writing their new pins to `internal/versions/versions.json`. Downloaded assets
still verify their existing pins and never move. Commit the pin change with the source change.

`make pins` stages only the assets this repository owns and updates `versions.json`; it does not build
the binary. `make check-pins` runs that and fails if `versions.json` differs from `HEAD`; CI runs it.
`make dev` builds the working-tree binary for live runs under `/var/tmp/swc`.

Build a release binary of one commit, offline and cached by input hash:

```bash
cd "$(git rev-parse --show-toplevel)"
C=$(git rev-parse HEAD)
scripts/build-socialweb-computer.sh --platform linux/amd64 --commit "$C"
B=$HOME/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64
sha256sum "$B"
```

`$B` and its sha256 are what `socialweb-computer install` and
`socialweb-computer upgrade --binary --sha256` take. CI publishes the same outputs to the public
bucket: `releases/<tag>/` on a `v*` tag, and `main/<commit>/` with a copy at `main/latest/` on `main`.

## Prepare a bare host

Install these as root; the commands in this document check the host and name anything missing.

| Need | Ubuntu / Debian package | Fedora / RHEL package |
|---|---|---|
| `newuidmap` and `newgidmap`, so each VM runs in its own user namespace | `uidmap` | `shadow-utils` |
| `ssh` and `ssh-keygen`, which the RFP flow uses | `openssh-client` | `openssh-clients` |
| `curl`, to install the binary and fetch the guest image | `curl` | `curl` |
| `/usr/share/zoneinfo`, for the embedded postgres of the gateway | `tzdata` | `tzdata` |
| `jq`, used by the examples below (optional) | `jq` | `jq` |
| `machinectl`, to log in as the VM account (optional; or use `ssh`) | `systemd-container` | `systemd-container` |

As root, once:

```bash
apt-get install -y uidmap openssh-client curl tzdata jq systemd-container
useradd -m -s /bin/bash -G kvm -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 test-0001
usermod --add-subuids 100000-1148575 --add-subgids 100000-1148575 test-0001
install -d -m 0700 -o test-0001 -g test-0001 ~test-0001/.ssh
install -m 0600 -o test-0001 -g test-0001 ~/.ssh/authorized_keys ~test-0001/.ssh/
loginctl enable-linger test-0001
sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
echo kernel.apparmor_restrict_unprivileged_userns=0 > /etc/sysctl.d/60-socialweb.conf
```

| Note | Why |
| --- | --- |
| `-K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0` | `useradd` must not add its own 65536-id range; the account needs exactly one range in each file. `100000-1148575` is 16 VM slots |
| `loginctl enable-linger` | keeps the account's `systemd --user` manager up; log in with `ssh test-0001@<host>` or, as root, `machinectl shell test-0001@` (`sudo -u` and `runuser` give no `/run/user/<uid>` session) |
| the `sysctl` | lets the embedded pasta create a user namespace on Ubuntu 24.04+; skip it on hosts without AppArmor. A production bidder loads an AppArmor profile through `install bidder` |
| `/dev/kvm`, `/dev/net/tun` | kvm is read-write for the `kvm` group; if tun is missing, `modprobe tun` |
| cgroup delegation | `cpu memory pids` on `user@<uid>.service` is the Ubuntu 24.04 default; the commands in this document name it when missing |
| apt lock on a cloud first boot | an unattended upgrade can hold it for minutes; rerun `apt-get` |

## Run a market network on one host

One instance, profile `market-network`, one kcp: shared services and one workspace per application,
components in `root:<app>:app`. Production runs the same manifests in a kcp per application.

Run these as the VM account on a host prepared as in [Prepare a bare host](#prepare-a-bare-host). The
wrapper sets that account's session environment; the first `dev up` fetches and verifies the guest
image unless `--guest-image-source` names one.

```bash
cd "$(git rev-parse --show-toplevel)"
swc() {
  sudo -n -u test-0001 env HOME=/home/test-0001 \
    XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u test-0001)/bus \
    /var/tmp/swc/bin/socialweb-computer "$@"
}
# start
swc dev up --instance mn1 --profile market-network --bidders 1 --with-gateway --vm-host-backend systemd-user
# inspect
swc dev status --instance mn1 | jq '.network.status.apps'
swc status --instance mn1
eval "$(swc dev env --instance mn1)"
swc dev logs core --instance mn1 --tail 50
# stop
swc dev down --instance mn1
```

| Command | Prints |
|---|---|
| `dev status --instance mn1` | the `MarketNetwork` object, each child's roots and contracts |
| `status --instance mn1` | market counters and contract steps |
| `dev env --instance mn1` | exports `KUBECONFIG` and the kcp ports |
| `dev logs core --instance mn1` | the core's log |
| `dev down --instance mn1` | stops the network |

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

`run request` posts a `compute.vm` record and a signed `market.rfp`, waits for bids, accepts one,
boots the guest, opens an SSH session to the guest's iroh ticket, and deletes the VM. Pass `--network`
to use a running market network; run it as that network's VM account.

A secrets bundle is a JSON array of `{ "path": "/abs/path", "value": ... }`. The secrets are written
only into the invocation's own OpenBao; the guest fetches them at boot, and they never enter a kcp
object, an argument, a log, or a record.

```bash
cd "$(git rev-parse --show-toplevel)"
swc() {
  sudo -n -u test-0001 env HOME=/home/test-0001 \
    XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u test-0001)/bus \
    /var/tmp/swc/bin/socialweb-computer "$@"
}
sudo -n -u test-0001 tee /var/tmp/swc-secrets.json >/dev/null <<'JSON'
[{"path":"/root/x","value":"hello"}]
JSON
# one-shot: returns when the command ends and releases the VM
swc run request --network mn1 --secrets /var/tmp/swc-secrets.json --exec 'cat /root/x'
# or hold: the VM stays until release, here or in another shell
swc run request --network mn1 --secrets /var/tmp/swc-secrets.json --hold --exec 'uname -a'
# list held invocations, then release one
swc run request list --json | jq -r '.[].name'
NAME=$(swc run request list --json | jq -r '.[0].name')
swc run request release "$NAME"
# or resume it in the foreground instead of releasing
# swc run request resume "$NAME"
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

SSH runs with `StrictHostKeyChecking=yes` against the host keys the guest reported (the guest's
`vm.hostKeys`), through a `ProxyCommand` that dials the guest's iroh ticket. The market records travel
through the relay; the guest is reached only over iroh.

## Run a bidder in a market network

Run this while the market network is up, as its VM account:

```bash
cd "$(git rev-parse --show-toplevel)"
swc() {
  sudo -n -u test-0001 env HOME=/home/test-0001 \
    XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u test-0001)/bus \
    /var/tmp/swc/bin/socialweb-computer "$@"
}
swc run bidder --host-backend proc --profile market-network --network mn1 --handle b2.mn1.bob.test
```

An installed bidder, its unit, state, guest image and upgrade path are in the README section
[Run and install a bidder](README.md#run-and-install-a-bidder) and [docs/bidder.md](docs/bidder.md).

## Run the integration tests

The integration tests are Go tests under `test/integration` with the `integration` build tag. Each
test starts its own market network as the VM account, drives it with the `socialweb-computer` verbs,
and tears it down. Run them as an account that may `sudo -n -u test-0001`.

```bash
cd "$(git rev-parse --show-toplevel)"
go test -tags integration -count=1 -v -timeout 60m ./test/integration/
# one test
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestRequestSecretsReachTheGuestThroughTheRFPFlow
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestTenantClusterScalesFromZeroOnMarketVMsAndBack
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestClusterEventsStreamFromSubscribeClusters
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestKubeconfigClientCertificateWithoutCredentialHelper
go test -tags integration -count=1 -v -timeout 60m ./test/integration/ -run TestWebAppSignInThroughThePluginKeepsTheCookieAcrossARestartAndIssuesACertificate
```

The web sign-in test drives the gateway's API directly: it calls `POST /oauth/start`, posts the PDS
authorization form and follows the callback to the `swc_session` cookie. It needs no browser.

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

Each integration test takes VM slots of that account and brings up its own market network. Use a unique
`SOCIALWEB_IT_RUN_ROOT` when running two at once.

## Run the gate before ready

```bash
cd "$(git rev-parse --show-toplevel)"
deploy/check.sh --fast
```

That runs gofmt, go build, apigen and its diff, go vet, the cross-OS builds and the release builds.
The full gate before a release also runs the release closure and the live proofs this host can run:
read [AGENTS.md](AGENTS.md) "How to verify".

## Select the PDS ingress provider

A production PDS is a public HTTPS service. Its account DID document carries an `atproto_pds`
service entry whose `serviceEndpoint` is scheme, host and optional port only, and the PDS
`/.well-known/did.json`, `createAccount` and `requestCrawl` use the same public host. An
`IngressProxy` provides that public URL; the PDS itself may bind loopback or an internal port.

Every app root selects the provider explicitly:

```yaml
spec:
  ingress:
    provider: xrpc-subscription
    ingressRef:
      kind: IngressProxy
      name: ingress
    publicHost: example.fedproxy.com
    xrpc:
      ingressProxyHost: example.fedproxy.com
      relayURL: https://relay.example
```

Or, for a tunneled route:

```yaml
spec:
  ingress:
    provider: opentunnel
    ingressRef:
      kind: IngressProxy
      name: ingress
    publicHost: example.example
    opentunnel:
      command: [/usr/local/bin/opentunnel, route, add]
      routeName: alice-pds
      localHost: 127.0.0.1:2583
      proxyProtocol: v2
```

The default is `xrpc-subscription` when `ingress.xrpc.ingressProxyHost` is set and `opentunnel` when
`ingress.opentunnel` is set. With neither, selection fails closed. `iroh` is recognized by the API
and rejected until a later plan. A missing provider component, command or route name is a named
failure, and a provider that returns a different public URL on restart is a `StableHostMismatch`
failure. There is no implicit local fallback and no `GuestTlsUrl` override.

Set `Pds.spec.requireIngress` for every production PDS. No account is created and no sign-in
completes while the ingress is not Ready. The first public URL is persisted in
`Pds.spec.publicURL` and `Pds.spec.stableHost`. See
[the atproto DID specification](https://atproto.com/specs/did) and
[the going-to-production guide](https://atproto.com/guides/going-to-production).

## Install a release build on a bidder host

Build the release commit first. `make` builds both linux release platforms and updates the owned
asset pins; commit `internal/versions/versions.json` if it changed.

```bash
cd "$(git rev-parse --show-toplevel)"
HANDLE=bobvmbuilder.bsky.social
STATE=/var/lib/socialweb-computer/swc-bidder

make
C=$(git rev-parse HEAD)
BIN=$HOME/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64
SHA=$(sha256sum "$BIN" | cut -d' ' -f1)
echo "$BIN $SHA"
sudo "$BIN" install bidder --handle "$HANDLE" \
  --guest-image-source file:///var/lib/socialweb-computer/guest-image
```

On a host without the local guest image, omit `--guest-image-source`; install resolves and verifies
the image from the release base.

Upgrade an existing release install as the account, then install again:

```bash
cd "$(git rev-parse --show-toplevel)"
C=$(git rev-parse HEAD)
BIN=$HOME/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64
SHA=$(sha256sum "$BIN" | cut -d' ' -f1)
sudo install -m 0755 "$BIN" /var/tmp/swc-release/socialweb-computer-${C:0:8}
STAGED=/var/tmp/swc-release/socialweb-computer-${C:0:8}
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=/var/lib/socialweb-computer/swc-bidder/config.json \
  /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer \
  upgrade --binary "$STAGED" --sha256 "$SHA"
sudo "$STAGED" install bidder --handle bobvmbuilder.bsky.social
```

### Associate the bidder DID

A fresh install gets a new DID. The association is one `badgeBlueKeys` claim record in the operator's
repo, plus the operator DID in `spec.approvedAssociations` of the bidder's `MarketIdentity`. The
helper reads the handle and password from the credentials file and prints neither.

```bash
cd "$(git rev-parse --show-toplevel)"
HANDLE=bobvmbuilder.bsky.social
ACCOUNT=BOB
CREDS=$HOME/.bobvmbuilder_bsky_social_source
STATE=/var/lib/socialweb-computer/swc-bidder

K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
BIDDER_DID=$(sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.did}')
OPERATOR_DID=$(curl -fsS "https://public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle?handle=$HANDLE" | jq -r .did)
echo "$BIDDER_DID $OPERATOR_DID"
deno run -A --no-lock scripts/associate-bidder.ts \
  --creds "$CREDS" \
  --account "$ACCOUNT" \
  --bidder-did "$BIDDER_DID"
sudo kubectl --kubeconfig "$K" patch marketidentity bidder --type=merge \
  -p "{\"spec\":{\"approvedAssociations\":[\"$OPERATOR_DID\"]}}"
sudo kubectl --kubeconfig "$K" get marketidentity bidder \
  -o jsonpath='{.status.phase}{" "}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

### Skip the association

For an installed bidder that must not write the association record, set the identity mode to `Skip`
and let the controller reconcile it:

```bash
cd "$(git rev-parse --show-toplevel)"
STATE=/var/lib/socialweb-computer/swc-bidder
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
sudo kubectl --kubeconfig "$K" patch marketbidder app --type=merge \
  -p '{"spec":{"identity":{"association":"Skip"}}}'
sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.phase}{" "}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

## Deploy a gateway to a host

`scripts/deploy.sh` installs or upgrades the gateway on the target host, writes the Caddy sites,
copies the gateway, edge-client and kcp CA certificates, validates Caddy and reloads it. The host
must already run Caddy and have `/etc/caddy/sites` enabled from `/etc/caddy/Caddyfile`.

```bash
cd "$(git rev-parse --show-toplevel)"
SSH_TARGET=root@socialweb.computer \
SSH_PORT=1997 \
ACCOUNT=socialweb \
STATE_DIR=/var/lib/socialweb-computer/socialweb \
GATEWAY_ENV_FILE=$HOME/.tmp/gateway.env \
GATEWAY_SPEC_FILE=$HOME/.tmp/gateway-spec-socialweb.yaml \
scripts/deploy.sh
```

`GATEWAY_ENV_FILE` holds the account's environment (audiences and cluster-provider options); it is
installed as `$STATE_DIR/gateway.env`, mode 0600 and owned by `socialweb`. `GATEWAY_SPEC_FILE` is
installed as `/var/lib/socialweb-computer/gateway-spec-$ACCOUNT.yaml`. Both are required for a fresh
host; an existing host may omit them and keep the files already in its state. `scripts/deploy.sh`
uses the release binary of `HEAD` (or `BINARY` and `SHA256`) and refuses a digest mismatch.

The public edge is Caddy:

- `k8s.<domain>` goes to `127.0.0.1:8788`, the gateway's web-app front. That front resolves to the
  GatewayAPI local Exposure, which serves `/api`, `/session`, `/oauth`, `/xrpc` and `/downloads`.
- `kcp.<domain>` goes to kcp `6443` without a client certificate and to the `8444` certificate front
  with one.

The web root returns 404: this tree is API-only. Start a sign-in with
`POST https://k8s.<domain>/oauth/start` and `{"handle":"..."}`; finish it in the returned
authorization URL. The callback sets `swc_session` on the gateway origin.

A fresh install over an old `denoruntime` workspace must purge first: the `app` WorkspaceType name is
immutable. Back up `/var/lib/socialweb-computer/<account>` and the Caddy sites before a purge. After
any fresh install, run the Caddy certificate handoff (inside `scripts/deploy.sh`) before testing
`kcp.<domain>`; stale edge-client or gateway certificates fail with `tls: bad certificate`.

## Install a development build on a bidder host

This installs a binary built from the working tree, not a release. The install and upgrade require
`--allow-dev-binary`. The binary is not reproducible.

Change `HANDLE`, `ACCOUNT` and `CREDS` for another account. `CREDS` is a mode-0600 file that exports
`<ACCOUNT>_ATPROTO_HANDLE` and `<ACCOUNT>_ATPROTO_PASSWORD`; never put the values on a command line.
Run the blocks in order in one shell: the later blocks use `DEV`, `SHA`, `HANDLE`, `ACCOUNT`, `CREDS`
and `STATE` from the earlier ones.

```bash
cd "$(git rev-parse --show-toplevel)"
HANDLE=bobvmbuilder.bsky.social
ACCOUNT=BOB
CREDS=$HOME/.bobvmbuilder_bsky_social_source
STATE=/var/lib/socialweb-computer/swc-bidder

scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
C=$(git rev-parse HEAD)
sudo mkdir -p /var/tmp/swc-release
sudo install -m 0755 /var/tmp/swc/bin/socialweb-computer /var/tmp/swc-release/socialweb-computer-${C:0:8}
DEV=/var/tmp/swc-release/socialweb-computer-${C:0:8}
SHA=$(sha256sum "$DEV" | cut -d' ' -f1)
echo "$DEV $SHA"
```

A fresh install over an old-shape install must purge first. This removes the state dir (the bidder's
DID, PDS repo, OpenBao state and bid history) and the install record. If install adopted an existing
account, as on this host, purge keeps that account and `--adopt-account` is then required because the
account has no install record. If install created the account, purge removes it and you omit
`--adopt-account`. Add `--force` only if uninstall refuses on active VM units; each stopped VM becomes
`Lost`.

```bash
sudo "$DEV" uninstall bidder --purge --json
sudo "$DEV" install bidder \
  --handle "$HANDLE" \
  --allow-dev-binary \
  --adopt-account \
  --state-dir "$STATE" \
  --guest-image-source file:///var/lib/socialweb-computer/guest-image \
  --dry-run
sudo "$DEV" install bidder \
  --handle "$HANDLE" \
  --allow-dev-binary \
  --adopt-account \
  --state-dir "$STATE" \
  --guest-image-source file:///var/lib/socialweb-computer/guest-image
```

The first install invocation is a server-side dry run: it prints the computed step list and persists
nothing. The second performs the install.

The installed host guest image matches the pins in `internal/versions/versions.json`. Without a stored
source, install searches the release base; a dev build's commit is not published, so name the local
image as above. If `/var/lib/socialweb-computer/guest-image` does not exist on the host, omit
`--guest-image-source` and let install resolve the image from the release base.

## Associate a development bidder DID

The bidder gets a new DID after a purge. The association is the same claim record and approved DID as
the release install:

```bash
cd "$(git rev-parse --show-toplevel)"
HANDLE=bobvmbuilder.bsky.social
ACCOUNT=BOB
CREDS=$HOME/.bobvmbuilder_bsky_social_source
STATE=/var/lib/socialweb-computer/swc-bidder

K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
BIDDER_DID=$(sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.did}')
OPERATOR_DID=$(curl -fsS "https://public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle?handle=$HANDLE" | jq -r .did)
echo "$BIDDER_DID $OPERATOR_DID"
deno run -A --no-lock scripts/associate-bidder.ts \
  --creds "$CREDS" \
  --account "$ACCOUNT" \
  --bidder-did "$BIDDER_DID"
sudo kubectl --kubeconfig "$K" patch marketidentity bidder --type=merge \
  -p "{\"spec\":{\"approvedAssociations\":[\"$OPERATOR_DID\"]}}"
sudo kubectl --kubeconfig "$K" get marketidentity bidder \
  -o jsonpath='{.status.phase}{" "}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

After the association, check the unit and the identity:

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user status 'socialweb-computer-bidder@bobvmbuilder.bsky.social.service'
sudo journalctl _SYSTEMD_USER_UNIT='socialweb-computer-bidder@bobvmbuilder.bsky.social.service' --since -10min
sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.phase}{"\n"}'
```

## Upgrade an installed bidder to a development build

If the install record and the old shape are compatible, upgrade instead of purging. As the account:

```bash
cd "$(git rev-parse --show-toplevel)"
HANDLE=bobvmbuilder.bsky.social
STATE=/var/lib/socialweb-computer/swc-bidder

scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
C=$(git rev-parse HEAD)
sudo mkdir -p /var/tmp/swc-release
sudo install -m 0755 /var/tmp/swc/bin/socialweb-computer /var/tmp/swc-release/socialweb-computer-${C:0:8}
DEV=/var/tmp/swc-release/socialweb-computer-${C:0:8}
SHA=$(sha256sum "$DEV" | cut -d' ' -f1)
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=$STATE/config.json \
  "$STATE/bin/current/socialweb-computer" \
  upgrade --binary "$DEV" --sha256 "$SHA" --allow-dev-binary
sudo "$DEV" install bidder --handle "$HANDLE" --allow-dev-binary
```

A core built for the Go-native shape cannot adopt a workspace whose type is the old `denoruntime`;
the install then reports `spec.type: Invalid value: name is immutable`. Purge and install fresh, as
above, when that happens.
