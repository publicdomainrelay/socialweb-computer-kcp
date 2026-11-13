# Staging

A full socialweb.computer stood up on rented VMs for one run of the suite, then torn down. Owner-approved (2026-11-12,
A+B). Status: slice 1 built, dry run only: requester, cluster `stg`, overlay, the 4 VMs (meshd only), names
resolving (`tests/modules/staging`; host: `computer.socialweb.test.{requester.cluster,requester.infra,staging.*}`).
Next: `services.yaml`, the VMs' `install gateway` / `install bidder`, health, `target-staging.json`.

## Pieces

| piece | where | how |
| --- | --- | --- |
| orchestrator | the trigger's requester: hat-8 by hand, the rented orchestrator VM from knotless | `run requester --with-gateway` (`computer.socialweb.test.requester.start`) |
| market k3s (A) | the orchestrator's own tenant cluster `stg` | `run requester cluster` (`computer.socialweb.test.requester.cluster`); pods that do not fit rent market nodes (NodeClaim) |
| PLC, PDS (throwaway accounts), relay, Jetstream | pods on `stg` (`services.yaml`, images by digest) | answer on the services VIP `10.45.0.1` |
| kcp gateway, ssh gateway, did:key ingress proxy | a plain VM (`gateway` contract) | `socialweb-computer install gateway` against the staging PLC/PDS/relay |
| 3 bidders (B) | plain VMs, nested KVM, QEMU | `socialweb-computer install bidder`, a host process, as prod |
| overlay | every VM and `stg` node | socialweb-meshd, one roster, one zone |

The VMs are one `EphemeralInfra` in the orchestrator's kcp (`--cpus`/`--mem` per contract, `userData` installs meshd
and the piece). Its `ttlSeconds` bounds a run whose teardown never ran.

## Overlay

- Zone: `<run>.staging.internal`. Names: `bidder-1..3`, `gateway`, and on the services VIP `plc`, `pds`, `relay`,
  `jetstream`, `ssh`, `ingress`.
- Roster: one `computer.socialweb.mesh.roster` record (lexicon: `lexicons/computer/socialweb/mesh/roster.json`) in the
  orchestrator's account at rkey `<run>`; meshd's `--roster` is its getRecord URL. VMs: `--roster`; `stg` nodes: the
  `socialweb.computer/mesh-roster` annotation.
- VM addresses `10.44.0.0/24` (`/32` each); `stg` nodes their podCIDR. The node running the services pods also carries
  `10.45.0.1/32` as its own peer entry `services` (same endpoint, no overlap), which only roster members (the VMs) route;
  pods reach the services through cluster DNS.
- meshd secrets are made by the host (`computer.socialweb.test.staging.meshKeys`, 0600 in the run's dir), so the roster
  is written before the VMs boot; the module sees only endpoints. A VM's secret rides in its `userData`
  (`@meshKey:<peer>`, filled by the host): its bidder sees it; the overlay lives one run.
- The orchestrator is a member (`staging.meshJoin`: `sudo -n socialweb-meshd`, `$SWC_MESHD`) at `10.44.0.1`, its DNS
  the one the checks ask.

## Run

`<run>`: the host's run name, `st-staging-<unix s>`.

1. requester (`--with-gateway`), tenant cluster `stg`.
2. mesh keys; roster record; `services.yaml` on `stg`; the `EphemeralInfra` in the orchestrator.
3. Held: every contract; Ready: every pod. Roster gains the `stg` nodes (`mesh-endpoint`, podCIDR).
4. Healthy: each service's health URL by its overlay name, from a bidder VM and from a `stg` pod; every name in the zone
   resolves on every member.
5. `target-staging.json` (the `targets/prod.json` shape, staging URLs, throwaway accounts) for the suite.
6. Always, however it ended: the `EphemeralInfra` released, the cluster deleted, the requester stopped, the roster
   record deleted (the run's ledger), throwaway DIDs tombstoned with the staging PLC (then gone with it).

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/staging && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/staging --dry-run
```

## Open

- kcp components (gateway, ssh gateway, ingress proxy) run as host processes only, so they sit on a VM, not in `stg`.
- The NAT test (requester and bidder on Firehose comms, inbound closed) runs against this target: a suite test, not
  the stand-up.
