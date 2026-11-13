//! docs/acceptance/0008-host-observer.md: a VM's timeline on its host and the orphan check after release, read from the
//! winner's kcp; the vm.terminated record pinned by one getRecord at its computed key. Needs the winner's kcp
//! (skipped otherwise). The watchers' atirc posts and journal counts are the human form; not ported.

use std::time::Duration;

use testkit::bidder::BidderKcp;
use testkit::*;

const MIN: Duration = Duration::from_secs(60);

#[tokio::test(flavor = "multi_thread")]
async fn vm_timeline_pin_orphan_check() {
    run("0008", "timeline", &["alice"], |ctx| async move {
        let name = format!("st-obs-{}", std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?.as_secs());
        let rr = RunRequest::start(&ctx, "alice", &name, &["--hold", "--exec", "id"]).await?;
        rr.wait_phase("Held", 10 * MIN).await?;
        rr.wait_exit(5 * MIN).await?;
        let s = rr.summary()?;
        let winner = s["contract"]["winnerDid"].as_str().unwrap_or_default().to_string();
        let receipt = s["contract"]["receiptUri"].as_str().context("no receipt")?.to_string();
        let rk = receipt.rsplit('/').next().unwrap().to_string();
        let bk = match ctx.target.bidder_by_did(&winner) {
            Some(b) => BidderKcp::connect(b).await?,
            None => None,
        };
        let running = match &bk {
            Some(bk) => {
                let vms = bk.vms_of(&rk).await?;
                ensure!(vms.iter().any(|v| v["status"]["phase"] == "Running"), "no Running VM of the receipt on the winner");
                Some(vms.iter().map(|v| json!({ "kind": v["kind"], "name": v["metadata"]["name"], "phase": v["status"]["phase"] })).collect::<Vec<_>>())
            }
            None => None,
        };
        ctx.reading("running", running);
        ctx.reading("release_s", rr.release().await?);
        let term = eventually("vm.terminated pinned", 2 * MIN, Duration::from_secs(3), || async { ctx.at.record_of(&winner, VM_TERMINATED, &termination_rkey(&receipt)).await }).await?;
        ctx.reading("terminated", json!({ "reason": term["reason"], "request": term["request"]["uri"], "createdAt": term["createdAt"] }));
        ensure!(term["reason"] == "deleted_on_request", "vm.terminated {term}");
        ctx.release_record_gone(&ctx.account("alice")?.did, &receipt).await?;
        match bk {
            Some(bk) => {
                eventually("the host empty of the receipt", 2 * MIN, Duration::from_secs(3), || async { Ok(bk.vms_of(&rk).await?.is_empty().then_some(())) }).await?;
                ctx.reading("host_empty", true);
            }
            None => ctx.reading("host_empty", "not read: no kcp for the winner"),
        }
        Ok(())
    })
    .await;
}
