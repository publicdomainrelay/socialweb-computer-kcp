//! docs/acceptance/0006-rollout-compat.md: after a rollout every bidder is signed in and runs bidder-tangled-vouch, and
//! an unchanged client (SWC_COMPAT_REQUESTER, default the target's requester) still rents and releases. Host journals
//! (ERROR counts) need root on the hosts: not ported; the page compat smoke is 0009.

use std::time::Duration;

use testkit::*;

const MIN: Duration = Duration::from_secs(60);

/// Per-bidder checks on each bidder kcp the target names.
#[tokio::test(flavor = "multi_thread")]
async fn bidders_ready_signed_in_vouch_policy() {
    run("0006", "bidders", &[], |ctx| async move {
        let bs = ctx.bidder_kcps().await?;
        if bs.is_empty() {
            return ctx.skip("needs bidder kcps in the target");
        }
        let mut out = json!({});
        for b in &bs {
            let mi = b
                .kcp
                .get("MarketIdentity", "bidder")
                .await?
                .context("MarketIdentity bidder")?;
            let signed = mi["status"]["conditions"]
                .as_array()
                .into_iter()
                .flatten()
                .find(|c| c["type"] == "SignedIn")
                .map(|c| c["status"].clone());
            ensure!(
                mi["status"]["phase"] == "Ready" && mi["status"]["pendingAction"].is_null(),
                "{} MarketIdentity {}",
                b.bidder.name,
                mi["status"]
            );
            ensure!(
                signed.as_ref().is_none_or(|s| s == "True"),
                "{} SignedIn {signed:?}",
                b.bidder.name
            );
            let policy =
                b.kcp.get("Bidder", "bidder").await?.context("Bidder")?["spec"]["policy"].clone();
            ensure!(
                policy.is_null() || policy == "bidder-tangled-vouch",
                "{} policy {policy}",
                b.bidder.name
            );
            ensure!(
                b.did().await? == b.bidder.did,
                "{} DID changed",
                b.bidder.name
            );
            out[&b.bidder.name] = json!({ "vms": b.vms().await?.len(), "policy": policy });
        }
        ctx.reading("bidders", out);
        Ok(())
    })
    .await;
}

/// Compat smoke: `run request --hold --exec id` of the unchanged CLI, Held, Released, records gone.
#[tokio::test(flavor = "multi_thread")]
async fn compat_cli() {
    run("0006", "compat-cli", &["alice"], |ctx| async move {
        let spec =
            std::env::var("SWC_COMPAT_REQUESTER").unwrap_or_else(|_| ctx.target.requester.clone());
        let bin = Binary::resolve(&spec).await?;
        ctx.reading("compat_requester", &bin.version);
        let name = format!(
            "st-compat-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)?
                .as_secs()
        );
        let rr =
            RunRequest::start_with(&ctx, bin, "alice", &name, &["--hold", "--exec", "id"]).await?;
        rr.wait_phase("Held", 10 * MIN).await?;
        ctx.reading("held_s", ctx.t());
        rr.wait_exit(5 * MIN).await?;
        ensure!(
            std::fs::read_to_string(&rr.log)?.contains("uid="),
            "no `id` output"
        );
        ctx.reading("release_s", rr.release().await?);
        let did = ctx.account("alice")?.did.clone();
        ctx.reading("records_checked", ctx.records_gone(&rr.logs(), &did).await?);
        ctx.check_logs(&rr.logs())?;
        Ok(())
    })
    .await;
}
