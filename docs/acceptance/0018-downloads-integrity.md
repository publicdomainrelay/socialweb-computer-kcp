# Acceptance 0018: the shipped binary never uses a tampered downloads tree

The two downloads trees (`socialweb-guest-downloads`, which a bidder's GuestControlPlane serves to its guests at
`/v1/downloads/<sha256>/<name>`, and `socialweb-computer-k8s-downloads`, which the web app serves at
`/downloads/<sha256>/<name>`) are built from this repository's own commit. `versions.json` does not pin them: the build
records each tree's digest beside its blob (`<asset>.built.json`, `internal/assets/built.go`), the record is embedded in the
binary with the blob, and the binary re-hashes the extracted tree against it before every use. Each file in a tree also has
a `<name>.sha256` sidecar, which the services check when they load the tree (`internal/kit/pinneddownloads`: the sidecar of
a node's or a guest's pin is the file's own hash). This runbook tampers what the binary extracts and proves the services
never get a tampered file.

- Code: `internal/assets` (`Store.Ensure`, `Ready`, `PlatformPins`), `internal/kit/pinneddownloads`,
  `cmd/socialweb-computer/verify_assets_release.go` (`dev verify-assets [--asset-cache DIR]`: extracts every embedded asset,
  then loads the two downloads trees the way the GuestControlPlane and the web app load them)
- Test: `tests/compositions/0018` (module `tests/modules/acc-0018`), entry `run`, in `tests/gate.sh` as `0018:run`; staging
  only (the module skips on prod); host-only, no VM (weight 0). The host handler is `assets.tamper` (`tests/host/src/more.rs`)
  and runs the target's own `SWC_BINARY`
- Last pass: see Evidence

## What it asserts

Each variant first runs `dev verify-assets --asset-cache <dir>` on an empty cache (it must succeed), then:

- `file`: one byte of `socialweb-guest-agent-x86_64` in the extracted tree is flipped, its sidecar untouched (the
  GuestControlPlane's loader would refuse that tree). `dev verify-assets` on the same cache succeeds and the file and its
  sidecar are the recorded bytes again: the tree no longer matched its record, so it was replaced from the embedded blob
- `fileSidecar`: the same, and the sidecar is rewritten to the new hash. The loaders alone accept that tree; the build record
  does not, so it is healed the same way. This is why the record, not the sidecar, is the root of trust
- `record`: a copy of the binary with one digit of the embedded record's tree digest flipped. Its embedded blob then unpacks to
  another tree than its record names, so `dev verify-assets` exits non-zero ("unpacks to tree X, but its pin ... is Y") and
  extracts no socialweb-guest-downloads tree: a binary whose record and blob disagree refuses to use the asset

## Steps

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/0018 && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=<staging target.json> cargo run -p swc-test-host -- compositions/0018 --account bob --entry run
```

The same assertions on any one binary without the test host: `dev verify-assets --asset-cache $(mktemp -d)` twice around a
`chmod -R u+w` and a byte flip in the extracted `socialweb-guest-downloads` tree.
