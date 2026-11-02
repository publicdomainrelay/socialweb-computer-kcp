# Acceptance 0014: a workload mints an atirc session and scrobbles through it (plan 0040)

Proves a workload with an OIDC workload identity signs an atproto OAuth app in as an account on
our PDS with no password and no browser, through the `com.fedproxy.rbac` role the account wrote,
and that the app then writes into the account's repo through that session within its scopes.
How it works: [docs/pds/oauth-wid.md](../pds/oauth-wid.md).

- Plan [0040](../plans/0040-workload-oauth-local-pds.yaml) items S8, S9, S11
- Last pass: 2026-11-01, `TestLiveWorkloadMintsAnAtircSessionAndScrobblesThroughIt` PASS in 130 s
  (scrobble `com.vibescrobble.scrobble/3mzbvauwixf22` written through the session)

## Preconditions

| Item | Need |
| --- | --- |
| Network | https to xrpc.fedproxy.com (websocket), plc.directory and irc.fedfork.com |
| Keys | none on disk: the ingress key, the PLC rotation key and the repo key live in memory for the run |
| atirc | `ATIRC_BIN` names an atirc binary, or it is fetched from `https://irc.fedfork.com/dl/<arch>` |
| Go | the module's toolchain; nothing else is built |

## Steps

```bash
cd socialweb-computer-kcp
go test -count=1 ./test/integration/                      # the in-process cases first
SWC_LIVE_WORKLOAD_OAUTH=1 go test -count=1 -v -timeout 12m \
  -run TestLiveWorkloadMintsAnAtircSessionAndScrobblesThroughIt ./test/integration/
```

The live test:

1. starts our PDS in process behind an ephemeral `<did:key label>.xrpc.fedproxy.com` host and
   creates a did:plc on plc.directory whose handle is that host;
2. writes `com.fedproxy.rbac/atirc-acceptance`: an in-process OIDC issuer and subject, client
   `https://irc.fedfork.com/oauth/client-metadata*`, redirect `https://irc.fedfork.com/oauth/callback`,
   the scopes atirc's client metadata declares (without `transition:*`);
3. as the workload: `GET https://irc.fedfork.com/login?handle=<host>`, exchangeToken,
   approveAuthorization, `GET` the callback (atirc's session cookie);
4. turns the vibescrobble switch on (`POST /scrobble`), links the account (`POST /agent/link`), reads
   `/u/<uuid>` from `/` and `GET`s it for a long atirc token;
5. runs `atirc pub -c '#swc-wid-<hex>' '<emoji> socialweb-computer workload OAuth acceptance <hex>'`
   with `ATIRC_TOKEN`; where the token's iroh transport cannot connect (a container with no UDP) the
   same message goes to `https://irc.fedfork.com/xrpc/chat.atirc.post`, and the log says which;
6. waits for a `com.vibescrobble.scrobble/...` op in the account's repo on the PDS commit stream;
7. `GET /logout`, then closes the PDS, which tombstones the DID.

## Expected observations

- `PDS https://did-key-....xrpc.fedproxy.com DID did:plc:... handle did-key-....xrpc.fedproxy.com`
- `atirc session for did:plc:... through https://did-key-....xrpc.fedproxy.com`
- `atirc wrote com.vibescrobble.scrobble/<rkey> through its OAuth session`
- After the run, `https://plc.directory/<did>` answers 404 (tombstoned).

## Pass criteria

The test passes; every in-process case of `test/integration` passes.

## Pitfalls

- An authorization server without `transition:generic` in `scopes_supported` is refused by atirc's
  OAuth library before the PAR (502 "could not start login for that account").
- atirc signs in with a keyed client_id (`.../oauth/client-metadata/<kid>`); a role naming the
  plain client_id refuses the approval. Allow `https://irc.fedfork.com/oauth/client-metadata*`.
- The line is public and permanent on the vibescrobble board while the account exists.
