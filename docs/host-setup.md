# Host setup and the guest image

The long form of the README host prerequisites: the bare-host packages and their settings, plus how a bidder resolves and verifies its guest image. Short path in [the README](../README.md#prerequisites).

A bare Ubuntu 24.04 or Fedora host needs these OS packages and settings for the market network and `run request` below. `dev up` for a network with a bidder checks the host first, and `run request` checks for `ssh`; each stops at once, naming what is missing and its fix; `socialweb-computer check-host -role bidder -state-dir <dir>` lists every check.

| Need | Ubuntu / Debian package | Fedora / RHEL package |
|---|---|---|
| `newuidmap` and `newgidmap`, so each VM runs in its own user namespace | `uidmap` | `shadow-utils` |
| `ssh` and `ssh-keygen`, which `run request` runs | `openssh-client` | `openssh-clients` |
| `curl`, to install the binary and fetch the guest image | `curl` | `curl` |
| `/usr/share/zoneinfo`, for the embedded postgres of the gateway | `tzdata` | `tzdata` |
| `jq`, used by the examples below (optional) | `jq` | `jq` |
| `machinectl`, to log in as the VM account (optional; or use `ssh`) | `systemd-container` | `systemd-container` |

As root, once:

```bash
apt-get install -y uidmap openssh-client curl tzdata jq systemd-container
useradd -m -s /bin/bash -G kvm -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 test-0001
usermod --add-subuids 100000-1148575 --add-subgids 100000-1148575 test-0001
install -d -m 0700 -o test-0001 -g test-0001 ~test-0001/.ssh
install -m 0600 -o test-0001 -g test-0001 ~/.ssh/authorized_keys ~test-0001/.ssh/
loginctl enable-linger test-0001
sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
echo kernel.apparmor_restrict_unprivileged_userns=0 > /etc/sysctl.d/60-socialweb.conf
```

- `-K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0` stops `useradd` from adding its own 65536-id range; the account must have exactly one range in each file. `100000-1148575` is 16 VM slots.
- Linger keeps the account's `systemd --user` manager running. Log in as the account with `ssh test-0001@<host>` (the two `install` lines give it root's `authorized_keys`) or, as root on the host, `machinectl shell test-0001@`; `sudo -u` and `runuser` do not create the `/run/user/<uid>` session that `XDG_RUNTIME_DIR` names.
- On a cloud host's first boot, an unattended upgrade can hold the apt lock for a few minutes; `apt-get` then stops with `Could not get lock`. Wait and run it again.
- The `sysctl` lets the embedded pasta create a user namespace on Ubuntu 24.04 and newer, where AppArmor restricts it. On a host that has no such sysctl, skip those two lines. A production bidder loads an AppArmor profile through `install bidder` instead.
- `/dev/kvm` is read-write for the `kvm` group; `/dev/net/tun` is open on current kernels, otherwise run `modprobe tun`.
- The account's cgroup delegation (`cpu memory pids` in `user@<uid>.service`) is the default on Ubuntu 24.04; `dev up` names it if the host lacks it.

---

Every tagged release publishes Linux binaries and the guest image to a public bucket. The bucket needs no credentials.

| Path | Holds |
| --- | --- |
| `releases/<version>/binaries/socialweb-computer-linux-amd64` | The host binary for Linux amd64. |
| `releases/<version>/binaries/socialweb-computer-linux-arm64` | The host binary for Linux arm64. |
| `releases/<version>/binaries/SHA256SUMS` | The sha256 of both binaries. |
| `releases/<version>/binaries/release.json` | The commit, the build mode and one entry per platform. |
| `releases/<version>/binaries/version-linux-<arch>.txt` | The pinned version and sha256 of every asset in that binary. |
| `releases/<version>/manifest.json` | Every file under the version, with its size and sha256. |
| `guest-images/firecracker-x86_64/<sha256>/` | `vmlinux`, `rootfs.ext4` and `image.json`: the guest image a bidder's VMs boot. `<sha256>` is the sha256 of `image.json` pinned in `internal/versions/versions.json`, which pins the other two files. |

`<version>` is a git tag, for example `v0.0.0-rc3`. Everything in the bucket is readable without credentials: `releases/<version>/` holds one tag's build, `main/<commit>/` holds the build of each commit on `main`, and `main/latest/` is a copy of the newest one. A guest image is uploaded once, under the key of its pins; releases before the content-addressed key also hold one under `releases/<version>/guest-image/`.

## See also

- [The bidder](bidder.md)
