# VMM integration tests

Each test boots a VM through `socialweb-computer internal vm start|inspect|stop|list` and checks it from outside:
user-data, size, disk, network out, ssh over a reverse tunnel, ssh over iroh (dumbpipe), the webrtc UDP forward,
a guest filling its memory, graceful stop, and that no unit, process, work dir, socket dir or ACL grant is left.
QEMU (the default) also boots a nested QEMU guest kernel under KVM inside its guest.

Needs a Linux x86_64 host with KVM, nested on (`scripts/host-nested-kvm.sh`), a subordinate id range, `sshd`,
`ssh-keygen`, `getfacl`, a systemd user manager and cargo. From a checkout:

```sh
B=~/.tmp/swc-vmm
scripts/stage-assets.sh --platform linux/amd64 --asset pasta --asset swc-userns-stage --asset socialweb-vmexec --asset firecracker --asset qemu --blobs $B/blobs --print-pins
for a in pasta swc-userns-stage socialweb-vmexec firecracker qemu; do mkdir -p $B/$a && tar --zstd -xf $B/blobs/linux_amd64/$a.tar.zst -C $B/$a; done
sha=$(jq -r '.guestImages.targets["firecracker-x86_64"].outputs["image.json"].sha256' internal/versions/versions.json)
mkdir -p $B/image && for f in vmlinux rootfs.ext4 image.json; do curl -fsSLo $B/image/$f https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com/guest-images/firecracker-x86_64/$sha/$f; done
go build -o $B/swc ./cmd/socialweb-computer
```

```sh
B=~/.tmp/swc-vmm
export SWC=$B/swc SOCIALWEB_HOST_BACKEND=systemd-user SOCIALWEB_STATE_DIR=$B/state SOCIALWEB_HANDLE=vmmtest.invalid \
  SOCIALWEB_USERNS_STAGE_PATH=$B/swc-userns-stage/swc-userns-stage SOCIALWEB_PASTA_PATH=$B/pasta/pasta \
  SOCIALWEB_VMEXEC_PATH=$B/socialweb-vmexec/socialweb-vmexec SOCIALWEB_FIRECRACKER_PATH=$B/firecracker/firecracker \
  SOCIALWEB_QEMU_PATH=$B/qemu/usr/bin/qemu-system-x86_64 SOCIALWEB_GUEST_IMAGE_SOURCE=file://$B/image
cargo test --manifest-path tests/Cargo.toml -p swc-vmm-tests -- --nocapture
```
