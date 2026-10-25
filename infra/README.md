# infra: deploys over ssh, from this machine

One directory per component (`infra/<component>/deploy.sh`), one script per host
(`infra/hosts/<slug>.sh`), and the shared functions in `infra/lib/common.sh`. What runs on each host,
and which old script deployed it: [docs/infra/inventory.md](../docs/infra/inventory.md).

## Rules

- No secrets in this repository: no tokens, keys, passwords or env file contents, not in a script, a
  default, an argument or a log line. A script names a secret file by its path only (the gateway's
  `GATEWAY_ENV_FILE`), tests that it exists and hands the path to the deploy; it never reads it.
  New secrets live in OpenBao behind a SecretRef, read by the callee over the bus (`vault.read`).
- Deploys run only from the lead's workstation, over ssh with the lead's own keys. CI never gets a
  deploy key and never deploys. There is no pull-based deploy (the mini-cloud-0002 `git-pull.timer` is
  a legacy to retire).
- Only the lead runs `--apply` against prod (AGENTS.md "Prod deployments"). Task agents run the dry-run
  and `--status` only.
- Never run `atproto-reverse-proxy/scripts/deploy.sh` of the old org-root: it prints CF_API_TOKEN.
- Shell style of `scripts/`: bash, `set -euo pipefail` in the entry point, no comments, ASCII only.

## Usage

Every host script and every component script takes the same flags:

| Flags | What it does |
| --- | --- |
| (none) | the deploy, printed only: no ssh, no build, nothing changes. Dry-run is the default |
| `--apply` | run the deploy: build, ship (sha256 checked), install, restart, health check |
| `--status` | read-only over ssh: unit state, versions, caddy sites, public health. Changes nothing |
| `--status --dry-run` | print the read-only commands and run none (no ssh) |
| `--rollback` | print the rollback; `--rollback --apply` runs it |
| `--only <component>` | host scripts: only this component (repeat, or comma separated) |
| `--list` | host scripts: print the components of the host |

```bash
infra/hosts/fedproxy-com.sh                      # dry-run of every component on fedproxy.com
infra/hosts/fedproxy-com.sh --status             # read-only
infra/hosts/fedproxy-com.sh --only ingress-relay --apply
infra/ingress-relay/deploy.sh --rollback --apply # a component alone, with its own default target
infra/hosts/hat-9.sh --status
BIDDER_HANDLE=<handle> infra/hosts/hat-9.sh --apply
```

Common env: `SSH_TARGET` (`user@host`, an `~/.ssh/config` alias, or `local` for this machine),
`SSH_KEY` (identity file; set it empty to use `~/.ssh/config`), `SSH_PORT`, `INFRA_SUDO` (prefix of
root commands for a non-root login, `sudo -n` on the hats and local). Each host script sets these for
its host; the environment overrides them.

## Hosts

| Host script | Target | Components |
| --- | --- | --- |
| `hosts/fedproxy-com.sh` | `root@fedproxy.com`, `~/.ssh/id_ed25519_no_password` | ingress-relay, fedproxy-legacy |
| `hosts/mini-cloud-0002-chadig-com.sh` | `root@mini-cloud-0002.chadig.com`, same key | atproto-relay, hono-jsr, org-root-git-pull, oidc-issuer |
| `hosts/mini-cloud-0001-chadig-com.sh` | `root@mini-cloud-0001.chadig.com:1997`, same key | legacy-poc, fluent-bit, oidc-issuer |
| `hosts/qr-fedfork-com.sh` | `root@qr.fedfork.com`, same key | did-key-associator |
| `hosts/socialweb-computer.sh` | `root@socialweb.computer:1997`, same key | gateway |
| `hosts/hat-7.sh`, `hosts/hat-9.sh` | ssh alias `hat-7`, `hat-9`, `sudo -n` | bidder |
| `hosts/local.sh` | this machine (hat-8), `sudo -n` | bidder |
| `hosts/tray-fedfork-com.sh` | `root@tray.fedfork.com`, same key | tray |

## Components

| Component | State | Deploy | Rollback | Env |
| --- | --- | --- | --- | --- |
| `ingress-relay` | wired: the port of `scripts/deploy-xrpc-relay.sh` | build `socialweb-computer` linux/amd64 (CGO off), ship to `/opt/xrpc-relay-go/<version>/`, flip `current`, write the `30-go.conf` drop-in, restart, wait for the socket, check `_health` carries the commit and `did.json` answers 200; a failed check rolls back | remove `30-go.conf` (back to the deno drop-in) | `INGRESS_RELAY_HOSTNAME`, `INGRESS_RELAY_SOCKET`, `INGRESS_RELAY_BASE`, `INGRESS_RELAY_OUT_ROOT`, `INGRESS_RELAY_NO_AUTO_ROLLBACK=1` |
| `did-key-associator` | wired: wraps `~/src/kcp/did-key-associator/scripts/deploy.sh` | its deploy (backup, upload, caddy validate, restore on failure), then the health of `/` (200) and `/xrpc/com.fedfork.atprotoOauthQR` (401) | its latest `/root/qr-backup/<ts>/restore.sh` | `DID_KEY_ASSOCIATOR_REPO`, `DID_KEY_ASSOCIATOR_MODE=full\|static`, `DID_KEY_ASSOCIATOR_BACKUP_TS`, `DID_KEY_ASSOCIATOR_DEEP_DRY_RUN=1` |
| `bidder` | wired: the release install and upgrade of CONTRIBUTING | ship the release binary to `/var/tmp/swc-release/socialweb-computer-<sha12>`, then `upgrade --binary --sha256` as `swc-bidder` and, with a handle, `install bidder` again; the first install needs `BIDDER_HANDLE` | `socialweb-computer rollback` as the account | `BIDDER_BINARY`, `BIDDER_SHA256`, `BIDDER_HANDLE`, `BIDDER_ALLOW_DEV_BINARY=1`, `BIDDER_ACCOUNT`, `BIDDER_STATE`, `BIDDER_PLATFORM`, `INFRA_COMMIT` |
| `gateway` | wired: wraps `scripts/deploy.sh` | the release binary of HEAD (built when missing), `GATEWAY_ENV_FILE` and `GATEWAY_SPEC_FILE` from `~/.tmp/deploy` by path; then the health of `https://k8s.socialweb.computer/api/config` | `socialweb-computer rollback` as `socialweb` | `GATEWAY_BINARY`, `GATEWAY_SHA256`, `GATEWAY_ENV_FILE`, `GATEWAY_SPEC_FILE`, `ALLOW_DEV_BINARY=1`, `GATEWAY_DOMAIN`, `INFRA_COMMIT` |
| `atproto-relay` | stub: delegates to `scripts/deploy-atproto-relay.sh` once it lands; status is read-only probes of the deno relay | that script | that script `--rollback` | `ATPROTO_RELAY_SCRIPT`, `ATPROTO_RELAY_HOSTNAME` |
| `fedproxy-legacy`, `hono-jsr`, `org-root-git-pull`, `fluent-bit`, `legacy-poc`, `oidc-issuer`, `tray` | stubs: `--status` works; deploy and rollback print what is left to port and refuse `--apply` | - | - | - |

`scripts/deploy-xrpc-relay.sh` stays as it is (AGENTS.md names it). After the lead has run
`infra/hosts/fedproxy-com.sh --only ingress-relay --apply` once, it can become
`exec "$(dirname "$0")/../infra/ingress-relay/deploy.sh" --apply` (and `--rollback --apply`, `--status`).
`infra/atproto-relay` folds in `scripts/deploy-atproto-relay.sh` the same way once that script lands.

## The functions in lib/common.sh

| Function | What it does |
| --- | --- |
| `infra_ro`, `infra_ro_root` | a read-only command on the target (printed in a dry-run) |
| `infra_rw`, `infra_rw_root`, `infra_rw_root_stdin` | a command that changes the target (runs only with `--apply`) |
| `infra_local_rw` | a local step that builds or changes something (runs only with `--apply`) |
| `infra_upload <src> <dest> [mode] [root\|user]` | ship a file through a `.partial` and `mv`, then compare its sha256 on the target |
| `infra_file_install <dest> <mode> <content>` | write a file, the old one kept as `<dest>.infra-bak.<ts>` |
| `infra_unit_install`, `infra_unit_dropin`, `infra_unit_restart`, `infra_unit_state` | systemd units and drop-ins |
| `infra_caddy_site_install <name> <content>` | write `/etc/caddy/sites/<name>.caddy`, add the import, `caddy validate`; a failed validate restores the backup in `/var/backups/infra/caddy/<ts>` and stops before any reload |
| `infra_caddy_restore <backup dir>`, `infra_caddy_sites` | restore a caddy backup; list the site lines |
| `infra_fetch`, `infra_health_wait`, `infra_report_urls` | public health checks from this machine |
| `infra_release_binary <platform>` | the release binary and sha256 of `INFRA_COMMIT` (HEAD), built with `scripts/build-socialweb-computer.sh` when missing |
| `infra_component_main`, `infra_host_main` | the flag parsing and the dispatch to `<component>_deploy`, `_status`, `_rollback` |

A new component: `infra/<name>/deploy.sh` that sources `lib/common.sh` when it is not loaded, defines
`<name>_deploy`, `<name>_status` and `<name>_rollback` (dashes become underscores), and ends with
`infra_component_main <name> <target> <key> <port> <sudo> "$@"` when run directly. Add it to the
`INFRA_COMPONENTS` of its host script.
