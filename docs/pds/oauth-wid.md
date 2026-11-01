# Workload identity OAuth on our PDS (oauth-wid)

A workload that holds an OIDC workload identity token (a JWT) can complete an atproto OAuth sign-in
of a third-party app as an account on our PDS, with no password and no browser. The account allows
it by writing a `com.fedproxy.rbac` record in its own repo. The workload exchanges its JWT at the
PDS for a short PDS token bound to that one record, and with that token approves the app's pending
authorization request, the step a person does on the PDS's authorize page. The app then holds an
ordinary atproto OAuth session for the account, limited to the scopes the record allows.

Only our PDS (`internal/atproto/pds`) implements this. The record rules, the two XRPCs and the
token shapes are written so another PDS could implement them; `docs/protocol/workload-oauth.md` is
the protocol note for that discussion. Plan: `docs/plans/0040-workload-oauth-local-pds.yaml`.

## The pieces

| Piece | Where |
| --- | --- |
| The record format, the matcher, the JSON Schema subset | `internal/contract/workloadauth`, `internal/workloadauth/rbac` |
| The OIDC token verifier (discovery, JWKS, signatures) | `internal/workloadauth/oidc` |
| `exchangeToken`, `approveAuthorization`, session checks | `internal/atproto/oauth/workload.go` |
| The PDS wiring (grant store, record reader, revocation on commit) | `internal/atproto/pds/workload.go` |
| OAuth scope enforcement on every PDS write | `internal/atproto/pds/auth.go`, `repo_handlers.go`, `blob.go`, `service_auth.go` |
| The workload side (record builder, exchange, approve, app sign-in) | `internal/atproto/workloadoauth` |
| CLI | `socialweb-computer oauth allow`, `oauth revoke`, `oauth approve` (`cmd/socialweb-computer/oauth.go`) |
| Requester rules | ComputeContract `spec.oauthClients`, MarketRequester and Requester `spec.oauthGrants` (`internal/market/requester/oauthclients.go`, `oauthgrants.go`) |
| Guest workload identity issuer | the sshsig OpenBao auth plugin, `wid/` paths (`cmd/openbao-plugin-auth-sshsig/wid.go`) |
| A PDS public through xrpc.fedproxy.com with no key on disk | Exposure `spec.ingressKey: Ephemeral`, Pds `spec.ephemeral`, `internal/stack/ephemeralpds` |
| Lexicons | `lexicons/com/fedproxy/rbac.json`, `lexicons/com/fedproxy/temp/oauth/{exchangeToken,approveAuthorization}.json` |
| Tests | `test/integration/workloadoauth_test.go` (in process), `workloadoauth_live_test.go` (live) |

## Sequence

```
workload            app (irc.fedfork.com)            our PDS                    OIDC issuer
   | GET /login?handle=<h> ->|                            |                           |
   |                         | resolve h -> DID -> PDS    |                           |
   |                         | POST /oauth/par (DPoP) --->|                           |
   |<- 302 /oauth/authorize?client_id&request_uri --------|                           |
   | (no browser: the workload reads the Location)        |                           |
   | token for aud <did>#role/<rkey> ------------------------------------------------>|
   |<-------------------------------------------------------------------------- JWT --|
   | POST exchangeToken {subject_token, audience} (DPoP) ->|                          |
   |                         |   getRecord(did, com.fedproxy.rbac, rkey)              |
   |                         |   discovery + JWKS of the role's iss ----------------->|
   |<---------------- wid+jwt (5 min, DPoP bound) --------|                           |
   | POST approveAuthorization {request_uri} (DPoP wid+jwt)>|                         |
   |                         |   policy on {client_id, redirect_uri, scope} of the PAR |
   |<--------------- {redirect_uri: callback?code&state&iss}                          |
   | GET callback (app cookies) ->|                       |                           |
   |                         | POST /oauth/token (code, DPoP) ->|                     |
   |<- app session cookie ---|<--------- access + refresh tokens|                     |
   |                         | createRecord ... (DPoP, scoped) ->|                    |
```

## The record

`at://<account DID>/com.fedproxy.rbac/<role>`. The rkey is the role name, so every lookup is one
`getRecord`; the PDS never lists the collection and never polls.

```json
{
  "$type": "com.fedproxy.rbac",
  "protects": {"vm1": {"service": "<PDS service DID>", "scope": "com.fedproxy.temp.oauth"}},
  "roles": {"vm1": {"role_name": "vm1", "definition": {
    "iss": "https://<bidder ingress>/v1/auth/sshsig/wid",
    "aud": "<account DID>#role/vm1",
    "sub": "actx:<bidder>:plc:<requester>:role:vm1",
    "policies": ["vm1"]}}},
  "policies": {"vm1": {"meta": {"policy": "vm1"}, "schemas": {
    "/xrpc/com.fedproxy.temp.oauth.exchangeToken": {
      "type": "object", "required": ["capability"],
      "properties": {"capability": {"enum": ["create"]}}},
    "/xrpc/com.fedproxy.temp.oauth.approveAuthorization": {
      "type": "object", "required": ["capability", "body"],
      "properties": {"capability": {"enum": ["create"]}, "body": {
        "type": "object", "required": ["client_id", "scope"], "additionalProperties": false,
        "properties": {
          "client_id": {"type": "string", "pattern": "^https://irc\\.fedfork\\.com/oauth/client-metadata[^?#*]*$"},
          "redirect_uri": {"const": "https://irc.fedfork.com/oauth/callback"},
          "scope": {"type": "array", "items": {"enum": ["atproto", "repo:com.vibescrobble.scrobble?action=create"]}}}}}}}}},
  "createdAt": "2026-11-01T00:00:00Z"
}
```

The format is the one the reference implementation (compute-contract-reference-implementation-poc,
`lib/rbac-helper`) and fedproxy use; nothing was added. What our PDS requires of it:

- Exactly one role, named as the rkey, with `role_name` equal to it, and `iss`, `aud`, `sub` and at
  least one policy that the record holds. Any other shape admits nothing.
- `protects` names this PDS's service DID (its `describeServer` `did`) and scope
  `com.fedproxy.temp.oauth`, exactly; no `*`.
- A path schema is matched as in the reference: an exact path first, else the longest `*` glob. The
  HTTP method maps to a capability (GET read, POST create, PUT/PATCH update, DELETE delete).
- The `body` schema is evaluated (the reference never did). The body is a document the PDS builds
  from its own state, never the caller's JSON: for approveAuthorization it is
  `{client_id, redirect_uri, scope: [...]}` of the stored PAR request.
- The JSON Schema subset: `type`, `const`, `enum`, `items`, `required`, `properties`,
  `additionalProperties`, `pattern` (Go RE2), `minItems`, `maxItems`, plus `$schema`, `title`,
  `description`. Any other keyword makes the policy refuse.
- Expiry is deleting the record. A commit that changes or deletes it revokes the sessions it
  admitted.

`workloadoauth.Record` (and `socialweb-computer oauth allow --oauth-print`) builds exactly this. A
client_id with `*` becomes a `pattern`; atirc signs in with a keyed client_id
(`https://irc.fedfork.com/oauth/client-metadata/<kid>`), so allow it as
`https://irc.fedfork.com/oauth/client-metadata*`. A scope `transition:*`, `repo:*` or
`repo:com.fedproxy.rbac...` is refused: a workload never approves full access and never lets an app
grant further workloads.

## Workload identity tokens

Any OIDC issuer works: the role's `iss` must serve `/.well-known/openid-configuration` whose
`issuer` equals it and whose `jwks_uri` names the keys (RS256/384/512, PS256/384/512, ES256,
ES384, EdDSA). The PDS fetches them only for an issuer a role of the target account names, only
over https to a public address (`ssrf.PublicOnlyTransport`), with a 1 MiB cap, and caches the keys
for 5 minutes (refetched at most every 10 s on an unknown kid). A token must carry `iss`, `sub`,
`aud` (the role's), `exp` and `iat`, live at most an hour, and is exchanged at most once (its `jti`).
`PdsOAuth.workloadIssuers` narrows the issuers a PDS accepts further; `disableWorkloadSignIn`
turns the XRPCs off.

The guest's own issuer is the bidder's OpenBao sshsig auth plugin (plan 0040 D1): the guest proves
its ssh host key and gets a JWT for any one audience, with no OpenBao token:

```
/usr/local/bin/guest-login.sh token '<account DID>#role/<rkey>'
```

`iss` is `<bidder ingress>/v1/auth/sshsig/wid`, `sub` is
`actx:<bidder>:plc:<requester>:role:<vm role>`, life 5 minutes. The host key is root's, so a
guest workload token is root's.

## The XRPCs

`POST /xrpc/com.fedproxy.temp.oauth.exchangeToken` with a DPoP proof (RFC 9449; the first call
answers `use_dpop_nonce` with a `DPoP-Nonce` header):

```json
{"subject_token": "<JWT>", "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
 "audience": "<account DID>#role/<rkey>", "requested_token_type": "urn:ietf:params:oauth:token-type:access_token"}
```

The account must be active on this PDS; the record at the rkey must protect it, name the token's
`iss`, `aud` and `sub`, and allow capability create on the exchangeToken path. The answer
`{access_token, issued_token_type, token_type: "DPoP", expires_in}` is a `wid+jwt`: `iss` the PDS
service DID, `sub` the account DID, `aud` the requested audience, `lxm`
`com.fedproxy.temp.oauth.approveAuthorization`, `act {iss, sub}` of the workload, `cnf.jkt` of the
DPoP key, at most 5 minutes and never past the subject token.

`POST /xrpc/com.fedproxy.temp.oauth.approveAuthorization` with `Authorization: DPoP <wid+jwt>`,
its DPoP proof (with `ath`) and `{"request_uri": "urn:ietf:params:oauth:request_uri:..."}`. The
token is single use. The PDS reads the stored PAR request, checks the role's approveAuthorization
policy against `{client_id, redirect_uri, scope}`, and only then consumes the request and issues
the code. A refusal leaves the request pending. The answer is `{"redirect_uri": "<callback>?code=...&state=...&iss=..."}`,
the same redirect the authorize page makes.

Errors: 400 `InvalidRequest`, 401 `InvalidToken` / `AuthenticationRequired` (bad, reused or
mis-bound token), 403 `Forbidden` with the policy's reason. The PDS logs `pds_workload_exchanged`,
`pds_workload_approved`, `pds_workload_exchange_refused`, `pds_workload_approve_refused`,
`pds_workload_session_refused`, `pds_workload_session_revoked`; never a token.

## The session

The approval stores `{session, account, role, iss, sub, aud, client_id, redirect_uri, scope}`
(`pds_oauth_workload_grants`). Every refresh re-reads the record and re-runs the approve policy;
a commit to `com.fedproxy.rbac/<rkey>` (update or delete) re-checks the role's live sessions and
revokes those no longer allowed, so the app's next request answers 401 and its refresh
`invalid_grant`.

Every OAuth session on our PDS, workload or not, is limited to its granted scopes (plan 0040 S1):
`createRecord`, `putRecord` (create and update), `deleteRecord` and each `applyWrites` op need
`repo:<collection>?action=...`, `uploadBlob` a `blob:` scope matching the mime type,
`getServiceAuth` `rpc:<lxm>?aud=<aud>`; `transition:generic` allows all of these. A miss is 403
`ScopeMissingError`. App passwords are managed only from a password session.

## Flow 2: the account that made the VM request

The requester's account must be on our PDS (identity mode localRepo, for example
`run request --identity <name>`); bsky.social cannot run these XRPCs.

```
socialweb-computer run request --name r1 --identity me --hold --secrets ... \
  --allow-oauth 'https://irc.fedfork.com/oauth/client-metadata*=atproto,repo:com.vibescrobble.scrobble?action=create'
```

`--allow-oauth` (repeatable) sets ComputeContract `spec.oauthClients [{clientId, scopes,
redirectUri}]`. Once the winner is known the requester writes the record at rkey `oauth-<vm role>`
in its own repo with `iss` = the winning bid's issuer and `sub` = the guest's subject, puts
`/etc/socialweb-computer/oauth-wid.json {account, pds, role, audience, clients}` into the guest's
secrets bundle (root 0600), reports `status.contract.oauthClients`, and deletes the record at
release or on failure. In the guest, as root:

```
socialweb-computer oauth approve --oauth-login https://irc.fedfork.com/login --oauth-jar /root/atirc.cookies
```

(`--oauth-account` and `--oauth-role` default from `oauth-wid.json`; the token comes from
`guest-login.sh token`, or `--oauth-token-command` / `--oauth-token-file`.) The cookie jar then
holds the app's session for the requester's account.

## Flow 1: the same account inside the guest

A guest that runs its own requester and PDS (R2, plan 0036) can let another process in the same
guest sign apps in as R2's account. The requester writes the record from MarketRequester and
Requester `spec.oauthGrants [{role, issuer, subject, clients}]` in its own repo (roles written are
kept in `<data dir>/oauth-grants.json`; a role that leaves the list is deleted):

```
socialweb-computer run request --name r2 --guest-requester -- \
  --oauth-self 'https://irc.fedfork.com/oauth/client-metadata*'
```

`--oauth-self` reads this guest's own `iss` and `sub` from `guest-login.sh token` and grants role
`oauth-self`; `--oauth-grants-file <json>` takes any list. Then, as root in the guest:
`socialweb-computer oauth approve --oauth-account <R2 DID> --oauth-role oauth-self --oauth-login https://irc.fedfork.com/login`.
Outside the market, `socialweb-computer oauth allow --oauth-pds <url> --oauth-account <did>
--oauth-role <rkey> --oauth-issuer <iss> --oauth-subject <sub> --oauth-client <client_id>` writes
the record with the account's password in `ATPROTO_PASSWORD`, and `oauth revoke` deletes it.

## A PDS public through xrpc.fedproxy.com with no key on disk

The app must reach the PDS: resolve the handle, read the DID document on plc.directory, fetch the
OAuth metadata, PAR and exchange the code. A PDS behind the did:key ingress relay is public at
`https://<did:key label>.xrpc.fedproxy.com`. With an ephemeral ingress key the label comes from a
key held only in memory, so the URL is known only at run time:

- `run request ... -- --ingress-ephemeral-key` (Requester `spec.ingress.xrpc.ephemeralKey`) composes
  the app Pds with `spec.ephemeral`: its keys stay in memory, its handle is the run-time host (the
  PDS answers `/.well-known/atproto-did` for it through the ingress), its account is created only
  once the URL is known, and it tombstones the DIDs it made when it stops. A restart is a new host
  and a new account.
- `internal/stack/ephemeralpds.Start` does the same in process (the live test uses it).

## Example: a workload mints an atirc session and scrobbles

`TestLiveWorkloadMintsAnAtircSessionAndScrobblesThroughIt` (`SWC_LIVE_WORKLOAD_OAUTH=1 go test
-run TestLive ./test/integration/`) runs this end to end; read 2026-11-01, PASS in 130 s:

1. Start our PDS behind an ephemeral `xrpc.fedproxy.com` host; create a did:plc on plc.directory.
2. Write the record above (role `atirc-acceptance`, the atirc client glob, its metadata scopes).
3. `GET https://irc.fedfork.com/login?handle=<host>` answers 302 to our authorize URL.
4. exchangeToken, approveAuthorization, then `GET` the callback: atirc sets its session cookie.
5. `POST /scrobble did=<did> on=1` (the vibescrobble switch), `POST /agent/link did=<did>`, read
   `/u/<uuid>` from `/`, `GET /u/<uuid>`: a long atirc token.
6. `ATIRC_TOKEN=<token> atirc pub -c '#<channel>' '<text whose first character is an emoji>'`
   (where the token's iroh transport cannot connect, the test posts the same message to
   `chat.atirc.post` over https).
7. atirc's scrobble writer does `createRecord com.vibescrobble.scrobble` in the account's repo on
   our PDS through the OAuth session (scope `repo:com.vibescrobble.scrobble?action=create`); the
   test sees it on the PDS's commit stream (`com.vibescrobble.scrobble/3mzbvauwixf22` in that run).
8. `GET /logout`; the PDS tombstones the DID.

Interop found on the way: atproto-oauth (atirc's client library) refuses an authorization server
whose `scopes_supported` lacks `transition:generic` (our PDS now lists `atproto` and
`transition:generic`); indigo re-registers jwt's `ES256` for atproto keys, so the OIDC verifier
checks signatures itself.

## Tests

- `go test ./test/integration/` (in `deploy/check.sh`; in process, no network): sign-in and a
  scoped write, the scope refusal, another subject or issuer, no record, a scope outside the record
  (the request stays pending), single-use and key-bound tokens, revocation on record delete.
- `SWC_LIVE_WORKLOAD_OAUTH=1 go test -run TestLive ./test/integration/`: the atirc acceptance above
  (`ATIRC_BIN` names an atirc binary; else it is fetched from `https://irc.fedfork.com/dl/<arch>`).
  Acceptance doc: `docs/acceptance/0014-workload-oauth.md`.

## Security notes

- The PDS trusts an issuer only because the account's own record names it; it fetches nothing for
  an account it does not host. A bidder can sign in only to roles that name its issuer and its
  guest's subject.
- Workload tokens may carry any audience (D2); every relying party checks `iss`, `sub` and `aud`.
- The workload approves only what the record's body schema allows, evaluated on the PDS's stored
  PAR, so a caller cannot widen the client, redirect or scope.
- Exchanged tokens are single use, 5 minutes, DPoP bound and limited by `lxm` to the approve call.

## The browser page

`spec.oauthClients` is part of the contract template, so Infra, EphemeralInfra and the browser page
(which requests VMs through an EphemeralInfra template) carry it. The page's "Allow OAuth apps"
field (Compute block, one app per line: client_id, then scopes) lives in the page repository
(johnandersen777/kcp, branch kcp-wasm-memory-storage-1); the change is
[browser-page-oauth-clients.patch](browser-page-oauth-clients.patch) (`git am` it there). The page's
own identity is oauthBrowser today, so its requests with apps listed are refused until the page
signs in with an account on a socialweb-computer PDS. The iOS app is future work.
