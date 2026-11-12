//! docs/acceptance/0001-auth-modes.md sections 1, 7 and 8 on the bidders' own kcps (target bidders[].kcp): root pause,
//! per-component pause (heartbeat frozen, obligations kept), typed settings, drift revert, fail-closed overrides.
//! Section 2 (a temporary oauthQr bidder) needs root on a bidder host: not ported.

use std::time::Duration;

use testkit::bidder::BidderKcp;
use testkit::*;

const HEARTBEAT: &str = "com.publicdomainrelay.temp.market.heartbeat";
const OFFERING: &str = "com.publicdomainrelay.temp.market.offering";

async fn heartbeat(ctx: &Ctx, did: &str) -> Result<String> {
    Ok(ctx
        .at
        .record_of(did, HEARTBEAT, "self")
        .await?
        .context("no heartbeat/self")?["createdAt"]
        .as_str()
        .unwrap_or_default()
        .into())
}

/// Section 1: root `spec.suspend` on every bidder but one: Suspended roots, Bidders `suspendParticipation` true and Ready.
#[tokio::test(flavor = "multi_thread")]
async fn s1_root_pause() {
    run("0001", "s1", &[], |ctx| async move {
        let bs = ctx.bidder_kcps().await?;
        if bs.len() < 2 {
            return ctx.skip("needs two bidder kcps in the target");
        }
        for b in &bs[1..] {
            let name = b.bidder.name.clone();
            let ep = b.bidder.clone();
            ctx.defer(&format!("resume {name}"), async move { BidderKcp::connect(&ep).await?.context("kcp")?.suspend(false).await });
            b.suspend(true).await?;
            let bd = b.kcp.get("Bidder", "bidder").await?.context("Bidder bidder")?;
            ensure!(bd["spec"]["suspendParticipation"] == true && bd["status"]["phase"] == "Ready", "{name} Bidder {}", bd["status"]);
            ctx.reading(&format!("{name}_summary"), &b.kcp.get("MarketBidder", "app").await?.context("app")?["status"]["componentSummary"]);
        }
        for b in &bs[1..] {
            b.suspend(false).await?;
        }
        Ok(())
    })
    .await;
}

/// Section 7: Bidder-only pause: the heartbeat stays frozen for 180 s, the root stays Ready, GuestControlPlane and
/// IngressProxy stay Ready; resume and the heartbeat advances.
#[tokio::test(flavor = "multi_thread")]
async fn s7_component_pause() {
    run("0001", "s7", &[], |ctx| async move {
        let Some(b) = ctx.bidder_kcps().await?.into_iter().next() else {
            return ctx.skip("needs a bidder kcp in the target");
        };
        let did = b.did().await?;
        let ep = b.bidder.clone();
        ctx.defer("drop the Bidder override", async move {
            let b = BidderKcp::connect(&ep).await?.context("kcp")?;
            b.kcp
                .merge(
                    "MarketBidder",
                    "app",
                    &json!({ "spec": { "components": { "Bidder": null } } }),
                )
                .await
                .map(|_| ())
        });
        b.kcp
            .merge(
                "MarketBidder",
                "app",
                &json!({ "spec": { "components": { "Bidder": { "suspend": true } } } }),
            )
            .await?;
        b.kcp
            .wait(
                "Bidder",
                "bidder",
                Duration::from_secs(120),
                "running the paused generation",
                |o| {
                    o.is_some_and(|o| {
                        o["spec"]["suspendParticipation"] == true
                            && o["status"]["observedGeneration"] == o["metadata"]["generation"]
                    })
                },
            )
            .await?;
        let hb = heartbeat(&ctx, &did).await?;
        tokio::time::sleep(Duration::from_secs(180)).await;
        ensure!(
            heartbeat(&ctx, &did).await? == hb,
            "the heartbeat moved while the Bidder was paused"
        );
        let app = b.kcp.get("MarketBidder", "app").await?.context("app")?;
        ensure!(
            app["status"]["phase"] == "Ready"
                && app["status"]["componentSummary"]["suspended"] == 1,
            "root {}",
            app["status"]
        );
        for k in ["GuestControlPlane", "IngressProxy"] {
            for o in b.kcp.list(k).await? {
                ensure!(
                    o["status"]["phase"] == "Ready",
                    "{k} {} is {}",
                    o["metadata"]["name"],
                    o["status"]["phase"]
                );
            }
        }
        ctx.reading("summary_paused", &app["status"]["componentSummary"]);
        b.kcp
            .merge(
                "MarketBidder",
                "app",
                &json!({ "spec": { "components": { "Bidder": null } } }),
            )
            .await?;
        b.kcp
            .wait_phase("Bidder", "bidder", "Ready", Duration::from_secs(120))
            .await?;
        let t = ctx.t();
        eventually(
            "the heartbeat advances",
            Duration::from_secs(120),
            Duration::from_secs(2),
            || async { Ok((heartbeat(&ctx, &did).await? != hb).then_some(())) },
        )
        .await?;
        ctx.reading("heartbeat_advanced_s", ctx.t() - t);
        Ok(())
    })
    .await;
}

/// Section 8: a 5 s heartbeat setting applies, a correct offering is not rewritten, a direct child edit is reverted,
/// a typo key fails closed.
#[tokio::test(flavor = "multi_thread")]
async fn s8_typed_settings() {
    run("0001", "s8", &[], |ctx| async move {
        let Some(b) = ctx.bidder_kcps().await?.into_iter().next() else { return ctx.skip("needs a bidder kcp in the target") };
        let did = b.did().await?;
        let ep = b.bidder.clone();
        ctx.defer("drop the overrides", async move {
            let b = BidderKcp::connect(&ep).await?.context("kcp")?;
            b.kcp.merge("MarketBidder", "app", &json!({ "spec": { "components": null } })).await.map(|_| ())
        });
        let settings = json!({ "spec": { "components": { "Bidder": { "suspend": false, "settings": { "bidder": { "heartbeatIntervalSeconds": 5, "offeringRefreshSeconds": 15 } } } } } });
        b.kcp.merge("MarketBidder", "app", &settings).await?;
        b.kcp.wait_phase("Bidder", "bidder", "Ready", Duration::from_secs(120)).await?;
        let app = b.kcp.wait("MarketBidder", "app", Duration::from_secs(60), "override applied", |o| o.is_some_and(|o| o["status"]["componentOverrides"].to_string().contains("\"applied\":true"))).await?;
        ctx.reading("overrides", &app.unwrap()["status"]["componentOverrides"]);
        let h1 = heartbeat(&ctx, &did).await?;
        tokio::time::sleep(Duration::from_secs(10)).await;
        ensure!(heartbeat(&ctx, &did).await? != h1, "a 5 s heartbeat did not advance in 10 s");
        let cid = |v: Option<Value>| v.map(|v| v.to_string());
        let o1 = cid(ctx.at.record_of(&did, OFFERING, "self").await?);
        tokio::time::sleep(Duration::from_secs(60)).await;
        let off = ctx.at.record_of(&did, OFFERING, "self").await?.context("offering/self")?;
        ensure!(o1 == cid(Some(off.clone())), "the offering was rewritten without a drift");
        ensure!(off.get("endpointUrl").is_none(), "offering carries endpointUrl");
        ctx.reading("offering_endpoints", off["endpoints"].as_array().map(|a| a.iter().map(|e| e["kind"].clone()).collect::<Vec<_>>()));

        // drift: a direct edit of the composed child is reverted
        b.kcp.merge("Bidder", "bidder", &json!({ "spec": { "suspend": true } })).await?;
        b.kcp.wait("Bidder", "bidder", Duration::from_secs(30), "reverted", |o| o.is_some_and(|o| o["spec"]["suspend"] != true)).await?;

        // a typo key fails closed, no child generation changes
        let gen0 = b.kcp.get("Bidder", "bidder").await?.context("bidder")?["metadata"]["generation"].clone();
        b.kcp.merge("MarketBidder", "app", &json!({ "spec": { "components": { "Bldier": { "suspend": true } } } })).await?;
        let app = b.kcp.wait_phase("MarketBidder", "app", "Failed", Duration::from_secs(60)).await?;
        let c = app["status"]["conditions"].as_array().into_iter().flatten().find(|c| c["type"] == "ComponentOverridesValid").cloned().unwrap_or_default();
        ensure!(c["status"] == "False" && c["reason"] == "InvalidKey", "ComponentOverridesValid {c}");
        ensure!(b.kcp.get("Bidder", "bidder").await?.context("bidder")?["metadata"]["generation"] == gen0, "a child changed under a failed override");
        b.kcp.merge("MarketBidder", "app", &json!({ "spec": { "components": null } })).await?;
        b.kcp.wait_phase("MarketBidder", "app", "Ready", Duration::from_secs(120)).await?;
        Ok(())
    })
    .await;
}
