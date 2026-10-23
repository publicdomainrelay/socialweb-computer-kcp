# OAuth sign-in, association and its failures

The long form of the README sign-in section: what the script drives, every option, the per-mode notes and how to recover. Short path in [the README](../README.md#sign-in-with-oauth).

`scripts/oauth-signin.ts` drives a headless `/usr/bin/google-chrome` (puppeteer-core) through the flow: the page, the PDS authorize page, the password, then "Authorize". The script reads the handle and password from the credential file itself, so they never appear on the command line or in a log. It writes one screenshot per step to `~/.tmp/oauth-signin/<ts>/` (mode 0700).

The credential file (mode 0600) exports `<ACCOUNT>_ATPROTO_HANDLE` and `<ACCOUNT>_ATPROTO_PASSWORD`.

```bash
# the gateway user (alice) signs in to the API-only web app; do this after every fresh gateway deploy
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.aliceoa_bsky_social_source --account ALICE \
  --url https://k8s.socialweb.computer/ --start-api
# an oauthBrowser identity reports a visitUrl in status.pendingAction
U=$(kubectl --kubeconfig $K get marketidentity <name> -o jsonpath='{.status.pendingAction.visitUrl}')
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.aliceoa_bsky_social_source --account ALICE --url "$U"
# an oauthQr identity reports a scan url in status.pendingAction
U=$(kubectl --kubeconfig $K get marketidentity <name> -o jsonpath='{.status.pendingAction.url}')
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.aliceoa_bsky_social_source --account ALICE --url "$U"
```

The gateway web app is API-only. `--start-api` posts the handle to `POST /oauth/start`, keeps the
`swc_oauth_state` cookie, then drives the authorization server. Without `--start-api` the script opens
the given url directly.

A bidder in `localRepo` mode does not sign in. Its association is one `badgeBlueKeys` record in the
operator's repo; the application's watch verifies the record and the identity controller writes the
operator DID into `MarketIdentity.spec.approvedAssociations`. See
[CONTRIBUTING.md](../CONTRIBUTING.md#bidder-association) for the `associate-bidder.ts` procedure.

`oauth-signin.ts` options: `--creds`, `--account`, and `--url` (all required), `--start-api` (begin at
the gateway's `/oauth/start` endpoint), `--out` (the screenshot directory), `--done "<text>"` (wait for
this text after consent), `--chrome` (the browser path, default `/usr/bin/google-chrome`),
`--chrome-arg` (an extra browser flag; repeat it), `--sign-in-as` (type this identifier instead of the
handle in the credential file) and `--cookie-file` (write the browser cookies to this file, mode 0600).

A QR url and an oauthBrowser visitUrl expire after 5 minutes, so read the current one from the
MarketIdentity status right before running the script. To force an `oauthQr` identity to sign in again,
delete its session secret and restart its app; the MarketIdentity reports `AwaitingScan` with a new url.

Check the result with `kubectl get marketidentity <name> -o jsonpath='{.status.phase}'` on the app's
kcp: `Ready`.

Symptoms: a gateway log line `no oauth session stored for did:...` means the gateway user must sign in.
A bidder log line `ScopeMissingError` means the OAuth client scope must be deployed to qr.fedfork.com
(a separate host) and the identity re-signed.

## See also

- [The bidder](bidder.md)
- [The gateway](gateway.md)
