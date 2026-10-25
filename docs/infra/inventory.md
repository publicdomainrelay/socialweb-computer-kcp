# Infra inventory: the old org-root and its hosts

What runs where, who deployed it, and which `infra/` script owns it now. Read on 2026-10-25 (UTC).
How to deploy: [infra/README.md](../../infra/README.md).

Sources:

- Read-only ssh (`systemctl show`, `ss -ltnp`, the caddy site lines, `git log -1` of each checkout, the
  NAMES of env files and of `Environment=` variables) on fedproxy.com, mini-cloud-0001.chadig.com,
  mini-cloud-0002.chadig.com and qr.fedfork.com. Nothing was changed. No env file was read.
- Not reached by ssh: socialweb.computer (the gateway), hat-7, hat-9 and this host (hat-8). The task
  rules forbid ssh to the hats and the gateway. Their rows come from `scripts/deploy.sh`,
  `CONTRIBUTING.md`, `docs/deploy.md`, `~/.tmp/deploy/README.md` and public HTTPS probes.
  tray.fedfork.com is not in the known host list and was probed over HTTPS only.
- The old org-root: `~/src/publicdomainrelay-kcp` (origin `github.com/publicdomainrelay/socialweb-computer`,
  HEAD 6ed8c72). Its deploy scripts: `atproto-reverse-proxy/scripts/deploy.sh` and
  `deploy-fluent-bit.sh`, `social-web-computer/scripts/deploy.sh`, `compute-spa/scripts/deploy.sh`,
  `deno-macos-runner-desktop/scripts/deploy.sh`, `did-key-associator/scripts/deploy.sh`,
  `socialweb-computer-kcp/scripts/deploy.sh` and `deploy-pds2.sh`, `socialweb-computer-ssh/scripts/deploy.sh`,
  `deno-kcp/deploy/install-provider.sh` (a kubectl install into a kcp workspace, not a host deploy),
  `gitlab-autoscale-socialweb-computer/systemd/gitlab-runner.service`, the Caddyfiles, `CI.md`.

## Summary

State: `go` replaced by Go and live, `wired` deployed by an `infra/` script that works today,
`in progress` a Go replacement is being built, `legacy` live but not managed, `dead` nothing serves it.

| Host | Component | Unit | Listen | Public name | Source | Deployed by (old) | infra/ owner | State |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| fedproxy.com | ingress-relay | `xrpc-relay-server.service` + drop-in `30-go.conf` | unix `/opt/caddy/xrpc-relay-server.sock` | xrpc.fedproxy.com, *.xrpc.fedproxy.com | `/opt/xrpc-relay-go/current` -> 64491ad29931 (previous d33616e310c3) | `scripts/deploy-xrpc-relay.sh` | `infra/ingress-relay` | go, wired |
| fedproxy.com | deno did-key ingress (fallback) | drop-in `20-socialweb-computer-kcp.conf` (overridden by 30-go) | same socket | same | `/opt/xrpc-relay-kcp/src` 0b7b0af, deno `/opt/xrpc-relay-kcp/deno-2.9.3` | by hand | `infra/ingress-relay --rollback` falls back to it | go (fallback kept) |
| fedproxy.com | caddy (xcaddy, cloudflare dns) | `caddy.service` (`/usr/lib/systemd/system`) | 80, 443, admin unix `/opt/caddy/caddy-admin.sock` | fedproxy.com (`/var/www/html`), rp, xrpc, *.xrpc, xrpc-test, *.xrpc-test | `/etc/caddy/Caddyfile` | `atproto-reverse-proxy/scripts/deploy.sh` (prints CF_API_TOKEN; never run) | `infra/fedproxy-legacy` | legacy |
| fedproxy.com | atprp-ssh-relay | `atprp-ssh-relay.service` | 2222, 127.0.0.1:6060 | ssh relay of fedproxy | `/usr/bin/atprp-ssh-relay` (Go, atproto-reverse-proxy) | same | `infra/fedproxy-legacy` | legacy |
| fedproxy.com | oauth-client-webapp | `oauth-client-webapp.service` | unix `/opt/caddy/web-ui.sock` | rp.fedproxy.com | `/usr/bin/oauth-client-webapp` | same | `infra/fedproxy-legacy` | legacy |
| fedproxy.com | caddy-check-dns-from-config | `caddy-check-dns-from-config.service` | 127.0.0.1:5555 | none (on-demand TLS ask) | `/usr/bin/caddy-check-dns-from-config` | same | `infra/fedproxy-legacy` | legacy |
| fedproxy.com | xrpc-test relay | none | `/opt/caddy/xrpc-relay-server-test.sock` missing | xrpc-test.fedproxy.com (502) | none | none | none | dead (caddy block left) |
| fedproxy.com | pre-iroh relay checkout | none | none | none | `/opt/xrpc-relay` ee6e4a8 (branch pre-iroh) | old deploy.sh `git clone` | none | dead (files left) |
| mini-cloud-0002.chadig.com | atproto-relay (deno) | `atproto-relay.service` (User johnandersen777) | 127.0.0.1:2584 | relay.mini-cloud-0002.chadig.com | `~/prod-org-root/atproto-relay` 9c26e76 | by hand from the prod-org-root checkout | `infra/atproto-relay` (stub until `scripts/deploy-atproto-relay.sh` lands) | in progress (Go `internal atproto-relay`) |
| mini-cloud-0002.chadig.com | hono-jsr | `hono-jsr.service` (User johnandersen777) | 0.0.0.0:8888, no caddy site | none here (jsr.publicdomainrelay.com points at mini-cloud-0001) | `~/prod-org-root/hono-jsr` 0de4055 | by hand | `infra/hono-jsr` | legacy |
| mini-cloud-0002.chadig.com | org-root git pull | `git-pull.timer` (every 5 min) -> `git-pull.service` (`~/bin/git-pull-and-rebuild.sh`) | none | none | `~/prod-org-root` ea522d1 | by hand | `infra/org-root-git-pull` | legacy, failing |
| mini-cloud-0002.chadig.com | hono-bidder (deno) | `bidder.service` (enabled, inactive) | none | none | `~/prod-org-root/atproto-market/hono-bidder` | by hand | none | dead |
| mini-cloud-0002.chadig.com | spindle (deno poc) | `spindle.service` (disabled, inactive) | none | none | `~/prod-compute-contract-reference-implementation-poc` | by hand | none | dead |
| mini-cloud-0002.chadig.com | oidc-issuer | none | 127.0.0.1:9000: nothing listens | mini-cloud-0002.fedfork.com (NXDOMAIN) | none | deno-macos-runner-desktop / digitalocean-bidder era | `infra/oidc-issuer` | dead (caddy block left) |
| mini-cloud-0001.chadig.com (ssh port 1997) | spindle (deno poc) | `spindle.service` | 0.0.0.0:7777 | *.gha.spindle.tangled.fedcicd.com | `~/prod-compute-contract-reference-implementation-poc` ef299b8 | by hand | `infra/legacy-poc` | legacy |
| mini-cloud-0001.chadig.com | market-registry (deno poc) | `market-registry.service`, `market-registry-tunnel.service` (ssh -R) | 0.0.0.0:4031 | via the tunnel | same | by hand | `infra/legacy-poc` | legacy |
| mini-cloud-0001.chadig.com | bidder tunnel | `bidder-tunnel.service` (ssh -R); `bidder.service` inactive | none | none | same | by hand | `infra/legacy-poc` | legacy |
| mini-cloud-0001.chadig.com | claude-rc | `claude-rc.service` (enabled, inactive) | none | none | `~/compute-contract-reference-implementation-poc` | by hand | `infra/legacy-poc` | dead |
| mini-cloud-0001.chadig.com | fluent-bit | `fluent-bit.service` | none | none (ships to OpenSearch) | `/opt/fluent-bit` | `atproto-reverse-proxy/scripts/deploy-fluent-bit.sh` | `infra/fluent-bit` | legacy |
| mini-cloud-0001.chadig.com | caddy | `caddy.service` + `override.conf` | 80, 443, 127.0.0.1:2019 | ui, test.ui (static), dev.ui :5177, docs.ui :3030, git.ui :9876, knot1 :5555, tangled.fedcicd.com :3000, gha.spindle (static), jsr.publicdomainrelay.com :8888, mini-cloud-0001.fedfork.com :9000 | `/etc/caddy/Caddyfile` | by hand | `infra/legacy-poc` (status) | legacy; only ui.fedfork.com answers 200, the proxied names answer 502 |
| qr.fedfork.com | did-key-associator (QR session transfer) | `qr-fedfork-backend.service` (root) + `caddy.service` | 0.0.0.0:5557, 80, 443 | qr.fedfork.com (`/var/www/qr.fedfork.com`, `/xrpc/*` -> :5557) | `/opt/qr-fedfork/qr-fedfork-backend` from `~/src/kcp/did-key-associator` | its `scripts/deploy.sh` (backups `/root/qr-backup/<ts>`, `restore.sh`) | `infra/did-key-associator` | wired |
| socialweb.computer (ssh port 1997) | gateway | `socialweb-computer-gateway.service` in `socialweb`'s user manager | web app 127.0.0.1:8788, kcp front 6443, TLS front 8443, cert front 8444 | k8s.socialweb.computer, kcp.socialweb.computer | release binary at `/var/lib/socialweb-computer/socialweb/bin/current` | `scripts/deploy.sh` with `~/.tmp/deploy` (orphan branch `swc-deploy`) | `infra/gateway` | go, wired (not ssh-verified) |
| socialweb.computer | old static site, digitalocean-bidder | `digitalocean-bidder.service` (old) | 127.0.0.1:8000 | socialweb.computer (200), digitalocean.socialweb.computer (502) | `/opt/org-root` | `social-web-computer/scripts/deploy.sh` | none | replaced by the gateway; the bidder is dead |
| socialweb.computer | ssh door | `socialweb-computer-ssh.service` (old deno) or the Go SshGateway | 22 (host sshd on 1997) | ssh.socialweb.computer (200) | `/opt/org-root/socialweb-computer-ssh` (old) | `socialweb-computer-ssh/scripts/deploy.sh` | none yet | replaced by Go (`run gateway ssh`); which one serves now is not verified |
| socialweb.computer | pds2 | `pds2.service` | 127.0.0.1:8794 | pds2.socialweb.computer (no answer) | `ts/hono-pds` of a socialweb-computer-kcp checkout | `scripts/deploy-pds2.sh` | none | dead (the TypeScript it runs is gone from this tree) |
| hat-7 | bidder | `socialweb-computer-bidder@<handle>.service` in `swc-bidder`'s user manager | port 0 | none | `/var/lib/socialweb-computer/swc-bidder/bin/current` | `install bidder` / `upgrade` over ssh (CONTRIBUTING) | `infra/bidder` | go, wired (not ssh-verified) |
| hat-9 | bidder | same | port 0 | none | same | same | `infra/bidder` | go, wired (not ssh-verified) |
| hat-8 (local) | bidder | same (account `swc-bidder`, uid 947); install records also for test-0002 and test-0003 | port 0 | none | same; staged releases in `/var/tmp/swc-release` | same, run locally | `infra/bidder` (`SSH_TARGET=local`) | go, wired |
| tray.fedfork.com | tray static site | `caddy.service` | 80, 443 | tray.fedfork.com (404) | `/var/www/tray.fedfork.com` | `deno-macos-runner-desktop/scripts/deploy.sh` | `infra/tray` | legacy (not ssh-verified) |
| compute.fedfork.com | compute-spa | caddy | - | NXDOMAIN | `/var/www/compute-spa` | `compute-spa/scripts/deploy.sh` | none | dead |
| plc, jsr, reg.market .fedfork.com | old CI deploy targets | - | - | NXDOMAIN | hono-plc, hono-package-registry, hono-atproto-relay | old `.github/workflows/deploy.yml` (opkssh, user `deploy`) | none | dead |

## Secret and env file paths (names only, never read)

| Host | Path or unit | Variable names |
| --- | --- | --- |
| fedproxy.com | `caddy.service` (`/usr/lib/systemd/system`) `Environment=` | CF_API_TOKEN |
| fedproxy.com | `atprp-ssh-relay.service` `Environment=` | CF_API_TOKEN, THIS_ENDPOINT, CADDY_SOCK, GOMEMLIMIT |
| fedproxy.com | `oauth-client-webapp.service` `Environment=` | THIS_ENDPOINT, LISTEN_SOCKET, DATABASE_URI (sqlite `/opt/caddy/oauth-client-webapp.db`), GOMEMLIMIT |
| fedproxy.com | `xrpc-relay-server.service` and its drop-ins | UNIX_SOCKET, HOSTNAME, DENO_DIR |
| mini-cloud-0002.chadig.com | `~/prod-org-root/atproto-relay/hono-atproto-relay/atproto-relay.env`, `.env.secrets` | not read |
| mini-cloud-0002.chadig.com | `~/prod-org-root/atproto-market/hono-bidder/bidder.env`, `.env.secrets` | not read |
| mini-cloud-0001.chadig.com | `~/prod-compute-contract-reference-implementation-poc/src/typescript/{spindle,market-registry,bidder}/*.env`, `*.env.secrets`, `*-tunnel.env` | not read |
| mini-cloud-0001.chadig.com | `caddy.service` and `/etc/systemd/system/caddy.service.d/override.conf` | CF_API_TOKEN |
| mini-cloud-0001.chadig.com | `/etc/default/fluent-bit`, `/etc/sysconfig/fluent-bit` (optional) | not read |
| socialweb.computer | `/var/lib/socialweb-computer/socialweb/gateway.env` (from `~/.tmp/deploy/gateway.env`) | SOCIALWEB_GATEWAY_AUDIENCES, SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST, SOCIALWEB_CLUSTERPROVIDER_JOIN, SOCIALWEB_CLUSTERPROVIDER_MARKET_DENY_BIDDER_DIDS, SOCIALWEB_CLUSTERPROVIDER_SCALE_MAX_NODES, SOCIALWEB_CLUSTERPROVIDER_SCALE_PODS_PER_NODE, SOCIALWEB_CLUSTERPROVIDER_ESO_NAMESPACE |
| deploy host | `~/.aliceoa_bsky_social_source`, `~/.bobvmbuilder_bsky_social_source` (mode 0600) | ALICE_ATPROTO_HANDLE, ALICE_ATPROTO_PASSWORD, BOB_ATPROTO_HANDLE, BOB_ATPROTO_PASSWORD |

CF_API_TOKEN sits inline in unit files on fedproxy.com and mini-cloud-0001. When those units are ported,
it moves to OpenBao behind a SecretRef; `infra/` never carries it.

## Deploy mechanisms, old and new

| Mechanism | Where | Targets | Now |
| --- | --- | --- | --- |
| `atproto-reverse-proxy/scripts/deploy.sh` (scp to `/tmp/stage`, `bash -xe` over ssh, writes caddy and four units, `git clone` of the pre-iroh relay) | old org-root | fedproxy.com | never run: it echoes CF_API_TOKEN. `infra/fedproxy-legacy` is the stub that will replace it |
| `atproto-reverse-proxy/scripts/deploy-fluent-bit.sh` (doctl lookups, OpenSearch) | old org-root | any droplet (mini-cloud-0001 has it) | `infra/fluent-bit` stub |
| `social-web-computer/scripts/deploy.sh` (static site, tar of the org-root to `/opt/org-root`, digitalocean-bidder unit) | old org-root | socialweb.computer | replaced by the gateway (`scripts/deploy.sh`) |
| `socialweb-computer-ssh/scripts/deploy.sh` (deno ssh door, moves host sshd to 1997) | old org-root | socialweb.computer | replaced by the Go SshGateway |
| `compute-spa/scripts/deploy.sh` | old org-root | compute.fedfork.com | dead (NXDOMAIN) |
| `deno-macos-runner-desktop/scripts/deploy.sh` | old org-root | tray.fedfork.com | `infra/tray` stub |
| `did-key-associator/scripts/deploy.sh` | `~/src/kcp/did-key-associator` (the old org-root copy is older) | qr.fedfork.com | wrapped by `infra/did-key-associator` |
| `socialweb-computer-kcp/scripts/deploy.sh` (old copy) | old org-root | socialweb.computer | superseded by `scripts/deploy.sh` here |
| `scripts/deploy-pds2.sh` | old org-root and this tree | socialweb.computer | dead: runs `ts/hono-pds/main.ts`, which this tree no longer has |
| `.github/workflows/deploy.yml` (opkssh, user `deploy`, six hosts) | old org-root, `atproto-reverse-proxy` | fedproxy, compute, qr, tray, plc, reg.market, jsr | removed ("ci: remove the Deploy workflow"); CI never deploys |
| `git-pull.timer` (pull every 5 min, `reset --hard`, submodules, compute-spa rebuild) | mini-cloud-0002 | itself | failing on the `atproto-relay` submodule checkout; `infra/org-root-git-pull` stub |
| `gitlab-runner.service` (user unit, `~/bin/gitlab-runner`) | old org-root `gitlab-autoscale-socialweb-computer` | the gitlab host (ssh alias `gitlab`) | not a deploy target here |
| `scripts/deploy-xrpc-relay.sh` | this tree | fedproxy.com | kept as is; `infra/ingress-relay` is its port |
| `scripts/deploy.sh` + `~/.tmp/deploy` (gateway.env, gateway-spec.yaml) | this tree, orphan branch `swc-deploy` | socialweb.computer | wrapped by `infra/gateway` |
| `install bidder` / `upgrade --binary --sha256` / `rollback` over ssh | this tree (CONTRIBUTING, docs/deploy.md) | hat-7, hat-9, local | `infra/bidder` |
| `scripts/deploy-atproto-relay.sh` (being written) | this tree | mini-cloud-0002 | `infra/atproto-relay` delegates to it once it lands |

## Findings

- mini-cloud-0002 `git-pull.service` fails every 5 minutes (`Unable to checkout ... in submodule path
  'atproto-relay'`), and it runs `git reset --hard` on the checkout the live atproto relay runs from.
- hono-jsr listens on 0.0.0.0:8888 on mini-cloud-0002 with no caddy in front.
- Caddy blocks with no upstream: xrpc-test.fedproxy.com, mini-cloud-0002.fedfork.com (also NXDOMAIN),
  mini-cloud-0001.fedfork.com, jsr.publicdomainrelay.com, knot1.fedfork.com, tangled.fedcicd.com.
- fedproxy.com keeps the deno relay drop-in `20-socialweb-computer-kcp.conf` under `30-go.conf`; that
  is the rollback target of `infra/ingress-relay --rollback`. `/opt/xrpc-relay` (pre-iroh) is unused.
- `/opt/xrpc-relay-go` holds four versions (64491ad29931 current, d33616e310c3 previous, b0a0710162b3,
  d7800c7668c2); nothing prunes them.
- The prod-org-root checkouts on mini-cloud-0002 point at `org-root-dispatcher-typescript`, the
  org-root's older repository name.
