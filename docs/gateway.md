# The gateway and its API front

The deploy script's environment, the gateway's API front, the caddy sites and the post-deploy sign-in.

A development gateway is a child of a market network: start the network with `--with-gateway`.

A production gateway is built as a release, shipped, and installed or upgraded with `scripts/deploy.sh`, which also writes the caddy sites.

```bash
BINARY=<release binary> SHA256=<its sha256> SSH_KEY=~/.ssh/id_ed25519_no_password \
  GATEWAY_ENV_FILE=~/.tmp/deploy/gateway.env GATEWAY_SPEC_FILE=~/.tmp/deploy/gateway-spec.yaml scripts/deploy.sh
```

[Deploying a release to production](deploy.md#deploying-a-release-to-production) builds that binary and computes its sha256.

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
| `ORIGIN` | The gateway API's host. | `k8s.<DOMAIN>` |
| `KCP_ORIGIN` | The kcp host. | `kcp.<DOMAIN>` |
| `GATEWAY_ENV_FILE` | The gateway's environment file, shipped as `gateway.env`. | |
| `GATEWAY_SPEC_FILE` | A YAML `GatewaySpec`, shipped as the Gateway spec. | |
| `ALLOW_DEV_BINARY` | Pass `--allow-dev-binary` to `install gateway` and `upgrade`. | off |

The script installs the gateway on its first run. On every later run it upgrades the gateway, then runs `install gateway` again with the new `bin/current`, which re-renders the unit files and restarts a core whose unit changed. `k8s.<domain>` proxies the gateway's web-app front on `127.0.0.1:8788`, which serves the GatewayAPI component: the page at `/`, `/oauth/*`, `/session`, `/api/*`, `/xrpc/*` and `/downloads/*`.

After a fresh deploy the requester account has no gateway session, so cluster claims fail with a `no oauth session stored` message. Sign in with [OAuth sign-in](oauth.md), `--url https://k8s.socialweb.computer/` and `--start-api`.

## Kubernetes clusters

Browser: [k8s.socialweb.computer](https://k8s.socialweb.computer/). Sign in, create a cluster, pick a
lifetime (1, 8 or 24 hours, or no expiry), get the kubeconfig, revoke it.

CLI, with a `goat account login` session:

```sh
GW=https://k8s.socialweb.computer
AUD=did:web:k8s.socialweb.computer
N=com.publicdomainrelay.temp.cluster
CL=my-cluster
xrpc() {
  verb=$1
  method=$2
  shift 2
  goat xrpc "$verb" "$GW" "$N.$method" "$@" "Authorization:Bearer $(goat account service-auth --aud "$AUD" --lxm "$N.$method")"
}
xrpc procedure createCluster name="$CL"
until xrpc query listClusters | jq -e --arg n "$CL" '.clusters[] | select(.name==$n and .phase=="Ready")' >/dev/null; do sleep 5; done
umask 077
xrpc procedure getKubeconfig name="$CL" ttlSeconds:=0 | jq -r .kubeconfig > "$CL.kubeconfig.yaml"
KUBECONFIG=./$CL.kubeconfig.yaml kubectl get ns
```

`ttlSeconds:=0` (or `noExpiry:=true`) never expires; `ttlSeconds:=3600`, `28800` or `86400` do.

Revoke the newest:

```sh
xrpc query listKubeconfigs name=="$CL" | jq '.kubeconfigs[] | select(.revoked|not)'
S=$(xrpc query listKubeconfigs name=="$CL" | jq -r '[.kubeconfigs[] | select(.revoked|not)] | sort_by(.createdAt) | last | .serial')
xrpc procedure revokeKubeconfig name="$CL" serial="$S"
```

Every kubeconfig of a cluster is also revoked by deleting the cluster, by `revokeIdentity`, and by signing
out on the page. A kubeconfig works only while its `ClusterClientCertificate` record exists and is not
revoked ([ARCHITECTURE.md](ARCHITECTURE.md), "A no-expiry kubeconfig is revoked by its record").

## See also

- [Deploy a release](deploy.md)
- [OAuth sign-in](oauth.md)
