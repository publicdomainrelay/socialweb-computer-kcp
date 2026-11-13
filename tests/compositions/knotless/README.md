# knotless (staging)

A staging network's own knotless: `dev up --with-knotless` runs knotless-serve and knotless-run in the gateway, on the
network's PLC, PDSes and relay, the runner signed in as the gateway's owner account (`<instance>-gateway-owner.alice.test`,
vouched both ways with every network bidder). Nothing here reaches prod knotless or a prod account; `seed` reads the
published `socialweb-computer-cicd` composition (`knotless.source`) once, its modules' wasm checked against their pins.

| entry | proves |
| --- | --- |
| `push` | a workflow pushed to a fresh repo: the runner rents a network bidder's VM, the check lands success |
| `after` | `build` keeps an artifact; `test` (`after: [build]`) reads it in its VM with `$KNOTLESS_ARTIFACTS_URL` and `$KNOTLESS_ARTIFACTS_TOKEN`; `knotless run artifacts --out` downloads it |
| `executor` | two workflows on two bindings (`executor: small`, `executor: big`): each VM has its binding's vCPUs |
| `pull` | a sealed secret: the push to main sees it, a pull's run gets none |
| `fallback` | one workflow per network bidder (the account's vouched DIDs with an offering), each on a `fill` binding of the tightest `maxCpus / 2 + 1` vCPUs so a bidder holds one: every check succeeds, and in one of up to 3 rounds the log shows `==> the rent noted: <bidder> declined the accept: ... CapacityExceeded` (the atlibs requester module's fallback; [0014](../../../docs/acceptance/0014-accept-fallback.md)'s atlibs path) |
| `terminated` | a step ends its own VM 20 s in (sysrq `o` under QEMU's ACPI, `b` under Firecracker: either VMM exits): the bidder's `vm.terminated` reaches the cicd workload (`com.fedfork.market.watch` on the requester that rented it) and the step fails in under 5 min with `the VM was terminated by its bidder: <reason>` |

## Run

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/knotless && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=<staging target.json> cargo run -p swc-test-host -- compositions/knotless --account owner --entry push
```

The target names the account and the network's knotless (paths from `dev status`):

```json
{
  "accounts": {"owner": {"handle": "<instance>-gateway-owner.alice.test", "did": "<did>",
    "creds": "<gateway state>/knotless/owner.env", "throwaway": true}},
  "knotless": {"env": "<gateway state>/knotless/client.env",
    "source": "at://did:plc:ocnuqjlzgohypj3zoznermms/com.fedfork.exec.composition/socialweb-computer-cicd"}
}
```
