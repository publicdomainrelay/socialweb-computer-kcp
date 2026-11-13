# Host a bidder

A bidder sells VMs (QEMU; [VMM](#vmm)) (and, with `--compute-kinds vm,deno`, Deno sandboxes) on the market.

## Prerequisites

- Linux x86_64 with `/dev/kvm` and `/dev/net/tun`, systemd 253+, cgroup v2 (arm64: build a guest image with
  `scripts/build-guest-image.sh`; only `firecracker-x86_64` is published)
- the binary: [README: Download](../README.md#download)
- packages, Debian / Ubuntu (24.04+ also needs the sysctl for unprivileged user namespaces):

```sh
sudo apt-get install -y uidmap openssh-client curl tzdata jq
echo kernel.apparmor_restrict_unprivileged_userns=0 | sudo tee /etc/sysctl.d/60-socialweb.conf
sudo sysctl --system
```

- packages, Fedora / RHEL:

```sh
sudo dnf install -y shadow-utils openssh-clients curl tzdata jq
```

- ports: no inbound TCP. ssh to a guest goes over iroh (outbound, n0 relays) or webrtc; webrtc leases one UDP port
  per VM from 31000-31999 on the host's private (RFC1918 / ULA) addresses: allow that range from your LAN
- nested virtualization, so guests get `/dev/kvm` (staging bidders run inside rented VMs), from a checkout of this repo:

```sh
scripts/host-nested-kvm.sh
sudo scripts/host-nested-kvm.sh --apply
```

  `--apply` reloads the KVM module only when nested is off and no VM runs; otherwise it persists the setting for the
  next boot
- `check-host -role bidder` runs at the end of an install and before a foreground start
- VM size: the offering advertises `maxCpus` / `maxMem` (the host's CPUs and memory); an RFP asking for more is refused
- `nestedVirt`: the offering sets it when the VMM is QEMU and nested virtualization (above) is on; an RFP asking for it is refused otherwise

## Identity

| mode | how |
| --- | --- |
| own PDS, new DID (default) | nothing; you are its operator with `--bidder-operator-signin <your handle>` |
| your existing account | foreground: `-- --atproto-oauth-qr` after the other options; installed: [CONTRIBUTING.md](../CONTRIBUTING.md#bidder-on-a-real-account-oauthqr) |

`--bidder-operator-signin <your handle>` asks you to sign in once (the URL below). The bidder then adds
`https://ident.link/agent-of/<your DID>` to its DID document and writes the grant
`com.publicdomainrelay.temp.market.association` and `link.ident.endorsement` (rkey = the bidder DID) to your repo:
[design/ident-link-associations.md](design/ident-link-associations.md). Until then it waits in `AwaitingApproval`.

The bidder bids only for requesters you vouch for (`sh.tangled.graph.vouch`, rkey = their DID, in your repo):
[quickstart-prod.md](quickstart-prod.md#2-vouches-on-prod).

## Foreground

As a normal user (not root) in the `kvm` group with one subordinate id range, 16 VMs here (once; `check-host` names what is missing):

```sh
sudo usermod -aG kvm $USER
sudo usermod --add-subuids 100000-1148575 --add-subgids 100000-1148575 $USER
sudo loginctl enable-linger $USER
```

Log in again, then (Ctrl-C stops it and keeps its state; the same command resumes the same DID):

```sh
socialweb-computer run bidder --handle <your handle> --host-backend proc --fg --bidder-operator-signin <your handle>
```

The sign-in URL, in a second terminal:

```sh
K=~/.local/state/socialweb-computer/dev/b-$(echo '<your handle>' | tr . -)/kcp/admin.kubeconfig
kubectl --kubeconfig "$K" get marketidentity operator -o jsonpath='{.status.pendingAction.url}{"\n"}'
kubectl --kubeconfig "$K" get marketbidder app -o jsonpath='{.status.phase} {.status.did}{"\n"}'
```

## Installed

A systemd user unit `socialweb-computer-bidder@<handle>` of the account `swc-bidder`, state
`/var/lib/socialweb-computer/swc-bidder`. `--dry-run` prints every step and writes nothing; a re-run is idempotent.

```sh
sudo "$(command -v socialweb-computer)" install bidder --handle <your handle> --bidder-operator-signin <your handle>
```

Helpers for the rest of this page:

```sh
A=swc-bidder ST=/var/lib/socialweb-computer/swc-bidder
asb() { sudo runuser -u $A -- env XDG_RUNTIME_DIR=/run/user/$(id -u $A) CONFIG_PATH_SOCIALWEB_COMPUTER=$ST/config.json "$@"; }
K=$(sudo sh -c "ls $ST/handles/*/kcp/admin.kubeconfig" | head -1)
```

Sign-in URL, status, logs:

```sh
sudo kubectl --kubeconfig "$K" get marketidentity operator -o jsonpath='{.status.pendingAction.url}{"\n"}'
sudo kubectl --kubeconfig "$K" get marketbidder app -o jsonpath='{.status.phase} {.status.did}{"\n"}'
asb systemctl --user status 'socialweb-computer-bidder@*'
sudo journalctl _SYSTEMD_USER_UNIT=socialweb-computer-bidder@<your handle>.service --since -10min
```

Upgrade to a newer release (download it as in the README first; VMs, DID and association survive), or roll back:

```sh
NEW=/var/tmp/socialweb-computer-$(socialweb-computer version | awk 'NR==1{print $2}')
sudo install -m 0755 "$(command -v socialweb-computer)" "$NEW"
asb $ST/bin/current/socialweb-computer upgrade --binary "$NEW" --sha256 "$(sha256sum "$NEW" | cut -d' ' -f1)"
sudo "$NEW" install bidder --handle <your handle>
```

```sh
asb $ST/bin/current/socialweb-computer rollback
```

Uninstall (`--purge` also removes the state: DID, PDS repo, OpenBao, VMs, and the account):

```sh
sudo $ST/bin/current/socialweb-computer uninstall bidder --purge
```

## Association

Without `--bidder-operator-signin` this is not automatic. On the bidder host (helpers above), add
`https://ident.link/agent-of/<your DID>` to the bidder's DID document (a PLC op signed by its local PDS
rotation key; `--drop-borrowed-handle` drops an `at://<your handle>` entry):

```sh
OP_DID=<your DID>
BIDDER_DID=$(sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.status.did}')
PDS_DIR=$(sudo kubectl --kubeconfig "$K" get pds pds -o jsonpath='{.spec.stateDir}')
asb $ST/bin/current/socialweb-computer identity agent-of add "$OP_DID" --did "$BIDDER_DID" --pds-state-dir "$PDS_DIR" --drop-borrowed-handle
```

Then write the grant and endorsement from a mode-0600 credentials file
(`<ACCOUNT>_ATPROTO_HANDLE`, `<ACCOUNT>_ATPROTO_PASSWORD`), from a checkout of this repo:

```sh
deno run -A --no-lock scripts/associate-bidder.ts --creds <creds file> --account <ACCOUNT> --agent-did "$BIDDER_DID"
socialweb-computer internal association "$BIDDER_DID" | jq -c '{roots, errors}'
```

`roots` is `[<your DID>]` once it holds. `--remove` deletes the endorsement, then the grant.

## VMM

QEMU on x86_64 (guests get `/dev/kvm` when nested is on), Firecracker on arm64. Pick one for new VMs; a VM keeps
the VMM it booted under:

```sh
sudo "$(command -v socialweb-computer)" install bidder --handle <your handle> --vmm firecracker
```

Foreground: `run bidder ... --vmm firecracker`. VMM integration tests: [tests/vmm](../tests/vmm/README.md).

## End a VM

Delete its `ComputeVM` (`fc-<first 12 chars of the requester DID>-<hash>`); the bidder writes `vm.terminated`
and releases the contract. Every VM also ends by itself 24 h after its accept (or at the RFP's `expiresAt`) plus
10 min.

```sh
sudo kubectl --kubeconfig "$K" get computevms.socialweb.computer
sudo kubectl --kubeconfig "$K" delete computevms.socialweb.computer <name> --wait=false
```
