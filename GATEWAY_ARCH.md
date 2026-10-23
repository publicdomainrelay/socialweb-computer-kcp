# Gateway architecture

Observed on 2026-10-22 on the installed production gateway `socialweb.computer` and from the Go
source at commit `5fad226a` (release binary sha256
`30ab2e465a080bbe47ea16e085cdd99d10c640f4c90d438484363a443b31bd92`).

## Shape

One `socialweb-computer` core owns the gateway and its kcp. The gateway is an application root in
that kcp; its components run in the core and its infrastructure runs as HostProcess units. There is
no web-app DenoPod and no per-component systemd application unit.

```
public
  k8s.socialweb.computer ──caddy──> 127.0.0.1:8788  (web-app front)
  kcp.socialweb.computer ──caddy──> 127.0.0.1:6443  (kcp, no client cert)
                              └───> 127.0.0.1:8444  (client-certificate front, with cert)

systemd --user (account socialweb, uid 982), state /var/lib/socialweb-computer/socialweb
  socialweb.slice
  ├─ socialweb-computer-gateway.service                 core: serve gateway kcp
  ├─ socialweb-kcp-gw@a.service / @b.service            blue/green kcp slots
  ├─ socialweb-kcp-gw-kine@a / @b.service               kine on the shared Postgres
  ├─ socialweb-kcp-gw-pg.service                        Postgres (unix socket)
  ├─ socialweb-kcp-gw-front.service                     L4 front for kcp
  └─ socialweb-hp-gw-bao-root-vault.service             OpenBao HostProcess

core goroutines
  ├─ devcore: kcp slot, bootstrap, HostProcess supervisor
  ├─ appctl: Gateway root, PDS, Requester, GatewayAPI, PolicyEngine,
  │          MarketIdentity, Service, Exposure, ComponentLink
  ├─ componentcore: in-process handlers for every component
  ├─ cluster provider: Cluster, NodeClaim, tenant bootstrap, scale driver
  └─ PolicyEngine and StateSync workers

process-local listeners
  127.0.0.1:6443   kcp API
  127.0.0.1:8443   TLS front for the gateway XRPC listener
  127.0.0.1:8444   client-certificate front for kcp
  127.0.0.1:8788   web-app front; its backend is the GatewayAPI local exposure
  127.0.0.1:20000  Pds local Exposure
  127.0.0.1:20001  Requester local Exposure
  127.0.0.1:20002  GatewayAPI local Exposure
```

The local port numbers are leases and may move between component restarts. The stable entry points are
the fixed fronts 6443, 8443, 8444 and 8788. `gatewaykcp` resolves the 8788 backend from the
GatewayAPI local Exposure when the API-only gateway has no `socialweb-computer-k8s` component.

## Public front

`/etc/caddy/sites/k8s.socialweb.computer.caddy` sends the whole public origin to
`http://127.0.0.1:8788`. That front proxies to the GatewayAPI exposure, which serves:

- `POST /oauth/start`, `GET /oauth/callback`, `GET|DELETE /session`
- `GET /api/config`, `POST /api/clusters/{list,create,up,down,delete,kubeconfig}`
- `/xrpc/*` for the gateway server
- `/downloads/*`

`/etc/caddy/sites/kcp.socialweb.computer.caddy` keeps client-certificate-aware routing: a request
without a client certificate goes to kcp 6443; a request with one goes to the 8444 certificate front
with `X-Socialweb-Client-Certificate`, and Caddy authenticates to that front with the gateway's
edge-client certificate. Caddy trusts the gateway front certificate at
`/etc/caddy/socialweb-computer/gateway.crt`, the kcp serving certificate at
`/etc/caddy/socialweb-computer/kcp.crt`, and holds the edge client certificate at
`/etc/caddy/socialweb-computer/edge-client.{crt,key}`.

`scripts/deploy.sh` installs the binary, runs `install gateway`, copies those four Caddy files from
the new state, writes the sites, validates the Caddyfile and reloads Caddy. A fresh install that
regenerates state must run this certificate handoff; otherwise Caddy keeps the previous gateway and
edge-client certificates and the kcp front returns `bad certificate`.

The web root returns 404 because this tree is API-only. There is no SPA at `/`. OAuth is started by
`POST /oauth/start` with `{"handle":"..."}`; the response is an authorization URL. The browser flow
ends at `/oauth/callback`, which sets the `swc_session` cookie on `k8s.socialweb.computer`.

## kcp layout

Observed root workspaces:

| workspace | type | role | phase |
| --- | --- | --- | --- |
| `socialweb-provider` | `universal` | schemas, exports, provider service accounts | Ready |
| `app` | `socialweb-application` | gateway application objects and components | Ready |
| `2wjeqtgx` | `account` | Alice's account workspace (`did:plc:lpfuqerea3deuoyrn7ojser4`) | Ready |

The root workspace namespaces are `default` and `kcp-system`; the `app` logical cluster also has
`default` and `kube-system`. The account workspace is the tenant boundary for Alice's clusters and
contains the `clusters` child mount pointing at her cluster workspace.

Provider exports in `root:socialweb-provider`: `applications`, `atprotoidentities`, `clusters`,
`computecontracts`, `computevms`, `events`, `exposures`, `hostprocesses`, `marketidentities`, `pds`
and `policy`. The root bindings include `computecontracts` and `computevms` in addition to the
default application set. `socialweb-application` binds `computevms`, so the gateway set applies the
`computevms` resource schema, export and root binding before it creates `root:app`.

Main objects:

| kind/name | phase | note |
| --- | --- | --- |
| `Gateway/gateway` | Ready | profile `prod`, DID `did:plc:5ipkeflamx7tz2venxxgjupl` |
| `OpenBaoServer/root-vault` | Ready | root vault |
| `GatewayAPI/gateway-api` | Ready | `/api`, `/session`, `/oauth`, `/xrpc`, downloads |
| `Pds/pds` | Ready | app-owned PDS |
| `Requester/requester` | Ready | market requester |
| `PolicyEngine/policy-engine` | Running | in-process engine |

Services and exposures:

| Service | target | Exposure |
| --- | --- | --- |
| `svc-gatewayapi-gateway-api` | `GatewayAPI/gateway-api` | local `127.0.0.1:20002`; ingress through `IngressProxy/ingress` |
| `svc-pds-pds` | `Pds/pds` | local `127.0.0.1:20000`; ingress through `IngressProxy/ingress` |
| `svc-requester-requester` | `Requester/requester` | local `127.0.0.1:20001`; ingress through `IngressProxy/ingress` |

The prod profile defaults `spec.ingress` to `xrpc-subscription` with
`ingressProxyHost: xrpc.fedproxy.com` and the app `IngressProxy/ingress`, so the app PDS owns a
public URL. A gateway spec that sets `spec.ingress.opentunnel` selects `opentunnel` instead; a spec
that sets `spec.ingress` names its own provider. The `market-network` profile inherits the
dispatcher provider from `MarketNetworkValue.ingress`. Public `k8s` traffic still uses the local 8788
front, not an ingress Exposure.

## Gateway and requester identity

`Gateway/gateway` has `spec.profile: prod`, `spec.webAppFront: 127.0.0.1:8788`, and options:

```yaml
network:
  plcDirectoryUrl: https://plc.directory
  ingressProxyHost: xrpc.fedproxy.com
  qrOrigin: ""
ingress:
  provider: xrpc-subscription
  ingressRef: {kind: IngressProxy, name: ingress}
  xrpc:
    ingressProxyHost: xrpc.fedproxy.com
options:
  - {name: public-origin, value: https://k8s.socialweb.computer}
  - {name: gateway-audience, value: did:web:k8s.socialweb.computer}
  - {name: kcp-public-host, value: https://kcp.socialweb.computer}
  - {name: node-policy, value: tangled-vouch}
  - {name: node-policy-args, value: '{"firstFree":true}'}
```

The Gateway's app-owned PDS is created by appctl, its account session is stored in the root vault
at `identity/requester/pds-session`, and the requester reports its repo DID. The gateway DID above is
that requester repo DID, not a browser account DID.

Web sign-in uses the PDS as the OAuth authorization server, with the GatewayAPI as the OAuth client
and callback server. The browser session is a cookie held by the gateway; the PDS session is in the
root vault, not in the web app's memory. Alice's observed session:

- handle `aliceoa.bsky.social`
- DID `did:plc:lpfuqerea3deuoyrn7ojser4`
- `GET /session` returns the DID after the OAuth callback
- `POST /api/clusters/list` returns the account's clusters

## Cluster API and kubeconfig

`/api/clusters/*` is the HTTP surface used by the browser and by scripts. The same operations are
also available as gateway XRPC (`com.publicdomainrelay.temp.cluster.*`) on the same handler.

For a Ready running cluster, `/api/clusters/kubeconfig` returns a client-certificate kubeconfig whose
server is:

```
https://kcp.socialweb.computer/clusters/<account>/<cluster>
```

On the `prod` profile the kubeconfig has no `certificate-authority-data`; it uses the system trust
store and therefore trusts Caddy's public certificate. A gateway on a loopback or self-signed front
keeps the gateway's `tls/tls.crt` as the CA unless the `kcp-public-ca-file` option names another file.
The client certificate is issued by the cluster's client CA and is presented through Caddy's 8444
certificate front.

A live check before the smoke cluster was deleted:

```bash
curl -sS -X POST -H "Cookie: $COOKIE" -H 'Origin: https://k8s.socialweb.computer' \
  -H 'Content-Type: application/json' -d '{"name":"prod-smoke","ttlSeconds":3600}' \
  https://k8s.socialweb.computer/api/clusters/kubeconfig
jq -r .kubeconfig response.json > prod-smoke.kubeconfig.yaml
kubectl --kubeconfig prod-smoke.kubeconfig.yaml get namespaces
# default, kube-node-lease, kube-public, kube-system, users
```

## Deploy and upgrade

The installed layout is:

- account `socialweb` (uid 982), state `/var/lib/socialweb-computer/socialweb`
- binary slot `bin/current -> bin/30ab2e...`
- config `/var/lib/socialweb-computer/socialweb/config.json`
- environment `/var/lib/socialweb-computer/socialweb/gateway.env` (mode 0600, owned by socialweb)
- spec `/var/lib/socialweb-computer/gateway-spec-socialweb.yaml`
- Caddy sites under `/etc/caddy/sites`, certificates under `/etc/caddy/socialweb-computer`

A routine upgrade is:

```bash
# as socialweb
CONFIG_PATH_SOCIALWEB_COMPUTER=/var/lib/socialweb-computer/socialweb/config.json \
  /var/lib/socialweb-computer/socialweb/bin/current/socialweb-computer \
  upgrade --binary /var/tmp/swc-release/socialweb-computer-<sha> --sha256 <sha>
sudo /var/lib/socialweb-computer/socialweb/bin/current/socialweb-computer install gateway \
  --account socialweb --state-dir /var/lib/socialweb-computer/socialweb \
  --gateway-spec-file /var/lib/socialweb-computer/gateway-spec-socialweb.yaml \
  --cert-front-address 127.0.0.1:8444 --tls-front-address 127.0.0.1:8443 \
  --kcp-front-address 127.0.0.1:6443 --host-backend systemd-user
```

Then run the Caddy certificate/site handoff from `scripts/deploy.sh` (or the script itself). An
upgrade preserves the kcp store, root vault, requester PDS, DID, account workspaces and clusters.

A fresh install is required when the existing `app` workspace type is the old `denoruntime`: the
WorkspaceType name is immutable. A fresh install removes the old state and requires the Caddy
certificate handoff and a new Alice sign-in.

## Live state

- Gateway Ready: root vault, GatewayAPI, Pds, Requester and PolicyEngine all Ready.
- Alice sign-in: `did:plc:lpfuqerea3deuoyrn7ojser4`, `swc_session` cookie.
- Account workspace `2wjeqtgx` Ready; the smoke cluster was created Ready and then deleted, leaving
  no clusters.
- `kcp.socialweb.computer/healthz` 200; `k8s.socialweb.computer/api/config` 200; `/` 404 (API-only).
- `kcp-public-host` is `https://kcp.socialweb.computer`; kubeconfigs use the system CA on prod.

## Known gaps

- No static web UI. A browser sees 404 at `/`; sign-in is the JSON `/oauth/start` call.
- Ingress Exposures are Pending without an `IngressProxy`; the public path is the Caddy local front.
- A fresh install must refresh Caddy's gateway, edge-client and kcp CA files. `scripts/deploy.sh`
  does this; a bare `install gateway` does not.
- The generic `AtprotoRecord` reconciler is absent; gateway records declared as `AtprotoRecord`
  objects do not get applied by the Go tree.
- The `app` workspace type is immutable; an old `denoruntime` install cannot be upgraded in place.

## Inspect

```bash
K=$HOME/.tmp/prod-kubeconfig-new   # fetched from the host admin kubeconfig
kubectl --kubeconfig "$K" get workspaces,gateway,openbaoserver
kubectl --kubeconfig "$K" get gatewayapi,pds,requester,policyengine,service,exposure -o wide
kubectl --kubeconfig "$K" get apibinding -A
kubectl --kubeconfig "$K" get gateway gateway -o jsonpath='{.spec}{"\n"}{.status}{"\n"}'
```

On the host, `systemctl --user -M socialweb@ list-units` shows the core, kcp slots, Postgres, kine,
front and OpenBao units.
