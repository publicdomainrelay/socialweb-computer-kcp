//! docs/acceptance/0007-bidder-termination.md: an operator delete of an in-flight VM on its bidder's kcp ends it cleanly:
//! the ComputeVM goes, `vm.terminated` is pinned `deleted_on_request`, the requester's contract records go, the host is
//! empty. Needs the winner's kcp in the target (skipped otherwise).

use std::time::Duration;

use testkit::bidder::BidderKcp;
use testkit::*;

const MIN: Duration = Duration::from_secs(60);

#[tokio::test(flavor = "multi_thread")]
async fn operator_delete_ends_the_vm() {
    run("0007", "operator-delete", &["alice"], |ctx| async move {
        let did = ctx.account("alice")?.did.clone();
        let name = format!(
            "st-term-{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)?
                .as_secs()
        );
        let rr = RunRequest::start(&ctx, "alice", &name, &["--hold", "--exec", "id"]).await?;
        rr.wait_phase("Held", 10 * MIN).await?;
        rr.wait_exit(5 * MIN).await?;
        let s = rr.summary()?;
        let winner = s["contract"]["winnerDid"]
            .as_str()
            .unwrap_or_default()
            .to_string();
        let receipt = s["contract"]["receiptUri"]
            .as_str()
            .context("no receipt")?
            .to_string();
        let Some(b) = ctx.target.bidder_by_did(&winner) else {
            return ctx.skip(&format!("winner {winner} is not a target bidder"));
        };
        let Some(bk) = BidderKcp::connect(b).await? else {
            return ctx.skip(&format!("the target names no kcp for {}", b.name));
        };
        let prefix = format!("fc-{}", &did.trim_start_matches("did:plc:")[..12]);
        // name = fc-<first 12 chars of the requester DID>-<hash>; more than one: the one naming the receipt
        let mut vms: Vec<_> = bk
            .kcp
            .list("ComputeVM")
            .await?
            .into_iter()
            .filter(|v| {
                v["metadata"]["name"]
                    .as_str()
                    .is_some_and(|n| n.starts_with(&prefix))
            })
            .collect();
        if vms.len() > 1 {
            vms.retain(|v| v.to_string().contains(receipt.rsplit('/').next().unwrap()));
        }
        ensure!(
            vms.len() == 1,
            "{} candidate ComputeVMs of this run on {}",
            vms.len(),
            b.name
        );
        let vm = vms[0]["metadata"]["name"].as_str().unwrap().to_string();
        let t = ctx.t();
        bk.kcp.delete("ComputeVM", &vm).await?;
        bk.kcp.wait_gone("ComputeVM", &vm, 2 * MIN).await?;
        ctx.reading("gone_s", ctx.t() - t);
        let rk = termination_rkey(&receipt);
        let term = eventually(
            "vm.terminated pinned",
            2 * MIN,
            Duration::from_secs(3),
            || async { ctx.at.record_of(&winner, VM_TERMINATED, &rk).await },
        )
        .await?;
        ensure!(
            term["reason"] == "deleted_on_request",
            "vm.terminated {term}"
        );
        ctx.reading(
            "terminated",
            json!({ "rkey": rk, "reason": term["reason"], "request": term["request"]["uri"] }),
        );
        rr.wait_log("requester_contract_records_deleted", 3 * MIN)
            .await?;
        ctx.reading("records_checked", ctx.records_gone(&rr.logs(), &did).await?);
        ensure!(
            bk.vms_of(receipt.rsplit('/').next().unwrap())
                .await?
                .is_empty(),
            "the host still runs a VM of the receipt"
        );
        Ok(())
    })
    .await;
}
