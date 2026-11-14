# Acceptance 0016: a VM rented with a requester-chosen image

Proves a requester can rent a VM that boots its own image (kernel, rootfs, `image.json`, pinned by the sha256 of
`image.json`), the bidder verifies every byte before boot, the guest reports the image's marker, and a wrong sha256 is
refused before any VM exists. The CI toolchain image (`deploy/vm-images/ci-toolchain.json`, built by
`scripts/build-ci-image.sh`) is the image the gate rents.

- Composition: `tests/compositions/0016` (module `acc-0016`), in `tests/gate.sh`: `0016:run`, `0016:badsha`, `0016:bigmarker`; the gate
  passes `--input '{image: {url, sha256}, marker}'` from `deploy/vm-images/ci-toolchain.json`
- `run`: `run request --hold --image <url>@<sha256>`; Held only after `requester_guest_image` says `ok` (the guest's
  `guest.image` marker equals the `image.json`'s); ssh: `/etc/swc-image` is the marker, `/opt/ci/env` answers go, cargo,
  deno, dockerd is installed; release; every contract record RecordNotFound
- `run` then rents the same image a second time (name `<name>-b`) and, when the target names the winner's kcp, reads the
  FirecrackerVM's `status.image.fetched`: true on the first boot of an image on a bidder, it must be false on the second
  (the bidder keeps requested images by last use within `requested-image-budget`, 16G by default, and evicts the least
  recently used only over the budget or with under 8G free; an image a VM uses, or used in the last 10 minutes, stays)
- `badsha`: the same with the first digit of the sha256 changed: every bidder refuses to bid (the fetched `image.json`
  hashes to another value): each answers submitRfp `declined` kind `unservable` with that reason, so the requester does not wait for capacity: the contract fails at once, `no bidder can serve this request: <bidder>: <reason>`, before a receipt; nothing Held, no VM on any host (about 40 s)
- `bigmarker`: the CI image of 2026-11-14 (`vm-images/ci-toolchain/bb59b647...`, its `image.json` marker 330 bytes; the module fixes its url and
  sha256, published content-addressed): every bidder answers submitRfp `declined` kind `unservable` with `its marker is over 256 bytes`
  (also when it is full: an image refusal wins over a capacity one), so the request fails at once naming it, under 150 s (about 40 s), never
  waiting for capacity. Every refusal of the image check reaches the requester this way: the url or sha256 malformed, a redirect or a
  non-public address, `image.json` not fetchable (status, read, over 64 KiB), its sha256 not the requested one, not JSON, a file record
  malformed, a target, hypervisor or arch other than the bidder's, no vcpu or memMiB, bootArgs empty or over 4096 bytes, a marker over 256 bytes,
  kernel and rootfs over the request's disk
- Refusals after a bid are re-placed: a bidder that declines the accept (its image check failed at provision), a guest that
  reports another marker or none, a `vm.terminated` `image_refused` (a file's sha or size did not match while fetching):
  the VM is released and the RFP goes to another bidder that offers `images`, up to `MaxAcceptReplacements`
- Limits: https and public addresses only, 64 KiB for `image.json`, kernel + rootfs within the RFP's `disk`; the full SSRF
  fix is deferred (`docs/FEATURES.md` Known limits)
- Needs: a bidder whose offering sets `images` (a VM bidder with a guest image store); the requester discovers it
