# OAuth sign-in, association and its failures

The long form of the README sign-in section: what the script drives, every option, the per-mode notes and how to recover. Short path in [the README](../README.md#sign-in-with-oauth).

`scripts/oauth-signin.ts` drives a headless `/usr/bin/google-chrome` (puppeteer-core) through the flow: the page, the PDS authorize page, the password, then "Authorize". The script reads the handle and password from the credential file itself, so they never appear on the command line or in a log. It writes one screenshot per step to `~/.tmp/oauth-signin/<ts>/` (mode 0700).

The credential file (mode 0600) exports `<ACCOUNT>_ATPROTO_HANDLE` and `<ACCOUNT>_ATPROTO_PASSWORD`.

```bash
# the web app user (alice) signs in to the gateway; do this after every fresh gateway deploy
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.aliceoa_bsky_social_source --account ALICE --url https://k8s.socialweb.computer/
# the bidder's handle associates the bidder's own DID once, through the QR url its MarketIdentity reports;
# K is the bidder's kcp kubeconfig
U=$(kubectl --kubeconfig $K get marketidentity bidder -o jsonpath='{.status.pendingAction.url}')
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source --account BOB --url "$U" --click '^Yes, this is my bidder$'
```

The bidder's url is `<qrOrigin>/#bdr=<bidder DID>`. After the sign-in, `--click` presses the confirm button: the page calls the bidder's `associateConfirm` through the account's PDS, the bidder's `pds` DenoPod records the association as the AtprotoRecord object `association-...` in the bidder's kcp, and the page writes one `badgeBlueKeys` record into the account's repo. That is the only write to the account. The MarketIdentity then reads `Ready` with the account in `status.associations`, and a restart or an `upgrade` keeps the DID: the key is in the bidder's OpenBao and the repo in `<handle state>/pds`.

For the bidder, read the current association url from the `MarketIdentity` object's `status.pendingAction.url` in the bidder's kcp (see [Inspect the kcp of an application](#inspect-the-kcp-of-an-application)). On an installed bidder host the kcp kubeconfig is readable by root only, so run `kubectl` through `sudo` with `K=/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp/admin.kubeconfig`; the `MarketIdentity` appears a few seconds after `install bidder` returns, so retry on `NotFound`. Run `oauth-signin.ts` on a machine with Chrome, passing the url.

`oauth-signin.ts` options: `--creds`, `--account`, and `--url` (all required), `--out` (the screenshot directory), `--done "<text>"` (wait for this text after consent), `--chrome` (the browser path, default `/usr/bin/google-chrome`), `--chrome-arg` (an extra browser flag; repeat it), `--sign-in-as` (type this identifier instead of the handle in the credential file) and `--cookie-file` (write the browser cookies to this file, mode 0600).

A QR url expires after 5 minutes, so read the current one from the MarketIdentity status right before running the script.

The association url does not expire; the session-transfer QR of the old `oauthQr` mode did. To force the bidder to sign in again, for example after the OAuth client scope changes on qr.fedfork.com (only an `oauthQr` identity has such a session):

1. Delete `bidder/metadata/identity/bidder/session` in the bidder's OpenBao: log in with the certificate at `<handle state>/openbaoservers/bidder/openbao/tls` as the client `bidder` (its certificate is under `clients/bidder`).
2. Restart the bidder unit, as its account: `sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) systemctl --user restart 'socialweb-computer-bidder@<handle>.service'`. The `MarketIdentity` reports `AwaitingScan` with a new url.
3. Run `oauth-signin.ts` against that url.

Check the result with `kubectl get marketidentity bidder -o jsonpath='{.status.phase}'` on the bidder's kcp: `Ready`.

Symptoms: a gateway log line `no oauth session stored for did:...` means the web app user must sign in. A bidder log line `ScopeMissingError` means the OAuth client scope must be deployed to qr.fedfork.com (a separate host) and the bidder re-signed.

> The `oauthQr` session-reset steps above applied before plan 0018; a `prod-qr` bidder now runs `localRepo` and holds no OAuth session, so only the web app sign-in and the association steps still apply.

## See also

- [The bidder](bidder.md)
- [The gateway](gateway.md)
