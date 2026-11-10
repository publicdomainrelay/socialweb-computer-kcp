# fedproxy.com: what runs there and what depends on it

One DigitalOcean droplet (157.230.140.76). mush.at, fedproxy.com, *.fedproxy.com and irc.fedfork.com
all resolve to it. Read live on 2026-11-10 over `ssh root@fedproxy.com`; the relay was at 39d519ea1747.
Other hosts: [inventory.md](inventory.md). How to deploy: [infra/README.md](../../infra/README.md).

## Summary

- Caddy (xcaddy v2.11.7 with the Cloudflare DNS module) is the only public listener besides sshd
  and atprp's port 2222. Ports 80 and 443 belong to one server, `srv0`. Its route list is built by
  three writers: the Caddyfile, atprp-ssh-relay and the Go relay.
- atprp-ssh-relay (port 2222) turns an `ssh -R <svc>:80:...` from an atproto handle into
  `<svc>--<handle-dashed>.fedproxy.com`. It does this by adding Caddy routes over the admin socket.
  Every 30 s it adds back any that are missing.
- The xrpc ingress relay (Go, `xrpc-relay-server.service`) serves `xrpc.fedproxy.com`,
  `did-key-*.xrpc.fedproxy.com` and the custom domains (mush.at). On 127.0.0.1:5556 it answers
  Caddy's on_demand ask, the check Caddy runs before it obtains a certificate for a new name.
- rp.fedproxy.com is oauth-client-webapp. irc.fedfork.com is atirc. fedproxy.com is a static site
  in `/var/www/html`.
- A Caddy restart drops everything that was added over the admin API. atprp and the Go relay put
  their parts back. On 2026-11-10 everything was back 44 s after `systemctl restart caddy`.

## Processes, units, ports

| Unit | Binary | Listens | Serves | Deployed by |
| --- | --- | --- | --- | --- |
| `caddy.service` (`/usr/lib/systemd/system`, `Environment=CF_API_TOKEN`) | `/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile` | :80, :443, admin unix `/opt/caddy/caddy-admin.sock` | everything below | by hand; the Caddyfile is `infra/fedproxy-legacy/Caddyfile` (same as live) |
| `atprp-ssh-relay.service` (`CF_API_TOKEN`, `THIS_ENDPOINT=fedproxy.com`, `CADDY_SOCK`) | `/usr/bin/atprp-ssh-relay` (`~/src/atproto-reverse-proxy/src/golang/cmd/atprp-ssh-relay`) | :2222 (ssh), 127.0.0.1:6060 (pprof), one unix socket per tunnel `/tmp/ssh-fwd-*/tcp.sock` | `<svc>--<handle>.fedproxy.com`, `*.<svc>--<handle>.fedproxy.com` | by hand. Never run its `scripts/deploy.sh`: it prints CF_API_TOKEN |
| `xrpc-relay-server.service` + drop-in `30-go.conf` (over the deno `20-socialweb-computer-kcp.conf`) | `/opt/xrpc-relay-go/current/socialweb-computer internal ingress-relay --unix-socket /opt/caddy/xrpc-relay-server.sock --hostname xrpc.fedproxy.com --ingress-relay-ask-listen 127.0.0.1:5556 --ingress-relay-caddy-admin-socket /opt/caddy/caddy-admin.sock --ingress-relay-caddy-custom-domains` | unix `/opt/caddy/xrpc-relay-server.sock`, 127.0.0.1:5556 (ask) | xrpc, did-key hosts, custom domains | `scripts/deploy-xrpc-relay.sh` (`infra/ingress-relay/deploy.sh` wraps it) |
| `oauth-client-webapp.service` | `/usr/bin/oauth-client-webapp` (atproto-reverse-proxy) | unix `/opt/caddy/web-ui.sock`, sqlite `/opt/caddy/oauth-client-webapp.db` | rp.fedproxy.com | by hand |
| `atirc.service`, socket-activated by `atirc-irc.socket` (127.0.0.1:6667), `atirc-xrpc.socket` (127.0.0.1:8080), `atirc-ops.socket` (127.0.0.1:6670); `atirc-iroh.service` | `/usr/local/bin/atirc serve ...` / `atirc iroh-proxy ...` (atirc 0.1.0) | those three, plus iroh | irc.fedfork.com | the atirc repo's CONTRIBUTING (`atirc server --install` / `--upgrade`) |
| `caddy-check-dns-from-config.service` (disabled) | `/usr/bin/caddy-check-dns-from-config` | 127.0.0.1:5555 when started | the old ask, used before the Go relay | started only by `scripts/deploy-xrpc-relay.sh --rollback` |
| static files | `/var/www/html` (`index.html`, `main.js`, `oauth-client-metadata.json`) | - | https://fedproxy.com/ | `atproto-reverse-proxy/scripts/deploy.sh` (`src/javascript/fedproxy-ssh-public-keys`), never run. Nothing current publishes it |

Dead, but still configured: the `xrpc-test.fedproxy.com` and `*.xrpc-test` blocks point at a socket
nothing serves, so they answer 502. `/opt/xrpc-relay` (pre-iroh) is left over.

## How Caddy routes a request

Caddy goes through `srv0`'s routes in order and stops at the first terminal match. If no route
matches, Caddy answers **200 with an empty body** (`Server: Caddy`, `Content-Length: 0`). That is not
an error page. On :80, Caddy answers the ACME HTTP-01 challenges and redirects everything else to
https with a 308.

| # | Match | Handler | Written by |
| --- | --- | --- | --- |
| 0 | `xrpc-test.fedproxy.com` | proxy to the missing test socket (502) | Caddyfile |
| 1 | `xrpc.fedproxy.com` | proxy to the relay socket | Caddyfile |
| 2 | `rp.fedproxy.com` | proxy to `/opt/caddy/web-ui.sock` | Caddyfile |
| 3 | `irc.fedfork.com` | proxy to 127.0.0.1:8080 (atirc) | Caddyfile |
| 4 | `fedproxy.com` | file_server `/var/www/html` | Caddyfile |
| 5 | `*.xrpc-test.fedproxy.com` | 502 | Caddyfile |
| 6 | `*.xrpc.fedproxy.com` (one label: `did-key-<multibase>`) | proxy to the relay socket | Caddyfile |
| 7.. | `route-<svc>--<handle>.fedproxy.com`, one per live tunnel | proxy to `/tmp/ssh-fwd-*/tcp.sock` | atprp (`@id route-<fqdn>`) |
| .. | `route-wildcard-catchall-fedproxy.com`: `*.fedproxy.com` | 404 `no route configured for host` | atprp; moved back to the end whenever it adds a tunnel |
| any | `route-xrpc-custom-domains`: NOT (`fedproxy.com`, `*.fedproxy.com`, `*.*.fedproxy.com`, `*.*.*.fedproxy.com`) | proxy to the relay socket | the Go relay |

The custom-domain route matches no fedproxy.com name, so its position in the list does not matter.
A Caddyfile site block without a host would instead sit before atprp's appended routes, and it would
take every tunnel. Host classes:

| Host | Route | Answer |
| --- | --- | --- |
| `fedproxy.com` | 4 | the static site |
| `rp.`, `xrpc.`, `xrpc-test.fedproxy.com`, `irc.fedfork.com` | 0-3 | their upstream |
| `did-key-*.xrpc.fedproxy.com` | 6 | the relay: the subscriber of that did:key, or the relay's 404 |
| `<svc>--<handle>.fedproxy.com` with a live tunnel | 7.. | the tunnel (502 when the far end has nothing listening, e.g. bob-bid-0001 while `bidder.service` on mini-cloud-0001 is inactive) |
| any other single label under `fedproxy.com` | catch-all | 404 |
| deeper names under fedproxy.com that are not `*.xrpc` or a tunnel (`a.b.fedproxy.com`) | none | TLS fails: no certificate covers them and the ask refuses them |
| a name outside fedproxy.com (mush.at) | custom-domain | the relay: the live subscriber that its `_xrpc.<host>` TXT names, else 404 |
| the same name while the custom-domain route is gone | none | **the empty 200**, served with the certificate Caddy already holds (see below) |
| a name outside fedproxy.com with no certificate and no TXT | - | TLS fails (the ask says 403) |

## Certificates

| Names | Issuer | How |
| --- | --- | --- |
| `*.fedproxy.com` | Let's Encrypt, DNS-01 (Cloudflare) | atprp's `tls-policy-wildcard-fedproxy.com` policy (no subjects: the default) and `apps/tls/certificates/automate`. The Caddyfile also loads the stored file as tag `cert0` for rp, xrpc and xrpc-test |
| `*.xrpc.fedproxy.com`, `*.xrpc-test.fedproxy.com` | DNS-01 (Cloudflare) | the Caddyfile `tls { dns cloudflare }` blocks |
| `fedproxy.com`, `irc.fedfork.com` | ACME HTTP-01 / TLS-ALPN-01 | the Caddyfile's own sites |
| `*.<svc>--<handle>.fedproxy.com` (a `*.x` tunnel) | on demand, DNS-01 | atprp's policy has `on_demand: true`; the ask decides |
| custom domains (mush.at) | on demand: DNS-01 is tried first and fails fast (not in the Cloudflare zone), then the **fallback** plain ACME issuer (HTTP-01 / TLS-ALPN-01) | the Go relay appends that fallback issuer to atprp's policy |

The on_demand ask is `http://127.0.0.1:5556/check` (in the Caddyfile, so it survives a restart). The
Go relay approves a name when any srv0 route has that host, either exactly or as a single-label
`*.suffix` (the same rule as the old 5555 checker; it reads the routes over the admin socket). It
also approves a name whose `_xrpc.<name>` TXT names a live subscriber. Certificates live in
`/root/.local/share/caddy/certificates/` and stay there after their route is gone.

## Dynamic config and Caddy restarts

| Part | Owner | Put back |
| --- | --- | --- |
| `tls-policy-wildcard-fedproxy.com` (DNS-01, on_demand), `*.fedproxy.com` in `certificates/automate` | atprp `ensureWildcardTLSPolicy` | every 30 s, written only if missing |
| tunnel routes, the catch-all | atprp `reconcileLoop` (`ensureForwardRoute`, `ensureCatchAllRoute`) | every 30 s, from its live SSH sessions; GETs only when nothing is missing |
| `route-xrpc-custom-domains`, the fallback issuer | the Go relay `--ingress-relay-caddy-custom-domains` (`internal/atproto/ingress/customdomains.go`) | at start and every 30 s, written only if missing; it waits for atprp's policy before adding the issuer |
| the ask endpoint | the Caddyfile | at every start |

Each config write reloads Caddy and cancels in-flight ACME orders. That is why both reconcilers write
only what is missing.

Measured on 2026-11-10 after `systemctl restart caddy` (06:59:35 UTC). Before the restart, the live
config was saved as a rollback snapshot that could be POSTed to `/load`; it was not needed.

- +12 s: the custom-domain route was back.
- +16 s: atprp had put back its policy, the wildcard automation, the three tunnel routes and the
  catch-all.
- +42 s: the fallback issuer was back. The relay's earlier tick had found no atprp policy yet.
- Between 0 and +16 s, the tunnels and mush.at answered the empty 200.
- After that, every probe below answered as before the restart. The mush.at subscriber's
  websocket reconnected by itself.

## Custom domains

- A subscriber is served on host H when `_xrpc.H` TXT holds its did:key (or its did-key host). This
  is checked per request and cached 60 s, negative answers included.
- A subscriber may also send `com.fedproxy.temp.xrpc.alias {hosts}`. Each host is checked against the
  same TXT, at most 16 per subscriber. A claimed host is exclusive, and the claim ends on disconnect.
  The relay answers `alias#result {accepted, rejected}`.
- A rejected host is still served through the TXT once the record appears. The atlibs-fedproxy
  ingress module re-sends its aliases every 60 s while any host is rejected, so its result catches up
  (atlibs-fedproxy 9dc1fe2).
- `infra/fedproxy-legacy/xrpc-custom-domains.sh status` prints the route, the issuers and the ask.
  `remove` deletes the route and the fallback issuer. While the Go relay runs with the flag, it adds
  them back within 30 s. `remove` sticks only after `scripts/deploy-xrpc-relay.sh --rollback` (which
  runs it once the deno relay serves) or a `CUSTOM_DOMAINS=0` deploy.

### Why mush.at answered 200 while the custom-domain route was removed

This was reproduced on 2026-11-10. With the route deleted, four requests to https://mush.at/ (and
`/xrpc/_health`) answered `HTTP/2 200`, `server: Caddy`, `content-length: 0`, with no `via` header.
With the route present, they answer 200 from the relay (`via: 1.1 Caddy`, the subscriber's 39-48
byte body). Nothing proxied the request: it matched none of srv0's routes, so Caddy wrote its
default empty 200. TLS still worked because the mush.at certificate (Let's Encrypt YE1, valid until
2027-02-08) was in Caddy's cache and storage, and removing the issuer does not evict a certificate.
A name with no certificate (`example.invalid`) failed the TLS handshake instead, because the ask
refused it. `apply` brought the relay's answer back.

## Deploy and rollback

| Component | Deploy | Rollback |
| --- | --- | --- |
| xrpc relay | `scripts/deploy-xrpc-relay.sh`, or `infra/ingress-relay/deploy.sh --apply`. It builds linux/amd64 from HEAD and ships to `/opt/xrpc-relay-go/<version>`, checking the sha256. It flips `current`, writes `30-go.conf` (with the flag unless `CUSTOM_DOMAINS=0`) and restarts. It checks that `_health` carries the commit and that `did.json` answers 200, and on a failed check it rolls back. Last, it points the ask at 5556, live and in the Caddyfile, and prints `xrpc-custom-domains.sh status` | `--rollback`, in this order: 1) `caddy-check-dns-from-config` up on 5555; 2) ask -> 5555 (Caddyfile backup `.bak-<ts>-pre-5555`); 3) remove `30-go.conf` and restart (the deno relay); 4) `xrpc-custom-domains.sh remove`. Back to a previous Go version: point `/opt/xrpc-relay-go/current` at `$(cat /opt/xrpc-relay-go/previous)` and `systemctl restart xrpc-relay-server` |
| Caddy | by hand. Change config through the admin API only; a Caddyfile reload or restart drops the API-added parts until the reconcilers put them back | POST a saved `/config/` snapshot to `/load` (the snapshot holds CF_API_TOKEN: keep it mode 600 and pipe it over ssh, never print it) |
| atprp, oauth-client-webapp, the checker, the static site | `infra/fedproxy-legacy` is a stub: `--status` only | - |
| atirc | the atirc repo | the atirc repo |

The default ssh key of these scripts is `~/.ssh/id_ed25519_no_password`, else `~/.ssh/id_ed25519`.

## What depends on fedproxy.com

| Consumer | Uses | Fails how when fedproxy is down | Check |
| --- | --- | --- | --- |
| atlibs-fedproxy `modules/ingress` | did:key service-auth (aud `did:web:xrpc.fedproxy.com`), `getRegistrationNonce`, one kept-open `subscribe` websocket, `#request` frames, alias frames | registration fails; after 3 reconnects (2, 4, 8 s) the run ends | `cargo wasm && node --test modules/*/test.mjs` (relay faked) |
| atlibs-fedproxy `modules/fedproxy`, `web/fedproxy-ssh-public-keys` | `com.fedproxy.sshPublicKey` records on the user's PDS; OAuth client `https://fedproxy.com/oauth-client-metadata.json` (the static site) | the key page cannot sign in | `curl -s https://fedproxy.com/oauth-client-metadata.json` |
| atlibs-examples-spa-networking `marque-fedproxy-ingress` (the owner's tab on http://127.0.0.1:8765/) | the ingress module in forward mode with `aliases: mush.at`; Marque writes the CNAME and the `_xrpc.mush.at` TXT | https://mush.at/ and its did-key host are dead | `dig +short TXT _xrpc.mush.at`; `curl -sI https://mush.at/` (`via: 1.1 Caddy` = relayed) |
| atlibs-agent-reference-spa | Publish: the ingress module serves `https://did-key-<key>.xrpc.fedproxy.com/mcp/<server>` | published MCP URLs are dead; the in-tab agent keeps working | `claude mcp add --transport http x https://did-key-<key>.xrpc.fedproxy.com/mcp/marque` |
| market registry and bidder (poc, mini-cloud-0001) | `market-registry-tunnel.service`, `bidder-tunnel.service`: `ssh -NnT -p 2222 ... -R market-registry-0001:80:127.0.0.1:4031 johnandersen777.bsky.social@fedproxy.com` (and `bob-bid-0001` -> 4021), `Restart=always`; `discovery.ts` pins `did:web:market-registry-0001--johnandersen777-bsky-social.fedproxy.com#pdr_temp_market`; the registry also publishes did-key relay endpoints (`DISPATCHER_HOST=xrpc.fedproxy.com`) | RFP and bid discovery fail; the ssh units retry every 5 s | `curl -s https://market-registry-0001--johnandersen777-bsky-social.fedproxy.com/.well-known/did.json` |
| `atlibs--johnandersen777-bsky-social` tunnel | an ad-hoc `ssh -R atlibs:80:...` from a dev session | that page | `curl -sI https://atlibs--johnandersen777-bsky-social.fedproxy.com/` |
| socialweb-computer (this tree) | `xrpc.fedproxy.com` is the default ingress (`internal/app/compose/exposures.go`, `internal/stack/bidder`, `internal/stack/gatewaykcp/env.go`, `run requester`) | exposures and bidder ingress have no public URL | `scripts/deploy-xrpc-relay.sh --status` |
| market requester atirc agents | `did:web:irc.fedfork.com#atirc` mint/revoke (`internal/market/requester/atirc.go`) | no atirc agent VM leases | `curl -A curl -sI https://irc.fedfork.com/chat` |
| the fedproxy skill (`~/.claude/skills/fedproxy`) | documents the tunnel: port 2222, `<svc>--<handle>`, the PDS key record | - | - |

## Verify

```bash
scripts/deploy-xrpc-relay.sh --status                    # relay unit, version, health, journal
infra/fedproxy-legacy/xrpc-custom-domains.sh status      # route 200, issuers [cloudflare, null], ask 5556
infra/hosts/fedproxy-com.sh --status                     # every component, read-only
for u in https://fedproxy.com/ https://rp.fedproxy.com/ https://xrpc.fedproxy.com/xrpc/_health \
  https://irc.fedfork.com/chat https://mush.at/ \
  https://market-registry-0001--johnandersen777-bsky-social.fedproxy.com/.well-known/did.json \
  https://nosuch--nobody.fedproxy.com/; do
  printf '%s %s\n' "$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$u")" "$u"
done                                                     # 200 401 200 200 200 200 404 (rp: 401 when signed out)
ssh root@fedproxy.com 'curl -s --unix-socket /opt/caddy/caddy-admin.sock http://127.0.0.1/config/apps/http/servers/srv0/routes |
  jq -r ".[] | .[\"@id\"] // .match[0].host[0]"'      # the route order above
ssh root@fedproxy.com 'journalctl -u xrpc-relay-server -o cat | grep ingress_caddy_custom_domains'  # adds after a Caddy restart
ssh root@fedproxy.com 'journalctl -u atprp-ssh-relay -o cat | grep reconciled'                      # atprp's re-adds
```

Never print `/config/` whole: atprp's policy carries the Cloudflare token. Select fields with jq.

## Open

- atirc, the static site, atprp and oauth-client-webapp have no `infra/` deploy; their sources are
  the atirc repo and `~/src/atproto-reverse-proxy`.
- `xrpc-test.fedproxy.com` and `/opt/xrpc-relay` are dead and still configured.
- Nothing prunes `/opt/xrpc-relay-go/<version>`.
