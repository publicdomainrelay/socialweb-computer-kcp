# Contributing

Read [AGENTS.md](AGENTS.md) first: working rules, edit discipline, live-run conventions.
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) holds decisions and invariants; plans and backlog live in
[docs/plans/](docs/plans/) and [NEXT.yaml](NEXT.yaml).

Work in a plain git worktree or branch. Commit small logical commits and push as you go. Live work
runs as the VM account (`test-0001`) from a dev build and a mirror that account can read.

Commands marked "variables carry over" use values set in the block above them.

## Dev build

Build the working tree, uncommitted changes included:

```bash
cd "$(git rev-parse --show-toplevel)"
scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
export PATH=/var/tmp/swc/bin:$PATH
socialweb-computer version
```

Artifacts:

| Path | Holds |
| --- | --- |
| `/var/tmp/swc/bin/socialweb-computer` | Host binary from `--publish`. |
| `/var/tmp/swc/src` | Mirror of the working tree, readable by every account. |
| `$HOME/.tmp/socialweb-computer-build/dev/<worktree>/socialweb-computer` | Host binary without `--publish`. |
| `$HOME/.tmp/socialweb-computer-build/dev/<worktree>/<goos>_<goarch>/socialweb-computer` | Per-platform binary. |

A dev binary prints `dev build, not reproducible` with its commit and dirty state.

Options:

| Option | Effect |
| --- | --- |
| `--dev` | Build the working tree for the host platform. |
| `--publish DIR` | Copy the host binary to `DIR/bin/socialweb-computer`, mirror the tree to `DIR/src`, and run from `DIR/src`. |
| `--sync-only` | With `--publish`, mirror the tree again without rebuilding. |
| `--platform P` | `linux/amd64` or `linux/arm64`; repeat for more. |
| `--commit REV` | Commit to build. Default `HEAD`; uncommitted changes are not built. |
| `--build-root DIR` | Checkouts, caches and outputs. Default `$HOME/.tmp/socialweb-computer-build`. |
| `--update-pins` | Rebuild owned assets and write new pins to `internal/versions/versions.json`. `make` passes it. |

A code change in a running market network. Every component runs in the core process, so a component
change needs a new core binary; `--sync-only` only re-mirrors the tree and does not build one:

```bash
cd "$(git rev-parse --show-toplevel)"
scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
sudo -n -u test-0001 env HOME=/home/test-0001 XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
  /var/tmp/swc/bin/socialweb-computer dev restart mn1 core --binary /var/tmp/swc/bin/socialweb-computer
```

`dev restart [<instance>] [<app>/]core` replaces the core of an instance, or of a market network
child application with `<app>/`; replace `mn1` with yours. `dev restart <instance>
[<app>/][<workspace>:][<kind>/]<component>` restarts one component through its kcp object without
a new binary.

## Release build

`make` builds both release platforms and updates owned asset pins:

```bash
cd "$(git rev-parse --show-toplevel)"
make
C=$(git rev-parse HEAD)
ls "$HOME/.tmp/socialweb-computer-build/release/$C"
```

Artifacts under `$HOME/.tmp/socialweb-computer-build/release/<commit>/`:

| Path | Holds |
| --- | --- |
| `socialweb-computer-linux-amd64` | Host binary, Linux amd64. |
| `socialweb-computer-linux-arm64` | Host binary, Linux arm64. |
| `SHA256SUMS` | sha256 of both binaries. |
| `release.json` | Commit, build mode, one entry per platform. |
| `version-linux-<arch>.json`, `.txt` | Pinned version and sha256 of every embedded asset. |

Build one commit and platform:

```bash
cd "$(git rev-parse --show-toplevel)"
C=$(git rev-parse HEAD)
scripts/build-socialweb-computer.sh --platform linux/amd64 --commit "$C"
B=$HOME/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64
SHA=$(sha256sum "$B" | cut -d' ' -f1)
echo "$B $SHA"
```

If `make` changed `internal/versions/versions.json`, commit it with the source change.

`B` and `SHA` are what `install` and `upgrade --binary --sha256` take. CI publishes the same names to
the public bucket: `releases/<tag>/` on a `v*` tag, `main/<commit>/` and `main/latest/` on `main`.
The guest image is published once per pin, to `guest-images/<target>/<sha256 of the pinned image.json>/`,
and CI fetches it from there instead of rebuilding it when its pins did not change.

Copy a release to another host:

```bash
# variables carry over: C, B
R=$(dirname "$B")
DEST=user@host
scp "$B" "$R/SHA256SUMS" "$DEST:/var/tmp/"
ssh "$DEST" 'cd /var/tmp && grep " socialweb-computer-linux-amd64$" SHA256SUMS | sha256sum -c -'
```

Then install or upgrade there:

```bash
# variables carry over: DEST
SHA=$(ssh "$DEST" "awk '\$2 == \"socialweb-computer-linux-amd64\" {print \$1}' /var/tmp/SHA256SUMS")
ssh "$DEST" 'sudo install -m 0755 /var/tmp/socialweb-computer-linux-amd64 /usr/local/bin/socialweb-computer'
ssh "$DEST" "sudo /usr/local/bin/socialweb-computer install bidder --handle <handle>"
```

## Fresh host

| Need | Ubuntu / Debian | Fedora / RHEL |
| --- | --- | --- |
| `newuidmap`, `newgidmap` for per-VM user namespaces | `uidmap` | `shadow-utils` |
| `ssh`, `ssh-keygen` | `openssh-client` | `openssh-clients` |
| `curl` | `curl` | `curl` |
| zoneinfo for embedded postgres | `tzdata` | `tzdata` |
| `jq` for examples | `jq` | `jq` |

```bash
# Ubuntu / Debian
sudo apt-get update && sudo apt-get install -y uidmap openssh-client curl tzdata jq
# Fedora / RHEL
# sudo dnf install -y shadow-utils openssh-clients curl tzdata jq
```

Host requirements:

- 64-bit Linux, `/dev/kvm`, `/dev/net/tun`, systemd 253+, cgroup v2.
- `/dev/kvm` read-write for the `kvm` group.
- One `/etc/subuid` and `/etc/subgid` range of `N * 65536` ids for the bidder account
  (`install bidder --vm-slots N`, default 256).
- A gateway host also runs Caddy with `/etc/caddy/sites/*.caddy` imported from `/etc/caddy/Caddyfile`.

`install bidder` and `install gateway` create their accounts (`swc-bidder`, `socialweb`), subuid ranges,
units and linger. Do not create them by hand for a production install. For a dev host, create the
`test-0001` account once:

```bash
sudo useradd -m -s /bin/bash -G kvm -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 test-0001
sudo usermod --add-subuids 100000-1148575 --add-subgids 100000-1148575 test-0001
sudo install -d -m 0700 -o test-0001 -g test-0001 ~test-0001/.ssh
sudo install -m 0600 -o test-0001 -g test-0001 ~/.ssh/authorized_keys ~test-0001/.ssh/
sudo loginctl enable-linger test-0001
```

Log in with `ssh test-0001@<host>` or `sudo machinectl shell test-0001@`. `sudo -u` and `runuser` give
no `/run/user/<uid>` session.

After an install, check the host contract as the account:

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer \
  check-host -role bidder -state-dir /var/lib/socialweb-computer/swc-bidder
sudo runuser -u socialweb -- env XDG_RUNTIME_DIR=/run/user/$(id -u socialweb) \
  /var/lib/socialweb-computer/socialweb/bin/current/socialweb-computer \
  check-host -role gateway -state-dir /var/lib/socialweb-computer/socialweb
```

## Install a release bidder

From the public bucket, with no credentials:

```bash
HANDLE=<your atproto handle>
BUCKET=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
V=$(curl -fsSL https://api.github.com/repos/publicdomainrelay/socialweb-computer-kcp/tags \
  | jq -r '.[].name' | grep '^v' | sort -V | tail -1)
ARCH=linux-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
mkdir -p /var/tmp/swc-release && cd /var/tmp/swc-release
curl -fLO "$BUCKET/releases/$V/binaries/socialweb-computer-$ARCH"
curl -fLO "$BUCKET/releases/$V/binaries/SHA256SUMS"
grep " socialweb-computer-$ARCH$" SHA256SUMS | sha256sum -c -
sudo install -m 0755 "socialweb-computer-$ARCH" /usr/local/bin/socialweb-computer
sudo /usr/local/bin/socialweb-computer install bidder --handle "$HANDLE"
```

Artifacts and state:

| Item | Path |
| --- | --- |
| Unit | `socialweb-computer-bidder@<handle>.service` in `swc-bidder`'s user manager. |
| State | `/var/lib/socialweb-computer/swc-bidder`. |
| kcp admin kubeconfig | `/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp/admin.kubeconfig` (root only). |
| Installed binary | `<state>/bin/current/socialweb-computer`; previous at `bin/previous`. |
| Config | `<state>/config.json`; env file `<state>/gateway.env` for a gateway. |

`install bidder` is idempotent. A re-run with another `--handle` adds an instance. A re-run after a
changed config or unit restarts the core. `--dry-run` prints the step list and changes nothing.

Install a locally built release binary:

```bash
# variables carry over: C, B
SHA=$(sha256sum "$B" | cut -d' ' -f1)
sudo "$B" install bidder --handle <handle>
```

### Bidder association

A fresh install creates a new DID. Association is one `badgeBlueKeys` record in the operator's repo
with `challenge` the operator DID and `keyId` the bidder DID, service `bidder_associate`. The
`AtprotoRecordWatch/association` of the application reads it and the identity controller writes the
operator DID into `MarketIdentity.status.associations`; no manual patch is needed. The helper
reads the handle and password from a mode-0600 credentials file and prints neither.

```bash
HANDLE=bobvmbuilder.bsky.social
ACCOUNT=BOB
CREDS=$HOME/.bobvmbuilder_bsky_social_source
STATE=/var/lib/socialweb-computer/swc-bidder
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
BIDDER_DID=$(sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.did}')
deno run -A --no-lock scripts/associate-bidder.ts --creds "$CREDS" --account "$ACCOUNT" --bidder-did "$BIDDER_DID"
sudo kubectl --kubeconfig "$K" get marketidentity bidder \
  -o jsonpath='{.status.phase}{" "}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Wait for `Associated=True`. The bidder side declaration `AtprotoRecord/association` is rendered and
reconciled through the app's PDS.

A requester uses the same helper with its own service and DID. The operator writes it once in the
operator's repo; the gateway's `AtprotoRecordWatch/association` (service `requester_associate`)
verifies it and the identity controller approves the requester automatically. No manual
`approvedAssociations` patch is needed. The gateway kcp is under its state dir (default
`/var/lib/socialweb-computer/socialweb`); the requester MarketIdentity lives in the workspace
`root:app`.

```bash
ACCOUNT=ALICE
CREDS=$HOME/.aliceoa_bsky_social_source
KGW=/var/lib/socialweb-computer/socialweb/kcp/admin.kubeconfig
GW=https://127.0.0.1:6443/clusters/root:app
REQUESTER_DID=$(sudo kubectl --kubeconfig "$KGW" --server "$GW" get marketidentity requester -o jsonpath='{.status.did}')
deno run -A --no-lock scripts/associate-bidder.ts --creds "$CREDS" --account "$ACCOUNT" --requester-did "$REQUESTER_DID"
sudo kubectl --kubeconfig "$KGW" --server "$GW" get marketidentity requester \
  -o jsonpath='{.status.phase}{" "}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Non-association mode: set the identity association to `Skip` and let the controller reconcile it. No
`badgeBlueKeys` record is written.

```bash
STATE=/var/lib/socialweb-computer/swc-bidder
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
sudo kubectl --kubeconfig "$K" patch marketbidder app --type=merge \
  -p '{"spec":{"identity":{"association":"Skip"}}}'
sudo kubectl --kubeconfig "$K" get marketidentity bidder \
  -o jsonpath='{.status.phase}{" "}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

### Bidder on a real account (oauthQr)

Run the bidder as an existing atproto account instead of its own local PDS. The account's repo holds
the market records; the bidder keeps no local Pds. Add a systemd drop-in so the mode survives
`install bidder` and `upgrade`:

```bash
STATE=/var/lib/socialweb-computer/swc-bidder
HANDLE=bobvmbuilder.bsky.social
DROP=$STATE/.config/systemd/user/socialweb-computer-bidder@$HANDLE.service.d/10-oauth-qr.conf
sudo -u swc-bidder mkdir -p "$(dirname "$DROP")"
sudo -u swc-bidder tee "$DROP" >/dev/null <<EOF
[Service]
ExecStart=
ExecStart=$STATE/bin/current/socialweb-computer run bidder --handle $HANDLE --fg -- --atproto-oauth-qr
EOF
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user daemon-reload
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user restart socialweb-computer-bidder@$HANDLE.service
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
until sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.pendingAction.url}' 2>/dev/null | grep -q .; do sleep 3; done
U=$(sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.pendingAction.url}')
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source --account BOB --url "$U"
sudo kubectl --kubeconfig "$K" get marketidentity bidder \
  -o jsonpath='{.spec.mode}{" "}{.status.phase}{" "}{.status.did}{" "}{.status.pdsUrl}{"\n"}'
```

Expect `oauthQr Ready <the account DID> <the account PDS>` and `kubectl get pds` to return no
resources. The QR url expires after 5 minutes.

### Upgrade, reinstall, uninstall

Upgrade an installed bidder as its account, then install again:

```bash
C=$(git rev-parse HEAD)
BIN=$HOME/.tmp/socialweb-computer-build/release/$C/socialweb-computer-linux-amd64
SHA=$(sha256sum "$BIN" | cut -d' ' -f1)
STATE=/var/lib/socialweb-computer/swc-bidder
sudo install -d -m 0755 /var/tmp/swc-release
sudo install -m 0755 "$BIN" /var/tmp/swc-release/socialweb-computer-${C:0:8}
STAGED=/var/tmp/swc-release/socialweb-computer-${C:0:8}
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=$STATE/config.json \
  "$STATE/bin/current/socialweb-computer" upgrade --binary "$STAGED" --sha256 "$SHA"
sudo "$STAGED" install bidder --handle <handle>
```

Upgrade keeps the state dir, OpenBao, DID and association. `rollback` swaps `bin/current` and
`bin/previous` and restarts the core:

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=/var/lib/socialweb-computer/swc-bidder/config.json \
  /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer rollback
```

Reinstall: run the same `install bidder` with the new binary. Completed steps are skipped; a changed
config or unit restarts the core. If install refuses because `bin/current` is not the binary you
invoked, run `upgrade --binary --sha256` first, then install again.

Uninstall, keeping state:

```bash
sudo /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer uninstall bidder
```

Uninstall and purge (removes the state dir, VMs, PDS repo, OpenBao and the account install created):

```bash
sudo /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer uninstall bidder --purge
```

`--force` proceeds while VM units are active; each stopped VM becomes `Lost`. `--json` prints the
result. `--account NAME` selects another account.

A fresh install over an old-shape workspace must purge first: the `app` WorkspaceType name is
immutable. Back up the state dir and Caddy sites before a purge. After a purge the DID is new and the
association is needed again. A purge keeps an account that install did not create (an adopted
account) but removes the install record, so the next `install bidder` on that host needs
`--adopt-account`. A shape change of the kcp storage (a new kcp build) also needs a purge: the new
kcp does not read the old store.

### Dev build on a bidder host

A dev binary needs `--allow-dev-binary`. Add `--adopt-account` only when install adopts an account that
has no install record (as after a purge that kept the account). `--force` only if uninstall refuses on
active VM units.

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
sudo "$DEV" uninstall bidder --purge --json
sudo "$DEV" install bidder --handle "$HANDLE" --allow-dev-binary --adopt-account \
  --state-dir "$STATE" --guest-image-source file:///var/lib/socialweb-computer/guest-image --dry-run
sudo "$DEV" install bidder --handle "$HANDLE" --allow-dev-binary --adopt-account \
  --state-dir "$STATE" --guest-image-source file:///var/lib/socialweb-computer/guest-image
```

If the install record and old shape are compatible, upgrade instead:

```bash
# variables carry over: HANDLE, STATE, DEV, SHA
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  CONFIG_PATH_SOCIALWEB_COMPUTER=$STATE/config.json \
  "$STATE/bin/current/socialweb-computer" upgrade --binary "$DEV" --sha256 "$SHA" --allow-dev-binary
sudo "$DEV" install bidder --handle "$HANDLE" --allow-dev-binary
```

## Deploy the gateway

`scripts/deploy.sh` installs on a fresh host or upgrades an installed gateway, ships `gateway.env` and
the Gateway spec, writes the Caddy sites, hands over the Caddy certificates, validates Caddy and
reloads it.

Fresh or routine gateway deploy:

```bash
cd "$(git rev-parse --show-toplevel)"
BINARY=$HOME/.tmp/socialweb-computer-build/release/$(git rev-parse HEAD)/socialweb-computer-linux-amd64
SHA=$(sha256sum "$BINARY" | cut -d' ' -f1)
SSH_KEY=~/.ssh/id_ed25519_no_password \
SSH_TARGET=root@socialweb.computer SSH_PORT=1997 \
ACCOUNT=socialweb STATE_DIR=/var/lib/socialweb-computer/socialweb \
GATEWAY_ENV_FILE=$HOME/.tmp/deploy/gateway.env \
GATEWAY_SPEC_FILE=$HOME/.tmp/deploy/gateway-spec.yaml \
BINARY=$BINARY SHA256=$SHA scripts/deploy.sh
```

`gateway.env` and the Gateway spec are required on a fresh host; an existing host may omit them and
keep the files in its state. The script refuses a digest mismatch. If `BINARY` is unset it builds the
release of `HEAD`.

Environment variables:

| Variable | Meaning | Default |
| --- | --- | --- |
| `BINARY` | Release binary to ship. | Build `HEAD`. |
| `SHA256` | Published sha256 of `BINARY`. | From `SHA256SUMS` when the script builds. |
| `SSH_KEY` | SSH key. | |
| `SSH_TARGET` | Deploy host. | `root@socialweb.computer` |
| `SSH_PORT` | SSH port. | `1997` |
| `ACCOUNT` | Gateway account. | `socialweb` |
| `STATE_DIR` | Gateway state dir. | `/var/lib/socialweb-computer/socialweb` |
| `DOMAIN` | Base domain. | `socialweb.computer` |
| `ORIGIN` | Web app host. | `k8s.<DOMAIN>` |
| `KCP_ORIGIN` | kcp host. | `kcp.<DOMAIN>` |
| `GATEWAY_ENV_FILE` | Shipped as `$STATE_DIR/gateway.env`. | |
| `GATEWAY_SPEC_FILE` | Shipped as `/var/lib/socialweb-computer/gateway-spec-$ACCOUNT.yaml`. | |
| `ALLOW_DEV_BINARY` | Pass `--allow-dev-binary` to `install gateway` and `upgrade`. | off |

Caddy certificate handoff (inside `scripts/deploy.sh`, after the gateway answers):

- Reads the kcp CA from the gateway's admin kubeconfig into `/etc/caddy/socialweb-computer/kcp.crt`.
- Copies `$STATE_DIR/tls/tls.crt` to `gateway.crt`.
- Copies `$STATE_DIR/tls/edge-client.crt` to `edge-client.crt` and its key (root:caddy, 0640) to
  `edge-client.key`.
- Writes `/etc/caddy/sites/k8s.<domain>.caddy` and `/etc/caddy/sites/kcp.<domain>.caddy`, imports the
  sites directory, adds `skip_install_trust`, runs `caddy validate` and reloads or restarts Caddy.

Verify:

```bash
curl -fsS https://k8s.socialweb.computer/api/config | jq
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl status caddy --no-pager
sudo ls -l /etc/caddy/socialweb-computer
```

`k8s.<domain>` proxies to the gateway API front on `127.0.0.1:8788`. `kcp.<domain>` proxies to kcp
`6443` without a client certificate and to the certificate front `8444` with one.

After a fresh deploy the requester has no gateway session, so cluster claims log
`no oauth session stored`. Sign in with `scripts/oauth-signin.ts --creds <file> --account ALICE
--url https://k8s.socialweb.computer/ --start-api`.

Upgrade a gateway: run the same `scripts/deploy.sh` with the new `BINARY` and `SHA256`. It runs
`upgrade --binary --sha256` as the account, then `install gateway` again with `bin/current`. A routine
upgrade keeps state, kcp objects, OpenBao, the requester DID, association, web sessions and running
components. A fresh install keeps nothing.

Uninstall the gateway and purge its state:

```bash
sudo /var/lib/socialweb-computer/socialweb/bin/current/socialweb-computer uninstall gateway --purge
```

The deploy inputs live on the orphan branch `swc-deploy`, in the worktree `~/.tmp/deploy`.
Credential files `~/.aliceoa_bsky_social_source` and `~/.bobvmbuilder_bsky_social_source` are mode
0600. Never print, log or commit their values.

## PDS ingress provider

A production PDS is a public HTTPS service. Its account DID document carries an `atproto_pds` service
entry whose `serviceEndpoint` is scheme, host and optional port only. The PDS `/.well-known/did.json`,
`createAccount` and `requestCrawl` use the same public host. The PDS may bind loopback; an
`IngressProxy` provides the public URL.

Every app root selects the provider with `spec.ingress`. The production profiles (`prod-qr`,
`market-network`, the requester default, gateway `prod`) default to `xrpc-subscription` with
`ingressProxyHost: xrpc.fedproxy.com` and `IngressProxy/ingress`. `xrpc.fedproxy.com` gives a stable
`https://<did-subdomain>.<host>` while the ingress identity keypair persists.

XRPC subscription:

```yaml
spec:
  ingress:
    provider: xrpc-subscription
    ingressRef:
      kind: IngressProxy
      name: ingress
    publicHost: example.fedproxy.com
    xrpc:
      ingressProxyHost: xrpc.fedproxy.com
      relayURL: https://relay.example
```

OpenTunnel (optional; stable route name required):

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

Selection rules:

- `xrpc-subscription` when `ingress.xrpc.ingressProxyHost` is set.
- `opentunnel` when `ingress.opentunnel` is set.
- Otherwise fail closed. There is no implicit local fallback and no `GuestTlsUrl` override.
- `iroh` is recognized by the API and rejected until a later plan.
- A missing provider, command, route name or local host is a named failure.
- A provider that returns a different public URL on restart is `StableHostMismatch`.
- Set `Pds.spec.requireIngress` for every production PDS. No account is created and no sign-in
  completes while the ingress is not Ready.
- The first public URL is persisted in `Pds.spec.publicURL` and `Pds.spec.stableHost`.

On `run bidder` and `run request`, `--ingress-provider`, `--ingress-public-host`,
`--ingress-xrpc-host`, `--ingress-relay-url`, `--ingress-opentunnel-command`,
`--ingress-opentunnel-route-name`, `--ingress-opentunnel-local-host` and
`--ingress-opentunnel-proxy-protocol` after `--` override the profile defaults. A gateway spec that
sets `spec.ingress.opentunnel` selects OpenTunnel. A `MarketNetwork` publishes one
`IngressProxy/dispatcher` in `status.network.ingress`; children inherit it.

Known gap (plan 0022): OpenTunnel is wired but not yet proven on a prod host. Check
`Pds.spec.publicURL`, the ingress `Exposure` and the DID document before trusting a standalone PDS
whose host does not resolve.

### Market transport modes

`spec.comms.mode` is `XRPC`, `Firehose` or `Both`; the requester defaults to `Both` and a bidder to
`XRPC`. PDS writes happen in every mode.

An external relay or firehose is an `EventSource` over a `RelayEventSource` (or a
`JetstreamEventSource`) whose `spec.url` is the endpoint; the root's `spec.comms.eventSourceRefs`
names those EventSources.

In `Firehose` and `Both` the requester composes the public sources itself. `spec.comms.relayUrls`
defaults to our relay `https://relay.mini-cloud-0002.chadig.com` (a market network's relay replaces
it), and `spec.comms.jetstreamUrls` defaults to the two Jetstream v2 instances,
`wss://jetstream.us-east.bsky.network` and `wss://jetstream.us-west.bsky.network` at
`/xrpc/network.bsky.jetstream.subscribeEvents`. An empty list turns that source off. A bidder
composes no per-bidder stream; while it has live ComputeVMs it composes requester streams from those
requesters' DIDs, and resumes one from its stored cursor only when the cursor was saved in the last
10 minutes.

```bash
K=$(sudo sh -c "ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig" | head -1)
sudo kubectl --kubeconfig "$K" apply -f - <<'YAML'
apiVersion: events.socialweb.computer/v1alpha1
kind: RelayEventSource
metadata: {name: relay-example}
spec: {url: https://relay.example}
---
apiVersion: events.socialweb.computer/v1alpha1
kind: EventSource
metadata: {name: relay-example}
spec: {backendRef: {group: events.socialweb.computer, kind: RelayEventSource, name: relay-example}}
YAML
# Firehose only: no submit XRPC; events come from the named EventSource
socialweb-computer run bidder --host-backend proc --profile prod-qr --handle <handle> \
  -- --comms-mode Firehose --comms-event-source relay-example
# Both: XRPC first, then the public sources and the named EventSource
socialweb-computer run request --name demo \
  -- --comms-mode Both --comms-event-source relay-example
```

A bidder in Firehose or Both needs a source (a named EventSource or the network relay); otherwise
the root fails closed instead of falling back to XRPC. Every market EventStream of a role delivers to
one sink, which drops a `uri|cid` it took in the last 15 s and logs
`market_event_duplicate_dropped`. The `StateSync` inboxes are not the dedup fact: the requester keeps
only records of its own ComputeContracts and the bidder only RFPs, accepts whose bid is in its repo
and events whose receipt is in its repo. The requester prunes the inbox of a Released or Failed
contract 10 minutes after it is Acked; the bidder prunes every Acked inbox after 10 minutes.
Switching back to XRPC deletes the root's market EventStreams.


### Account modes

| `spec.identity.mode` | What runs | Association |
| --- | --- | --- |
| `localRepo` (default) | Local `Pds` component and a new DID | `RequireApproval` or `Skip` |
| `pdsAccount` | Existing account on a `RemotePDS`; password in OpenBao | None |
| `oauthBrowser` | Existing account signed in from a browser callback | None |
| `oauthQr` | Existing account signed in by scanning a QR | None |
| `oauthDelegated` | Existing account signed in by a delegated flow | None |
| `oauthSessionProxy` | The gateway's per-account session: `spec.did` and a `spec.sessionRef` to that session in the vault | None |

`run bidder` and `run request` do not set `oauthSessionProxy`; a cluster claim uses it to act as the
signed-in account.

```bash
# Managed, unassociated: patch the root after it exists (or use run request -- --skip-qr)
K=$(sudo sh -c "ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig" | head -1)
sudo kubectl --kubeconfig "$K" patch marketbidder app --type=merge \
  -p '{"spec":{"identity":{"association":"Skip"}}}'
# Existing account on another PDS (password comes from ATPROTO_PASSWORD)
export ATPROTO_PASSWORD=...   # never echo it
socialweb-computer run bidder --host-backend proc --profile prod-qr --handle <handle> \
  -- --atproto-identity-mode pdsAccount --atproto-pds-url https://pds.example
# OAuth browser
socialweb-computer run request --name demo \
  -- --atproto-identity-mode oauthBrowser --atproto-pds-url https://pds.example
```

`pdsAccount` fails closed when it names no PDS. OAuth modes fail closed without a `pdsUrl` or a
resolvable handle. Tokens stay in OpenBao; `MarketIdentity.status` carries refs only.

The prod CLI requester (`run request` without `--network`) signs in with `-- --atproto-oauth-qr`:
the MarketIdentity `requester` reads and writes its oauth session in the invocation's own
OpenBaoServer `requester` (mount `requester`, `identity/requester/pds-session`), asks for the
requester role scopes (guest.* included) through qr.fedfork.com, and the CLI prints, on stderr,
`identity requester is AwaitingScan (no DID yet): scan the qr url to sign in` and then
`identity requester pendingAction scanQr url <url> expiresAt <RFC 3339>`, again for every new URL.
The operator signs in with `scripts/oauth-signin.ts --url "<url>"` (AGENTS.md, Prod deployments).
It waits without a deadline once the identity waits on a person; Ctrl-C stops it before any
contract is posted. The default `localRepo` makes a new DID per invocation and waits in
`AwaitingApproval` for the operator's claim (`scripts/associate-bidder.ts --requester-did <did>`;
the CLI prints the command with the DID); `-- --skip-qr` skips it but leaves the requester with no
operator, so `requester-tangled-vouch` vouches no prod bidder.


### Verify public PDS, DID and requestCrawl

```bash
HANDLE=bobvmbuilder.bsky.social
DID=$(curl -fsS "https://public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle?handle=$HANDLE" | jq -r .did)
PDS=$(curl -fsS "https://plc.directory/$DID" | jq -r '.service[] | select(.id == "#atproto_pds") | .serviceEndpoint')
HOST=${PDS#https://}; HOST=${HOST%%/*}
echo "$DID $PDS $HOST"
curl -fsS "$PDS/.well-known/did.json" | jq '{id, service}'
curl -fsS "$PDS/xrpc/com.atproto.repo.listRecords?repo=$DID&collection=com.publicdomainrelay.temp.badgeBlueKeys" | jq '.records | length'
curl -fsS -X POST "$PDS/xrpc/com.atproto.sync.requestCrawl" -H 'Content-Type: application/json' \
  -d "{\"hostname\":\"$HOST\"}" -o /dev/null -w '%{http_code}\n'
```

`serviceEndpoint` must be HTTPS with no path. The last command returns 200; confirm the crawl in the
PDS journal:

```bash
sudo journalctl _SYSTEMD_USER_UNIT='socialweb-computer-bidder@<handle>.service' --since -5min | grep -i requestCrawl
```

## hat-9 and hat-7

SSH aliases exist on the lead's workstation: `ssh hat-9`, `ssh hat-7`, `ssh socialweb.computer`.
No credentials belong in this document; use your own SSH key.

| Host | State root | Bidder DID | Association |
| --- | --- | --- | --- |
| this host | `/var/lib/socialweb-computer/swc-bidder` | `did:plc:vxd7pmlhw6bnpyu6wngewthn` | `Associated`, `RequireApproval` |
| hat-9 | `/var/lib/socialweb-computer/swc-bidder` | `did:plc:cjuftobfmmmdkmblcl4h54eb` | `Associated`, `RequireApproval` |
| hat-7 | `/var/lib/socialweb-computer/swc-bidder` | `did:plc:exjutifioeu2p7oorbdrex2o` | `AssociationSkipped`, `Skip` |

The bidder state dir is root-readable only:

```bash
ssh hat-9
sudo ls /var/lib/socialweb-computer/swc-bidder/handles
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user status 'socialweb-computer-bidder@<handle>.service'
```

## Troubleshooting

Units, logs and kcp status. The unit account for a bidder is `swc-bidder`, for a gateway `socialweb`.

```bash
# units as the account (the unit itself reads the backend and state from config.json)
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer units list \
  --host-backend systemd-user --state-dir /var/lib/socialweb-computer/swc-bidder
# bidder unit and journal (system journal: swc-bidder is a system account)
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user status 'socialweb-computer-bidder@<handle>.service'
sudo journalctl _SYSTEMD_USER_UNIT='socialweb-computer-bidder@<handle>.service' --since -10min
# gateway unit and journal
sudo systemctl --user -M socialweb@ status socialweb-computer-gateway.service
sudo journalctl --user -M socialweb@ -u socialweb-computer-gateway.service --since -10min
# kcp status (there is no kubectl kcp plugin in this tree; use the admin kubeconfig)
K=$(sudo sh -c 'ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig' | head -1)
sudo kubectl --kubeconfig "$K" get --raw /readyz
sudo kubectl --kubeconfig "$K" get workspaces,marketbidder,marketidentity,pds,exposure
sudo kubectl --kubeconfig "$K" get marketbidder app -o jsonpath='{.status.phase}{"\n"}'
sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.spec}{"\n"}{.status}{"\n"}'
# a dev instance
sudo -n -u test-0001 env HOME=/home/test-0001 XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
  /var/tmp/swc/bin/socialweb-computer dev status --instance mn1
sudo -n -u test-0001 env HOME=/home/test-0001 XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
  /var/tmp/swc/bin/socialweb-computer dev logs core --instance mn1 --tail 50
# gateway's embedded k3s on a host without kubectl
K3S=$(sudo find /var/lib/socialweb-computer/socialweb/runtime -name k3s -type f | head -1)
sudo "$K3S" kubectl get ns
```

Paths:

| Item | Path |
| --- | --- |
| Bidder state | `/var/lib/socialweb-computer/swc-bidder` |
| Gateway state | `/var/lib/socialweb-computer/socialweb` |
| kcp admin kubeconfig | `<state>/handles/<hash>/kcp/admin.kubeconfig` |
| Tenant k3s kubeconfig | `<state>/.../server/cred/admin.kubeconfig` |
| Core binary | `<state>/bin/current/socialweb-computer` |
| Previous binary | `<state>/bin/previous/socialweb-computer` |

## Dev market network

Run as the VM account on a host prepared above. The first `dev up` fetches and verifies the guest
image unless `--guest-image-source` names one.

```bash
cd "$(git rev-parse --show-toplevel)"
swc() {
  sudo -n -u test-0001 env HOME=/home/test-0001 \
    XDG_RUNTIME_DIR=/run/user/$(id -u test-0001) \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u test-0001)/bus \
    /var/tmp/swc/bin/socialweb-computer "$@"
}
swc dev up --instance mn1 --profile market-network --bidders 1 --with-gateway --vm-host-backend systemd-user
swc dev status --instance mn1 | jq '.network.status.apps'
swc status --instance mn1
eval "$(swc dev env --instance mn1)"
swc dev logs core --instance mn1 --tail 50
swc dev down --instance mn1
```

Rent a VM through the RFP flow:

```bash
# variables carry over: swc
sudo -n -u test-0001 tee /var/tmp/swc-secrets.json >/dev/null <<'JSON'
[{"path":"/root/x","value":"hello"}]
JSON
swc run request --network mn1 --secrets /var/tmp/swc-secrets.json --exec 'cat /root/x'
swc run request list --json | jq -r '.[].name'
swc run request release "$(swc run request list --json | jq -r '.[0].name')"
```

`run request` options: `--network`, `--name`, `--identity`, `--secrets`, `--tunnel`, `--exec`,
`--hold`, `--skip-ssh`, `--contract-timeout`, `--transport`, `--tag`; `run request ssh`, `resume` and
`proxy` also take `--contract` and `--log-level`. `--tunnel` is
`<tag>:<port>[:<transport>][:shell[=<envFile>]]`, transport `opentunnel` (default) or `webrtc`.

`--identity <name>` reuses one localRepo did:plc, repo and embedded PDS state across invocations
from `<state-root>/identities/<name>/`; the first run needs `-- --skip-qr`. Each invocation still gets
a fresh kcp and OpenBao. One PDS process per identity store is enforced; `ssh`/`release` attach to a
running kcp. `release` keeps the identity store; delete it manually only after the invocation is
released.

Run a bidder as a child application of the market network:

```bash
# variables carry over: swc
swc run bidder --host-backend proc --profile market-network --network mn1 --handle b2.mn1.bob.test
```

With `--fg` it stays until Ctrl-C, which removes it from the network. With `--network` the profile
defaults to `market-network`.

### Foreground bidder (no systemd)

[README: run in the foreground](README.md#run-in-the-foreground-no-systemd) has the prod command, the
prerequisites and the paths. `run bidder --host-backend proc` without `--network` is its own instance
`b-<handle-with-dashes>`: `bringUp` of a quick-profile instance under a devsup (die-with this process
with `--fg`), then the MarketBidder root object, the identity wait and the Ready wait. It runs the
bidder host checks of `check-host -role bidder` first, without the systemd ones.

Join a dev market network over the market protocol only, in its own kcp (the standalone path, with
no public PLC DID):

```bash
# variables carry over: swc
swc dev status --instance mn1 | jq .network.status > /var/tmp/swc/mn1.json
swc run bidder --market-network-file /var/tmp/swc/mn1.json --handle b3.mn1.bob.test --host-backend proc --fg
K=/home/test-0001/.local/state/socialweb-computer/dev/b-b3-mn1-bob-test/kcp/admin.kubeconfig
sudo -n -u test-0001 kubectl --kubeconfig "$K" patch marketbidder app --type=merge \
  -p '{"spec":{"identity":{"association":"Skip"}}}'
swc run request --network mn1 --exec 'uname -r'
swc status --instance b-b3-mn1-bob-test
```

Ctrl-C (or SIGTERM) on `run bidder` prints the `dev down --keep-state` report (`vmsStopped`,
`remaining: []`). `swc dev down --instance b-b3-mn1-bob-test` removes its state.

## Run the requester where the release assets are not staged

`run request` brings up its own kcp (kine over sqlite, the port-0 front, one kcp slot, the core) as
units of the proc placement backend (`devsup`, `hostsvc` backend `proc`), then posts the RFP from a
requester that runs in the core process. The backend has a darwin implementation, so the requester
runs on macOS natively; the bidder role does not (it needs the launchd backend and the
Virtualization.framework runtime, plan 0012). On a release platform, `scripts/build-socialweb-computer.sh
--dev` stages every pinned asset and the binary resolves them itself. This section is for the hosts
where that cannot run: macOS (not a release platform in `internal/versions/versions.json`), and a
Linux host whose asset staging fails on an unrelated pinned asset (the PGDG deb by-hash the pin
names has since been withdrawn, or its rustc is not rustup-managed). Give the four executables by
path instead.

### The four executables

| Executable | Why | Pin |
| --- | --- | --- |
| `kcp` | the API server of the invocation's kcp | built from the pinned commit; `kcp --version` must name the version `internal/versions/versions.json` pins |
| `kine` | the sqlite etcd shim under kcp | the release binary, its sha256 pinned |
| `bao` | the requester's own OpenBao (the vault phase of its set) | the release archive, verified against `checksums.txt` |
| `dumbpipe` | iroh ssh into the guest (`--transport iroh`, the default) | the release archive, the sha256 GitHub publishes for it is the pin |

`internal/versions/versions.json` carries the URL and the sha256 of each under `assets.<name>.fetch.<os>/<arch>`,
so a host reads them from the checkout it built:

```bash
v=internal/versions/versions.json
url=$(jq -r '.assets.kine.fetch["linux/amd64"].url' "$v")
sha=$(jq -r '.assets.kine.fetch["linux/amd64"].sha256' "$v")
curl -fsSL -o kine "$url" && echo "$sha  kine" | sha256sum -c -
```

kcp is built from source, `CGO_ENABLED=0`, with the version flags the pin names:

```bash
git clone https://github.com/kcp-dev/kcp
git -C kcp checkout 74e9fb9c03f778c57d1a6a782d5adc9476ec1af5
cd kcp && CGO_ENABLED=0 go build -trimpath -buildvcs=false -mod=readonly \
  -ldflags="-s -w -buildid= \
    -X k8s.io/client-go/pkg/version.gitVersion=v1.36.0+kcp-v0.33.0-162-g74e9fb9c03f778 \
    -X k8s.io/client-go/pkg/version.gitCommit=74e9fb9c03f778c57d1a6a782d5adc9476ec1af5 \
    -X k8s.io/client-go/pkg/version.gitTreeState=clean \
    -X k8s.io/component-base/version.gitVersion=v1.36.0+kcp-v0.33.0-162-g74e9fb9c03f778 \
    -X k8s.io/component-base/version.gitCommit=74e9fb9c03f778c57d1a6a782d5adc9476ec1af5 \
    -X k8s.io/component-base/version.gitTreeState=clean" \
  -o ../bin/kcp ./cmd/kcp
```

Two build facts for this binary: the generated deepcopy imports `encoding/json/jsontext`, so a Go
1.26 toolchain needs `GOEXPERIMENT=jsonv2` (Go 1.27 has it on); and `CGO_ENABLED=0` is enough and
avoids needing libc headers. `GOTOOLCHAIN=go1.26.6` (the version `go.mod` names) builds on a host
whose own Go differs.

The policy engine embeds no Deno outside a `-tags embed_deno` release build, so `deno` must be on
`PATH` when the PolicyEngine component starts (`internal/policyengine/deno`, pinned v2.9.3). Without
it the root stays `Provisioning` with `PolicyEngine/policy-engine ready=false phase=Pending`.

Then build this tree and run one invocation, with the four paths:

```bash
go build -o bin/socialweb-computer ./cmd/socialweb-computer
bin/socialweb-computer run request --name host1 \
  --kcp-path "$PWD/bin/kcp" --kine-path "$PWD/bin/kine" --bao-path "$PWD/bin/bao" --dumbpipe-path "$PWD/bin/dumbpipe" \
  --exec 'uname -a' -- --skip-qr
```

### macOS

`scripts/build-socialweb-computer.sh --dev` refuses darwin: it builds only the release platforms of
`versions.json`, and darwin/arm64 is a seam there with only vfkit pinned. Build the binary with plain
`go build` (Go 1.27 or newer) and pass the paths as above; the darwin kcp, kine (the cgo sqlite
build, `CGO_ENABLED=1 CGO_CFLAGS="-DSQLITE_USE_ALLOCA=1" go build -tags nats` of v0.17.1), bao
(`openbao_2.6.2_darwin_arm64.tar.gz`) and dumbpipe (`dumbpipe-v0.39.0-darwin-aarch64.tar.gz`) are
built or fetched once into `~/.tmp/swc-mac/bin`.

### Linux

On a release platform, `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>` stages
every pinned asset once and the binary resolves them itself, so `run request` then takes no path
options. When that staging fails on an asset the requester never uses (the PGDG deb the postgres pin
names has been withdrawn upstream; a rustc that is not rustup-managed stops the meshd downloads), use
the explicit-path recipe above, with `GOTOOLCHAIN` and `GOEXPERIMENT=jsonv2` as on Windows.

### Windows

Use WSL2: a native Windows requester would need a windows placement backend, and there is none yet
(plan 0011). Windows 11's WSL2 carries AF_UNIX, so the devsup, kine and front sockets work unchanged.
Copy the checkout into the distro, install the toolchain there, and run as a non-root user, because
dev refuses uid 0. In a WSL shell (`wsl.exe -d Ubuntu`), as root:

```bash
apt-get update && apt-get install -y golang-go jq zstd make
useradd -m -s /bin/bash swc
```

then, as that user (`su - swc`):

```bash
CGO_ENABLED=0 GOEXPERIMENT=jsonv2 GOTOOLCHAIN=go1.26.6 \
  go build -o ~/bin/socialweb-computer ./cmd/socialweb-computer
~/bin/socialweb-computer run request --name wsl1 \
  --kcp-path ~/bin/kcp --kine-path ~/bin/kine --bao-path ~/bin/bao --dumbpipe-path ~/bin/dumbpipe \
  --exec 'uname -a' -- --skip-qr
```

The distro's own `golang-go` is 1.26, hence `GOEXPERIMENT=jsonv2` and the toolchain pin; the Ubuntu
image's gcc has no libc headers, which `CGO_ENABLED=0` avoids. The four executables are the
linux/amd64 ones above, and `deno` has to be on `PATH` as on any other host.

### What differs on macOS

| Area | On macOS |
| --- | --- |
| `XDG_RUNTIME_DIR` | usually unset; the instance's runtime dir, its devsup socket and the front's control socket fall back to `<state-dir>/run` |
| front fd store | the proc backend offers none (`hostsvc` `Capabilities.FDStore` false), so the front binds its address itself and the kcp slot is restarted when the front is placed on a new port |
| unix socket paths | `sun_path` is 104 bytes, so the limit is 103, not 107 |
| bound ports | read from `lsof` (about 25 ms per read) instead of `/proc` |
| process identity | kinfo start time and `kern.boottime` instead of start ticks and `/proc` |
| child reaping | no subreaper; an orphaned grandchild reparents to launchd and the env-attribution sweep kills it |

### Reading the result

`kubectl --kubeconfig <state-dir>/requests/<name>/dev/<name>/kcp/admin.kubeconfig get marketrequester
app` reads the root (`Ready` when the pod-less stack is up); `get computecontract` shows the RFP it
posted. The requester's PDS, ingress proxy and policy engine run in the core process, so nothing
else is needed on the host. The RFP reaches public bidders through the ingress relay: the contract
leaves `Requesting` with a verified receipt, goes `Held` when the guest publishes its address, and
`--exec` runs over iroh ssh before the contract is released. `--skip-ssh` rents, reports and releases
without a session.

## Run a bidder that sells a deno sandbox

A bidder can offer a compute kind that is not a virtual machine: `computeKind: deno` in the contract
(the payload's `kind`) makes the bidder run `spec.userData` as a Deno program on the bidder's host and
report its output, instead of booting a firecracker guest. Nothing about that needs KVM, a guest image
or a VM runtime, so this is the bidder a macOS or WSL2 host can run.

The pieces: `kind` on `com.publicdomainrelay.temp.compute.vm` (absent reads as `vm`), `computeKind` on
the contract and the Bidder spec, the `DenoSandbox` backend kind on the ComputeVM, and the guest event
`com.publicdomainrelay.temp.compute.events.vm.sandbox` (`exitCode`, `timedOut`, `stdout`, `stderr`).
The sandbox runs `deno run --no-remote --no-lock --deny-net --deny-run --deny-ffi` with read and
write of its own work dir only, at most 2 minutes.

A bidder serves the kinds it is told to, and drops an RFP of a kind it does not serve before it bids:

```bash
run bidder --host-backend proc --compute-kinds deno --handle <handle> --fg \
  --state-dir <state> --kcp-path <kcp> --kine-path <kine> --bao-path <bao> \
  --openbao-plugin-dir <dir with both openbao plugins>
```

`--compute-kinds` takes `vm`, `deno` or both; the default is `vm`. One bidder serves both (`vm,deno`):
the kind of each accepted RFP decides whether a firecracker guest or a sandbox runs, and both count
against `maxConcurrentGuests`. Without `vm` the bidder opens no VM runtime, runs no KVM preflight, and
applies the bidder API sets itself (`internal/computevm` `ApplyBidderSets`), because the ComputeVM
controller that usually applies them is not started. A bidder with no `deno` on `PATH` (pinned v2.9.3)
declines deno RFPs rather than bidding on a kind it cannot run. On macOS the two OpenBao plugins need
the darwin pins that `internal/versions/versions.json` now carries for them.

The requester asks for the kind after `--`, with the program in the usual user-data file:

```bash
run request --name deno1 <the four paths> -- --skip-qr --compute-kind deno --user-data prog.ts
```

`--compute-kind deno` skips ssh (a sandbox has no guest to ssh into). The contract goes `Held` when
the sandbox has reported its result, and `summary.json` carries `contract.sandbox {exitCode, timedOut,
stdout, stderr}`. A program:

```ts
console.log(`hello from ${Deno.build.os}/${Deno.build.arch}`);
```

One market caveat: a bidder running older code does not read `kind`, so it still bids on a deno RFP
and, if it wins, boots a VM for it. On a market with other bidders, pin the requester to the deno
bidder with `--allow-bidder-dids <did>` (the bidder's `MarketIdentity bidder` status; `--deny-bidder-dids` is the
inverse).

Read on each host: macOS `sandbox://...` with `hello from darwin/arm64`, hat-7 and WSL2
`hello from linux/x86_64`, exit 0, release 200.

### The proof recipe (three hosts)

One request runner, copied to every host; it prints the fields a proof reads:

```bash
#!/usr/bin/env bash
# run-one.sh <kind> <name> <bidder-did> <userdata>; env SWC_BIN SWC_STATE KCP KINE BAO
set -e
kind="$1"; name="$2"; did="$3"; ud="$4"
extra=()
[ "$kind" = deno ] && extra=(--compute-kind deno)
"$SWC_BIN" run request --name "$name" --state-dir "$SWC_STATE" \
  --kcp-path "$KCP" --kine-path "$KINE" --bao-path "$BAO" \
  --ready-timeout 90s --network-timeout 180s --contract-timeout 5m --skip-ssh \
  -- --skip-qr --allow-bidder-dids "$did" "${extra[@]}" --user-data "$ud" \
  > "$SWC_STATE/$name.log" 2>&1
grep -E "\"phase\": \"(Held|Released|Failed)\"|exitCode|\"stdout\"|vmAddress|releaseStatus|winnerDid" "$SWC_STATE/$name.log" | tail -6 | cut -c1-190
```

Start a bidder per host (all in the foreground under devsup, which dies with the process):

```bash
# macOS, deno only
nohup ~/.tmp/swc-mac/bin/socialweb-computer run bidder --host-backend proc --compute-kinds deno \
  --handle probe.deno.test --fg --state-dir ~/.tmp/swc-mac/state \
  --kcp-path ~/.tmp/swc-mac/bin/kcp --kine-path ~/.tmp/swc-mac/bin/kine --bao-path ~/.tmp/swc-mac/bin/bao \
  --openbao-plugin-dir ~/.tmp/swc-mac/bin/plugins > ~/.tmp/swc-mac/bidder-mac-deno.log 2>&1 &

# hat-7, both kinds (VM assets at explicit paths; see the VM half below)
B=$HOME/.tmp/swc-req/bin
nohup "$B/socialweb-computer" run bidder --host-backend proc --compute-kinds vm,deno --handle probe.mixed.test --fg \
  --state-dir "$HOME/.tmp/swc-req/state-mixed" \
  --kcp-path "$B/kcp" --kine-path "$B/kine" --bao-path "$B/bao" --openbao-plugin-dir "$B/plugins" \
  --userns-stage-path "$B/vm/swc-userns-stage" --pasta-path "$B/vm/pasta" \
  --vmexec-path "$B/vm/socialweb-vmexec" --firecracker-path "$B/vm/firecracker" \
  > "$HOME/.tmp/swc-req/mixed.log" 2>&1 &

# desktop: WSL2, deno only, as a non-root user (dev refuses uid 0)
wsl.exe -d Ubuntu -u root -e bash -c 'su - swc -c "export PATH=/usr/local/bin:\$PATH; cd ~; nohup /var/tmp/swc-req/bin/socialweb-computer run bidder --host-backend proc --compute-kinds deno --handle probe.deno.test --fg --state-dir /home/swc/.tmp/swc-req/state --kcp-path /var/tmp/swc-req/bin/kcp --kine-path /var/tmp/swc-req/bin/kine --bao-path /var/tmp/swc-req/bin/bao --openbao-plugin-dir /var/tmp/swc-req/bin/plugins > /home/swc/bidder-deno.log 2>&1 < /dev/null & echo started"'
```

Wait for the root, and read the bidder DID for the allowlist:

```bash
KUBECONFIG=<state>/dev/<instance>/kcp/admin.kubeconfig kubectl get marketbidder app -o jsonpath='{.status.phase}'
KUBECONFIG=<state>/dev/<instance>/kcp/admin.kubeconfig kubectl get marketidentity bidder -o jsonpath='{.status.did}'
```

Then, on macOS and hat-7, run the matrix directly:

```bash
export SWC_BIN=$HOME/.tmp/swc-mac/bin/socialweb-computer SWC_STATE=$HOME/.tmp/swc-mac/state \
       KCP=$HOME/.tmp/swc-mac/bin/kcp KINE=$HOME/.tmp/swc-mac/bin/kine BAO=$HOME/.tmp/swc-mac/bin/bao
bash run-one.sh vm   m3-mac-vm   <bidder-did> $HOME/.tmp/swc-mac/vm.yaml
bash run-one.sh deno m3-mac-deno <bidder-did> $HOME/.tmp/swc-mac/prog.ts
```

On desktop the arguments go into the script, not through the command line: a nested call crosses ssh,
the MSYS bash, `wsl.exe` and `su -c`, and positional parameters are lost on the way.

```bash
# ~/swc-desk/wsl-deno.sh
wsl.exe -d Ubuntu -u root -e bash -c 'su - swc -c "export PATH=/usr/local/bin:\$PATH SWC_BIN=/var/tmp/swc-req/bin/socialweb-computer SWC_STATE=/home/swc/.tmp/swc-req/state KCP=/var/tmp/swc-req/bin/kcp KINE=/var/tmp/swc-req/bin/kine BAO=/var/tmp/swc-req/bin/bao; bash /home/swc/run-one.sh deno d-wsl-deno <bidder-did> /home/swc/prog.ts"'
ssh desktop 'bash ~/swc-desk/wsl-deno.sh' | tr -d '\r' | tail -5
```

The two user-data files:

```ts
// prog.ts, the program of a deno request
console.log(`hello from ${Deno.build.os}/${Deno.build.arch}`);
```
```yaml
# vm.yaml, the cloud-init of a vm request
#cloud-config
runcmd:
  - [ sh, -c, "echo vm-ok > /tmp/vm-ok" ]
```

Getting the hosts to that point: build the binary with the pinned toolchain (`CGO_ENABLED=0
GOEXPERIMENT=jsonv2 GOTOOLCHAIN=go1.26.6`, `cmd/socialweb-computer`), ship the tree with rsync to a
Linux host or a tar stream into the distro, and stage the two OpenBao plugins to a directory that
matches the pins.

The VM half on hat-7 needs four pinned assets, a subuid range for the account, and `/dev/kvm`:

```bash
scripts/stage-assets.sh --platform linux/amd64 --asset firecracker --asset pasta --blobs $HOME/.tmp/swc-req/vmblobs
scripts/stage-assets.sh --platform linux/amd64 --asset socialweb-vmexec --print-pins --blobs $HOME/.tmp/swc-req/vmblobs
scripts/stage-assets.sh --platform linux/amd64 --asset swc-userns-stage --blobs $HOME/.tmp/swc-req/vmblobs
for a in firecracker pasta socialweb-vmexec swc-userns-stage; do
  zstd -q -d -c $HOME/.tmp/swc-req/vmblobs/linux_amd64/$a.tar.zst | tar -x -C $HOME/.tmp/swc-req/bin/vm
done
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"
```

`--print-pins` is for a dirty tree: `socialweb-vmexec` embeds `internal/versions/versions.json`, so a
changed pin makes the built binary differ from its own pin and `stage-assets.sh` refuses it. The
staged binaries are what the live run uses, and the bidder takes them by path.

Each run keeps its log at `<state>/<name>.log`, so any result is re-read with
`grep -E "exitCode|vmAddress|releaseStatus" <state>/<name>.log`.

## Gate and acceptance

```bash
cd "$(git rev-parse --show-toplevel)"
deploy/check.sh --fast
```

`deploy/check.sh --fast` runs gofmt, go build, go vet, the generated-code drift check
(`go generate ./...`), the architecture rules (`scripts/check-architecture.sh`), the cross-OS builds
and the release builds. The full gate before a release adds the release closure and the live proofs
this host can run; read [AGENTS.md](AGENTS.md) "How to verify".

There are no Go tests until plan 0027. The scenarios the old integration suite covered, and the
acceptance rows of plan 0026, are in `docs/spec/integration-catalog.yaml`. A release candidate is
proven live with `docs/acceptance/0001-auth-modes.md`.

## See also

- [README](README.md)
- [Bidder architecture](BIDDER_ARCH.md)
- [Gateway architecture](GATEWAY_ARCH.md)
- [Inspect a kcp](docs/inspect.md)
- [Host contract](docs/host-contract.md)
