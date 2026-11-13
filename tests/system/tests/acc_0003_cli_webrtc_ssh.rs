//! docs/acceptance/0003-cli-webrtc-ssh.md: CLI ssh over WebRTC, fresh and restarted invocation kcp, no plaintext
//! signaling, release. Discovery picks the winner; with every bidder's kcp in the target, each host is forced in turn by
//! pausing the others (the runbook's per-host form).

use std::time::Duration;

use testkit::bidder::BidderKcp;
use testkit::*;

const MIN: Duration = Duration::from_secs(60);

async fn one(ctx: &std::sync::Arc<Ctx>, label: &str) -> Result<Value> {
    let name = format!(
        "st-acc34-{label}-{}",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)?
            .as_secs()
    );
    let rr = RunRequest::start(
        ctx,
        "alice",
        &name,
        &[
            "--hold",
            "--tunnel",
            "ssh:22:webrtc",
            "--transport",
            "webrtc",
            "--exec",
            "uname -a; id",
        ],
    )
    .await?;
    rr.wait_phase("Held", 10 * MIN).await?;
    let held = ctx.t();
    let first = rr.wait_log("ssh returned", 3 * MIN).await?;
    ensure!(
        first["transport"] == "webrtc" && first["exit"] == 0,
        "first ssh {first}"
    );
    rr.wait_exit(3 * MIN).await?;
    let out = rr.ssh(Some("webrtc"), "uname -a; id").await?;
    ensure!(
        out.contains("Linux") && out.contains("uid="),
        "restarted ssh: {out}"
    );
    let s = rr.summary()?;
    let winner = s["contract"]["winnerDid"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    let receipt = s["contract"]["receiptUri"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    ensure!(
        ctx.target.bidder_by_did(&winner).is_some(),
        "winner {winner} is not a target bidder"
    );
    let rel = rr.release().await?;
    let s = rr.summary()?;
    ensure!(
        s["phase"] == "Released" || s["contract"]["phase"] == "Released",
        "not Released: {} {}",
        s["phase"],
        s["contract"]["phase"]
    );
    ctx.check_logs(&rr.logs())?;
    let did = ctx.account("alice")?.did.clone();
    let n = ctx.records_gone(&rr.logs(), &did).await?;
    ctx.release_record_gone(&did, &receipt).await?;
    Ok(json!({
        "winner": winner, "held_s": held, "ssh_fresh_s": first["seconds"], "release_s": rel, "records_checked": n,
        "host_empty": ctx.host_empty(&winner, receipt.rsplit('/').next().unwrap_or("-")).await?,
    }))
}

#[tokio::test(flavor = "multi_thread")]
async fn webrtc_ssh_fresh_and_restarted() {
    run("0003", "webrtc-ssh", &["alice"], |ctx| async move {
        let bs = ctx.bidder_kcps().await?;
        if bs.len() < ctx.target.bidders.len() || bs.len() < 2 {
            ctx.reading("discovery", one(&ctx, "any").await?);
            return Ok(());
        }
        let mut per_host = json!({});
        for i in 0..bs.len() {
            for (j, b) in bs.iter().enumerate() {
                if j != i {
                    let ep = b.bidder.clone();
                    ctx.defer(&format!("resume {}", ep.name), async move {
                        BidderKcp::connect(&ep)
                            .await?
                            .context("kcp")?
                            .suspend(false)
                            .await
                    });
                    b.suspend(true).await?;
                } else {
                    b.suspend(false).await?;
                }
            }
            let r = one(&ctx, &bs[i].bidder.name).await?;
            ensure!(
                r["winner"] == bs[i].bidder.did.as_str(),
                "{} paused all but itself, winner {}",
                bs[i].bidder.name,
                r["winner"]
            );
            per_host[&bs[i].bidder.name] = r;
        }
        for b in &bs {
            b.suspend(false).await?;
        }
        ctx.reading("per_host", per_host);
        Ok(())
    })
    .await;
}
