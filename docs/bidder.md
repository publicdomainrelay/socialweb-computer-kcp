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
