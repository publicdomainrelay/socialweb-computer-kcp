# The gateway and its web app

The long form of the gateway sections of the README: the deploy script's environment, the web app, the caddy sites and the post-deploy sign-in. Short path in [the README](../README.md#run-the-gateway-and-its-web-app).

A development gateway is a child of a market network: start the network with `--with-gateway`.

A production gateway is built as a release, shipped, and installed or upgraded with `scripts/deploy.sh`, which also writes the caddy sites.

```bash
BINARY=<release binary> SHA256=<its sha256> SSH_KEY=~/.ssh/id_ed25519_no_password \
  GATEWAY_ENV_FILE=~/.tmp/deploy/gateway.env GATEWAY_SPEC_FILE=~/.tmp/deploy/gateway-spec.yaml scripts/deploy.sh
```

[Deploy a release to production](#deploy-a-release-to-production) builds that binary and computes its sha256.

`scripts/deploy.sh` reads these environment variables:

| Variable | Meaning | Default |
|---|---|---|
| `BINARY` | The release binary to ship. Unset: the script builds the current commit. | |
| `SHA256` | The published sha256 of `BINARY`; a mismatch stops the deploy. | |
| `SSH_KEY` | The SSH key for the deploy host. | |
| `SSH_TARGET` | The deploy host. | `root@socialweb.computer` |
| `SSH_PORT` | The deploy host's SSH port. | `1997` |
| `ACCOUNT` | The account the gateway runs as. | `socialweb` |
| `STATE_DIR` | The gateway's state directory. | `/var/lib/socialweb-computer/socialweb` |
| `DOMAIN` | The base domain. | `socialweb.computer` |
| `ORIGIN` | The web app's host. | `k8s.<DOMAIN>` |
| `KCP_ORIGIN` | The kcp host. | `kcp.<DOMAIN>` |
| `GATEWAY_ENV_FILE` | The gateway's environment file, shipped as `gateway.env`. | |
| `GATEWAY_SPEC_FILE` | A YAML `GatewaySpec`, shipped as the Gateway spec. | |

The script installs the gateway on its first run. On every later run it upgrades the gateway, then runs `install gateway` again with the new `bin/current`, which re-renders the unit files and restarts a core whose unit changed. The web app (`socialweb-computer-k8s`) serves `k8s.<domain>`, and caddy proxies the web app and the `/xrpc/computer.socialweb.k8s.*` routes to the gateway.

After a fresh deploy the requester account has no web app session, so cluster claims fail with a `no oauth session stored` message. Sign in with [Sign in with OAuth](#sign-in-with-oauth), `--url https://k8s.socialweb.computer/` and `--start-api`.

## See also

- [Deploy a release](deploy.md)
- [OAuth sign-in](oauth.md)
