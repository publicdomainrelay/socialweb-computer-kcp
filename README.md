# socialweb-computer-kcp

Rent and sell VMs on the atproto compute market. One binary: `socialweb-computer`.

## Download

Linux amd64 and arm64 (no macOS build is published).

```sh
V=v0.0.0-rc164
ARCH=linux-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
cd "$(mktemp -d)"
curl -fLO https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com/releases/$V/binaries/socialweb-computer-$ARCH
cat > SHA256SUMS <<'EOF'
0722fb6551163e8c5f800aef6a66fe9fcad33d6567594f564a9388ae5ec5030c  socialweb-computer-linux-amd64
83ab88477f3968eb067bf7eec0b39dd64093695ff440e4c3d54e1b35f2c905d5  socialweb-computer-linux-arm64
EOF
sha256sum -c --ignore-missing SHA256SUMS
install -D -m 0755 socialweb-computer-$ARCH ~/.local/bin/socialweb-computer
export PATH=$HOME/.local/bin:$PATH
socialweb-computer version
```

## Request compute

Browser: [shell.socialweb.computer](https://shell.socialweb.computer) (sign in, rent a VM, shell in the tab) or
[app.socialweb.computer](https://app.socialweb.computer).

CLI: sign in by the QR code (or the URL under it), get a root shell on the VM. `exit` releases it.

```sh
socialweb-computer run request --name vm1 -- --atproto-oauth-qr
```

Keep it for 2 hours instead. Leave it running; in a second terminal:

```sh
socialweb-computer run request --name vm2 --hold --expires-in 2h --exec 'uname -a' -- --atproto-oauth-qr
```

```sh
socialweb-computer run request ssh vm2
socialweb-computer run request release vm2
```

A bidder bids only when its operator vouches for you and you vouch for it:
[docs/quickstart-prod.md](docs/quickstart-prod.md#2-vouches-on-prod). More (agents, `kubectl apply` of `Infra`,
a requester as a systemd unit): [docs/quickstart-prod.md](docs/quickstart-prod.md).

Kubernetes clusters: [k8s.socialweb.computer](https://k8s.socialweb.computer/) (sign in, create, get a
kubeconfig, revoke it); CLI: [docs/gateway.md](docs/gateway.md#kubernetes-clusters).

## Host a bidder

Linux with `/dev/kvm`. [docs/bidder.md](docs/bidder.md): prerequisites, sign-in, foreground, installed, upgrade.

```sh
sudo "$(command -v socialweb-computer)" install bidder --handle <your handle> --bidder-operator-signin <your handle>
```

## Troubleshooting

| symptom | fix |
| --- | --- |
| `no bids received within 30s` | a vouch is missing (both directions), or denied less than 5 min ago (cached) |
| no bids, both vouches exist | the bidder is not its operator's agent: [docs/bidder.md](docs/bidder.md#association) |
| `lacks scopes the identity now needs` / `SessionExpired` after an upgrade | sign in again from the new URL |
| `journalctl --user` of `swc-bidder` / `swc-requester` is empty | system accounts log to the system journal: `sudo journalctl _SYSTEMD_USER_UNIT=<unit>` |
| a restart is refused | options after `--` differ from the first start: give the same ones, none, or a new `--name` |

## Docs

| doc | what |
| --- | --- |
| [docs/bidder.md](docs/bidder.md) | host a bidder |
| [docs/quickstart-prod.md](docs/quickstart-prod.md) | requester in depth: vouches, `Infra` objects, agents, `install requester` |
| [docs/quickstart-local.md](docs/quickstart-local.md) | a whole market on one host (dev) |
| [CONTRIBUTING.md](CONTRIBUTING.md) | build, release, gateway |
| [docs/README.md](docs/README.md) | every other doc |

## License

Unlicense (public domain).
