# Bidder architecture

Host readings on 2026-10-22 from the installed bidder on this host, hat-9 and hat-7 (release binary
sha256 `30ab2e465a080bbe47ea16e085cdd99d10c640f4c90d438484363a443b31bd92`); the source claims were
rechecked against commit `1cad64b0` on 2026-10-26.

## Shape

A bidder is one installed application. It has one `socialweb-computer` core process, one kcp
logical cluster, one OpenBao server and its own PDS. It does not join a market network and it does
not run a separate application per component.

```
systemd --user (account swc-bidder, uid 947 / 988 / 964)
  socialweb.slice
  ├─ socialweb-computer-bidder@<handle>.service       core: appctl, componentcore, HostProcess supervisor
  ├─ socialweb-kcp-<instance>-kine@a.service           kine on sqlite
  ├─ socialweb-kcp-<instance>-front.service             L4 front for kcp
  ├─ socialweb-kcp-<instance>@a.service                 kcp API server slot a
  └─ socialweb-hp-<instance>-bao-bidder.service         OpenBao HostProcess

core (one process)
  ├─ devcore: lock, kcp slot lifecycle, bootstrap, members
  ├─ appctl: MarketBidder root, child objects, status, reconcile loop
  ├─ componentcore: Bidder, GuestControlPlane, Pds, PolicyEngine, Service/Exposure/ComponentLink
  ├─ hostprocess supervisor: bao-bidder and ComputeVM units
  └─ PolicyEngine runner + StateSync workers
```

The installed state root is `/var/lib/socialweb-computer/swc-bidder` (or the path passed to
`install bidder`). Its kcp handle is under `handles/<hash>/kcp`; the current bidder uses
`handles/53eff7074d/`.

## kcp layout

Observed root workspaces:

| workspace | type | role | phase |
| --- | --- | --- | --- |
| `socialweb-provider` | `universal` | API schemas, exports and provider service accounts | Ready |
| `app` | `socialweb-application` | the bidder's application objects and components | Ready |

The `root` workspace also holds the kcp bootstrap objects and the `MarketBidder` root. Namespaces in
the root and `app` workspaces are `default` and `kcp-system` (the latter from kcp itself).

The `socialweb-application` type binds `applications`, `policy`, `computevms`, `hostprocesses`,
`clusters`, `atprotoidentities`, `marketidentities`, `computecontracts`, `exposures`, `pds` and
`events`. The bidder set additionally installs the `computevms` schema/export and the root binding,
because the bidder owns ComputeVM objects.

Observed root objects on this host:

| kind/name | phase | note |
| --- | --- | --- |
| `MarketBidder/app` | Ready | profile `prod-qr`, DID `did:plc:vxd7pmlhw6bnpyu6wngewthn` |
| `MarketIdentity/bidder` | Ready | localRepo, association `RequireApproval`, approved operator DID |
| `OpenBaoServer/bidder` | Ready | `https://127.0.0.1:45457`, one node, initialized and unsealed |
| `Pds/pds` | Ready | app-owned PDS, endpoint `http://pds.pds` |
| `Bidder/bidder` | Ready | market loop and guest launch |
| `GuestControlPlane/guest-kcp` | Ready | relay, OIDC, sshsig, on-network and host keys |
| `PolicyEngine/policy-engine` | Running | in-process policy engine |
| `AtprotoRecord/association` | Pending | association record declaration |
| `AtprotoRecordWatch/association` | — | watches the operator repo |
| `EventStream/watch-association` | Running | feeds the watch |
| `HostProcess/bao-bidder` | Running | OpenBao |
| `HostProcess/bootstrap-kcp-a`, `bootstrap-kine-a`, `bootstrap-front` | Running | kcp bootstrap units |

Every component has a `Service` and an `Exposure`:

| Service | target | Exposure |
| --- | --- | --- |
| `svc-bidder-bidder` | `Bidder/bidder` | ingress `bobvmbuilder.bsky.social` through `IngressProxy/ingress` |
| `svc-guestcontrolplane-guest-kcp` | `GuestControlPlane/guest-kcp` | ingress through `IngressProxy/ingress` |
| `svc-pds-pds` | `Pds/pds` | local `http://127.0.0.1:20000`; ingress through `IngressProxy/ingress` |

Application components do not bind their own public sockets. The component runtime registers
handlers in-process; the local Exposure controller leases a loopback port from the port registry
(`pds` is 20000 here). The `prod-qr` and `market-network` profiles default `spec.ingress` to
`xrpc-subscription` with `ingressProxyHost: xrpc.fedproxy.com` and `IngressProxy/ingress`; the PDS
ingress Exposure is fail-closed until that provider is Ready. A bidder joined to a market network
inherits `MarketNetworkValue.ingress` and its `dispatcher` instead.

## Identity and association

`install bidder` creates the PDS account and writes the DID into `MarketIdentity/bidder`, the
session into the bidder's OpenBao at `bidder/data/identity/bidder/pds-session`, and the PDS repo key
at `bidder/data/identity/bidder/keys`. The bidder's own repo is the PDS, not the operator's repo.

Association is one `com.publicdomainrelay.temp.badgeBlueKeys` record in the operator's repo; the
verified operator DID lands in `MarketIdentity.status.associations`. The record shape is:

```json
{
  "$type": "com.publicdomainrelay.temp.badgeBlueKeys",
  "keyId": "did:plc:<bidder>",
  "challenge": "did:plc:<operator>",
  "service": "bidder_associate",
  "name": "bidder",
  "createdAt": "2026-10-22T00:41:02Z"
}
```

`scripts/associate-bidder.ts` writes that record from a mode-0600 credentials file without printing
the handle or password. The operator's `AtprotoRecordWatch/association` verifies the claim and the
identity controller writes the operator DID into `status.associations`, setting `Associated=True`
with reason `Associated`. With `spec.association: Skip` it sets `Associated=True` and reason
`AssociationSkipped`. A manual patch of `spec.approvedAssociations` is a declarative workaround, not
the accepted path:

```bash
sudo kubectl --kubeconfig "$K" patch marketidentity bidder --type=merge \
  -p "{\"spec\":{\"approvedAssociations\":[\"$OPERATOR_DID\"]}}"
```

The bidder's own `AtprotoRecord/association` is applied into the app's PDS by the `AtprotoRecord`
controller; the record is listed by `com.atproto.repo.listRecords`.

## Live bidder instances

| host | state root | uid | bidder DID | association | mode |
| --- | --- | --- | --- | --- | --- |
| this host | `/var/lib/socialweb-computer/swc-bidder` | 947 | `did:plc:vxd7pmlhw6bnpyu6wngewthn` | `Associated`, operator `did:plc:ocnuqjlzgohypj3zoznermms` | `RequireApproval` |
| hat-9 | `/var/lib/socialweb-computer/swc-bidder` | 988 | `did:plc:cjuftobfmmmdkmblcl4h54eb` | `Associated`, same operator | `RequireApproval` |
| hat-7 | `/var/lib/socialweb-computer/swc-bidder` | 964 | `did:plc:exjutifioeu2p7oorbdrex2o` | `AssociationSkipped` | `Skip` |

All three run the release binary at
`bin/30ab2e465a080bbe47ea16e085cdd99d10c640f4c90d438484363a443b31bd92` and report
`MarketBidder/app` Ready. Each has its own PDS, OpenBao, DID and host guest image
`bddc484c610e956fb7e198281cf4a19dc8c95fc441c0575f49d2a33de0c74021`.

Association-record state:

- this host: `at://did:plc:vxd7pmlhw6bnpyu6wngewthn/com.publicdomainrelay.temp.badgeBlueKeys/association`,
  cid `bafyreiapgmzf3cgdmpxefzonba52xk5lo6kojkd2eijyiq2yg6jtmirjpm`
- hat-9: `at://did:plc:cjuftobfmmmdkmblcl4h54eb/com.publicdomainrelay.temp.badgeBlueKeys/association`,
  cid `bafyreiebup7ijio2u73l2sgz4bfpginggbqdjxiqxfspjg5tzcw2da5czm`

## Install, upgrade, delete

`install bidder` is an account install. It creates the `swc-bidder` account, subordinate ID
ranges, state dir, binary slot, config, guest image, systemd user units, linger and kcp bootstrap.
`upgrade --binary --sha256` switches `bin/current`, restarts the core and keeps `bin/previous`.
`install bidder` again reconciles the unit files and host contract. `uninstall bidder --purge`
stops the app units, removes the state dir (including the PDS repo and OpenBao) and deletes the
account; `--force` is only for live VM units.

The `prod-qr` profile uses a local repo and an app-owned PDS. A fresh install therefore creates a
new DID and needs the one association claim; an upgrade keeps the state, DID, repo and association.

## Known gaps

- Ingress Exposures are Pending on a standalone bidder with no `IngressProxy`.
- `AtprotoRecordWatch/association` can report `invalid repo op` while the PDS stream is consumed;
  the association state above does not depend on that watch.

## Inspect

```bash
K=$(sudo sh -c 'ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig' | head -1)
sudo kubectl --kubeconfig "$K" get workspaces,marketbidder,marketidentity,openbaoserver
sudo kubectl --kubeconfig "$K" get bidder,guestcontrolplane,pds,policyengine,service,exposure -o wide
sudo kubectl --kubeconfig "$K" get hostprocess,atprotorecord,atprotorecordwatch,eventstream -o wide
sudo kubectl --kubeconfig "$K" get marketidentity bidder -o jsonpath='{.spec}{"\n"}{.status}{"\n"}'
```

The kcp kubeconfig is readable only by root on an installed host; the core's health and unit state
are under `systemctl --user` for the `swc-bidder` account.
