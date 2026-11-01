# The bidder: install, identity and upgrades

Short copy-paste commands are in [CONTRIBUTING.md](../CONTRIBUTING.md#install-a-release-bidder).
This page records the installed shape and the facts behind those commands.

## Release artifacts

Tagged releases publish Linux binaries and the guest image to a public bucket. No credentials.

| Path | Holds |
| --- | --- |
| `releases/<version>/binaries/socialweb-computer-linux-amd64` | Host binary, Linux amd64. |
| `releases/<version>/binaries/socialweb-computer-linux-arm64` | Host binary, Linux arm64. |
| `releases/<version>/binaries/SHA256SUMS` | sha256 of both binaries. |
| `releases/<version>/binaries/release.json` | Commit, build mode, one entry per platform. |
| `releases/<version>/binaries/version-linux-<arch>.txt` | Pinned version and sha256 of every embedded asset. |
| `releases/<version>/manifest.json` | Every file under the version, with size and sha256. |
| `guest-images/firecracker-x86_64/<sha256>/` | `vmlinux`, `rootfs.ext4`, `image.json`; `<sha256>` is the pinned sha256 of `image.json`. |

`<version>` is a git tag such as `v0.0.0-rc3`. `main/<commit>/` holds commit builds and
`main/latest/` is the newest. A guest image is uploaded once, under the key of its pins, and
every release with the same pins uses it.

## Install

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

`install bidder` creates the `swc-bidder` account, its subuid/subgid range and kvm membership, the
state dir, the binary slot, config, guest image, systemd user units, linger and the kcp bootstrap. It
ends by running `check-host -role bidder` as the account. State:
`/var/lib/socialweb-computer/swc-bidder`; kcp at `handles/<hash>/kcp` (root only); unit
`socialweb-computer-bidder@<handle>.service` in the account's user manager.

A re-run is idempotent: completed steps are skipped, and only a changed config or unit restarts the
core. A re-run with another `--handle` adds an instance. `--dry-run` prints the step list. A dev
binary needs `--allow-dev-binary`. `--ready-timeout` (default `1m`) bounds the first `/readyz` wait.
`--vm-slots` (default 256) reserves `N * 65536` subordinate ids.

## Guest image

`--guest-image-source` takes a `file://` or `https://` base, fetches `image.json`, `vmlinux` and
`rootfs.ext4`, and verifies each against the sha256 pinned in the binary. A mismatch is refused.

```bash
# variables carry over: BUCKET, V
HANDLE=<handle>
PIN=$(curl -fsSL "https://raw.githubusercontent.com/publicdomainrelay/socialweb-computer-kcp/$V/internal/versions/versions.json" \
  | jq -r '.guestImages.targets["firecracker-x86_64"].outputs["image.json"].sha256')
sudo /usr/local/bin/socialweb-computer install bidder --handle "$HANDLE" \
  --guest-image-source "$BUCKET/guest-images/firecracker-x86_64/$PIN"
```

Without a source, the binary resolves the image pinned into its own build from
`guest-images/<target>/<its pinned image.json sha256>`, then `releases/<its version>`, then
`main/<its commit>`, then `main/latest`, then the newest published release. Every candidate
is verified; only when all differ is the image refused. A stored image is re-verified.

Only `firecracker-x86_64` is published. An arm64 host builds one with `scripts/build-guest-image.sh`.

## Identity and association

The default identity is `localRepo`: the bidder's own PDS, never the operator's repo. `install
bidder` creates the PDS account and writes the DID into `MarketIdentity/bidder`, the session into the
bidder's OpenBao and the repo key there too. `-- --atproto-oauth-qr` signs in an existing account
instead; that account's PDS holds the repo and the bidder runs no local PDS.

Association is one `com.publicdomainrelay.temp.badgeBlueKeys` record in the operator's repo with
`challenge` the operator DID, `keyId` the bidder DID and service `bidder_associate`.
`scripts/associate-bidder.ts` writes the record from a mode-0600 credentials file and prints neither
handle nor password. The application's `AtprotoRecordWatch/association` verifies it and the identity
controller writes the operator DID into `MarketIdentity.status.associations`; no manual patch is
needed. `spec.approvedAssociations` is the declarative workaround, not the accepted path.

```bash
HANDLE=bobvmbuilder.bsky.social
ACCOUNT=BOB
CREDS=$HOME/.bobvmbuilder_bsky_social_source
STATE=/var/lib/socialweb-computer/swc-bidder
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
BIDDER_DID=$(sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.did}')
deno run -A --no-lock scripts/associate-bidder.ts --creds "$CREDS" --account "$ACCOUNT" --bidder-did "$BIDDER_DID"
```

A requester association uses the same helper with `--requester-did`; the helper derives service
`requester_associate`.

Non-association mode: patch the root object and let the controller reconcile it. It sets
`Associated=True` with reason `AssociationSkipped` and writes no record.

```bash
STATE=/var/lib/socialweb-computer/swc-bidder
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
sudo kubectl --kubeconfig "$K" patch marketbidder app --type=merge \
  -p '{"spec":{"identity":{"association":"Skip"}}}'
```

The bidder's own `AtprotoRecord/association` is applied through the app's PDS by the `AtprotoRecord`
controller. The helper writes the operator's claim once; the verified claim is the proof, and the
identity reports `Associated=True`.

### Operator sign-in: the operator's vouches from the bidder kcp

The option `operator-signin` (plan 0039 D5) signs the operator's own account in next to the bidder, so
[../examples/infra/vouch-bidder-operator.yaml](../examples/infra/vouch-bidder-operator.yaml) applies unchanged
in the bidder kcp, as in a market network. Set it once:

| where | how |
| --- | --- |
| `install bidder` | `--bidder-operator-signin <operator handle or DID>`, written to config.json `bidder-operator-signin` |
| `run bidder` | `--bidder-operator-signin <handle>`, or `-- --operator-signin <handle>` (wins) |
| config.json | `"bidder-operator-signin": "<handle>"`, or `"bidder-options": "--operator-signin <handle>"` |

It composes, in the bidder's workspace, the names a market network uses:

- `MarketIdentity operator`: mode `oauthQr`, `spec.sessionScope` `atproto
  repo:sh.tangled.graph.vouch?action=create&action=update` (the scope the identity checks and asks for; nothing
  else), the session at `<mount>/identity/operator/oauth-session` of the bidder's OpenBao, association `Skip`;
  the transferred QR session must be the named account
- once it is Ready: `RemotePDS operator-<hash>` (its PDS), `RemoteRepo operator`, `AtprotoRepo operator` and the
  ComponentLink `link-records-operator`, which lets AtprotoRecords labelled
  `apps.socialweb.computer/record-repo: operator` write `sh.tangled.graph.vouch` through that repo

Without the option nothing of it is composed, and the bidder's own `MarketIdentity bidder` and its scope never
change. The operator account should be the bidder's associated operator (`MarketIdentity bidder`
`status.approvedAssociations`): `bidder-tangled-vouch` reads that operator's vouches.

```bash
STATE=/var/lib/socialweb-computer/swc-bidder
K=$(sudo sh -c "ls $STATE/handles/*/kcp/admin.kubeconfig" | head -1)
kb() { sudo kubectl --kubeconfig "$K" "$@"; }
URL=$(kb get marketidentity operator -o jsonpath='{.status.pendingAction.url}')
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source --account BOB --url "$URL"
kb get marketidentity operator -o jsonpath='{.status.phase} {.status.did}{"\n"}'
REQ=did:plc:...
sed -e "s/REPLACE-REQUESTER-DID/$REQ/" -e "s/2026-11-01T00:00:00Z/$(date -u +%Y-%m-%dT%H:%M:%SZ)/" \
  examples/infra/vouch-bidder-operator.yaml | kb apply -f -
kb get atprotorecords.socialweb.computer operator-vouch-requester -o jsonpath='{.status.phase} {.status.uri}{"\n"}'
```

- Applied with `at://<operator did>/sh.tangled.graph.vouch/<requester did>`; the record is `deletionPolicy:
  Retain`, so a `kubectl delete` keeps it in the repo
- the QR page decides the scope it asks the PDS for; a session granted without the vouch scope fails the
  identity's scope check ("lacks scopes the identity now needs"); sign in again once the QR page asks for it
- the QR link expires after 5 minutes; read it right before the sign-in, never print it
- `-- --operator-signin-mode pdsAccount --operator-pds-url <url>` signs the operator in with a password instead
  (the market network's mode, an app password on a hosted PDS): put it in the bidder's OpenBao at
  `<mount>/identity/operator/keys`, key `password`, before the bidder starts
- removing the option stops composing them; the objects it made stay until you delete them
  (`marketidentity`, `atprotorepo`, `remoterepo` named `operator`); the records stay in the repo

## Upgrade and uninstall

```bash
cd "$(git rev-parse --show-toplevel)"
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

The upgrade switches `bin/current`, keeps `bin/previous`, restarts the core and adopts every installed
and transient unit. VMs survive. State, OpenBao, DID and association survive. `rollback` swaps the two
binary slots and restarts.

```bash
sudo /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer uninstall bidder --purge
```

`uninstall bidder --purge` stops the units, removes the state dir (PDS repo, OpenBao, keys, VMs) and
deletes the account install created. `--force` proceeds with active VM units; each stopped VM becomes
`Lost`.

## Terminate a VM from the bidder

A bidder operator ends a VM by deleting its `ComputeVM` in the bidder's kcp. Use it when the requester
cannot release the contract (the tab or app that holds it is gone) or when the owner asks for a VM to be
freed. It is the same path a requester's `vm.delete` takes, so nothing is left behind.

```bash
K=$(sudo -n -u swc-bidder sh -c 'ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig | head -1')
kb() { sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG="$K" kubectl "$@"; }
kb get computevms.socialweb.computer -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,CREATED:.metadata.creationTimestamp
kb delete computevms.socialweb.computer <name> --wait=false
kb get computevms.socialweb.computer
```

The name is `fc-<first 12 chars of the requester DID after did:plc:>-<hash>`; pick the VM by that DID
prefix and its creation time, and never delete one you have not matched to the contract you mean to end.

What the delete does, in order (its finalizer, logged by the bidder):

1. a termination StateSync, and a `vm.terminated` record pinned in the bidder's repo with reason
   `deleted_on_request`;
2. the VM process, its unit and its work dir are removed and the contract is released;
3. the finalizer is dropped and the ComputeVM is gone.

The `vm.terminated` event is also delivered to the requester; a 404 there only means the requester is
gone, and the record in the bidder repo still stands. Every atirc token leased to the contract dies by
its lease as soon as that termination record exists; a requester that is still running also revokes it.
Check the host afterwards: `kb get computevms.socialweb.computer` is empty for that contract and no
`firecracker` process remains for it.

## Unit and logs

`swc-bidder` is a system account, so its user units log to the system journal.

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user status 'socialweb-computer-bidder@<handle>.service'
sudo journalctl _SYSTEMD_USER_UNIT='socialweb-computer-bidder@<handle>.service' --since -10min
```

`journalctl --user` as that account finds nothing; read the system journal as root with the
`_SYSTEMD_USER_UNIT` filter.

## See also

- [CONTRIBUTING.md](../CONTRIBUTING.md) for copy-paste commands and troubleshooting
- [BIDDER_ARCH.md](../BIDDER_ARCH.md) for the live shape and known gaps
