//! docs/acceptance/0005-guest-requester-association.md: a guest requester R2 rents a nested VM through the renter's
//! contract-scoped grant + endorsement; the outer release cascades; records gone, DIDs tombstoned. Part B step 5
//! (out-of-band loss) deletes the outer ComputeVM on its bidder's kcp (fault injection; skipped without that kcp).

use std::time::Duration;

use testkit::bidder::BidderKcp;
use testkit::*;

const ASSOC: &str = "com.publicdomainrelay.temp.market.association";
const ENDORSE: &str = "link.ident.endorsement";
const MIN: Duration = Duration::from_secs(60);
const ACCOUNT: &str = "alice";

fn stamp() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
}

/// Steps 1-5 (`run request r2`), the pair + grant check while R2 is Held, the outer-only release.
#[tokio::test(flavor = "multi_thread")]
async fn a_r2_nested_vm_cascade() {
    run("0005", "A", &[ACCOUNT], |ctx| async move {
        let renter = ctx.account(ACCOUNT)?.did.clone();
        let name = format!("st-r2-{}", stamp());
        let rr = RunRequest::start(&ctx, ACCOUNT, &name, &["--hold", "--guest-tools", "socialweb-computer", "--associate", "guest", "--agent"]).await?;
        rr.wait_phase("Held", 10 * MIN).await?;
        rr.wait_exit(5 * MIN).await?;
        let (ok, out) = rr.bin.run(&["run", "request", "r2", &name, "--state-dir", &rr.state.display().to_string(), "--r2-name", "r2-st", "--agent", "--hold"]).await?;
        ensure!(ok, "run request r2: {}", requester::tail(&out));
        let r2 = rr.wait_log("requester_association_written", 5 * MIN).await?["subject"].as_str().context("no subject")?.to_string();
        ctx.reading("r2", &r2);
        {
            let (ctx2, rr2, r2c) = (ctx.clone(), rr.clone(), r2.clone());
            ctx.defer("renter endorsement + grant", async move {
                rr2.release().await?;
                let s = ctx2.at.session(ctx2.target.account(ACCOUNT)?).await?;
                ctx2.at.delete_record(&s, ENDORSE, &r2c).await?;
                ctx2.at.delete_record(&s, ASSOC, &r2c).await
            });
        }
        // R2 Held on its nested VM (the guest's socialweb-r2.service journal)
        eventually("R2 Held", 10 * MIN, Duration::from_secs(10), || async {
            let j = rr.ssh(None, "journalctl -u socialweb-r2.service -o cat --no-pager | grep -c '\"phase\":\"Held\"' || true").await?;
            Ok(j.lines().any(|l| l.trim().parse::<u32>().is_ok_and(|n| n > 0)).then_some(()))
        })
        .await?;
        ctx.reading("r2_held_s", ctx.t());

        let grant = ctx.at.record_of(&renter, ASSOC, &r2).await?.context("renter grant")?;
        ensure!(grant["agent"] == r2.as_str() && grant["contract"].is_string() && grant["expiresAt"].is_string(), "grant {grant}");
        ensure!(ctx.at.record_of(&renter, ENDORSE, &r2).await?.context("renter endorsement")?["agent"] == r2.as_str());
        let req = ctx.at.record_of(&r2, "com.publicdomainrelay.temp.market.associationRequest", &renter).await?.context("R2 associationRequest")?;
        ensure!(req["principal"] == renter.as_str() && req["contract"] == grant["contract"], "associationRequest {req}");
        ensure!(ctx.at.also_known_as(&r2).await?.contains(&format!("https://ident.link/agent-of/{renter}")));
        let (ok, walk) = rr.bin.run(&["internal", "association", &r2]).await?;
        ensure!(ok && walk.contains(&renter), "internal association {r2}: {walk}");

        let secs = rr.release().await?;
        ctx.reading("release_s", secs);
        rr.wait_log("requester_association_deleted", 2 * MIN).await?;
        let left = ctx.at.still_present(&[format!("at://{renter}/{ENDORSE}/{r2}"), format!("at://{renter}/{ASSOC}/{r2}")]).await?;
        ensure!(left.is_empty(), "left: {left:?}");
        eventually("R2 tombstoned", 3 * MIN, Duration::from_secs(5), || async { Ok(ctx.at.tombstoned(&r2).await?.then_some(())) }).await?;
        ctx.reading("records_checked", ctx.records_gone(&rr.logs(), &renter).await?);
        ctx.check_logs(&rr.logs())?;
        Ok(())
    })
    .await;
}

/// Part B steps 1-4: R2 with a co-located account mesh1 (peer records, its grant chain), then release; step 5 the
/// out-of-band loss: delete the outer ComputeVM on its bidder, the renter tombstones both DIDs with its rotation key.
#[tokio::test(flavor = "multi_thread")]
async fn b_colocated_and_out_of_band_loss() {
    run("0005", "B", &[ACCOUNT], |ctx| async move {
        let renter = ctx.account(ACCOUNT)?.did.clone();
        let name = format!("st-r2b-{}", stamp());
        let rr = RunRequest::start(&ctx, ACCOUNT, &name, &["--hold", "--guest-tools", "socialweb-computer", "--associate", "guest"]).await?;
        rr.wait_phase("Held", 10 * MIN).await?;
        rr.wait_exit(5 * MIN).await?;
        rr.ssh(
            None,
            "printf 'R2_NAME=r2\\nR2_HOLD=1\\nR2_COLOCATED=mesh1\\n' > /run/socialweb-r2/r2-start.env && chown root:socialweb-r2 /run/socialweb-r2/r2-start.env && chmod 0640 /run/socialweb-r2/r2-start.env && systemctl start --no-block socialweb-r2.service",
        )
        .await?;
        let r2 = rr.wait_log("requester_association_written", 5 * MIN).await?["subject"].as_str().context("no subject")?.to_string();
        // mesh1: R2's co-located account, endorsed by R2
        let mesh1 = eventually("R2's endorsement of mesh1", 5 * MIN, Duration::from_secs(10), || async {
            let r = ctx.at.http.get(format!("{}/xrpc/com.atproto.repo.listRecords", ctx.at.net.pds(&r2).await.map_err(|e| anyhow!(e))?)).query(&[("repo", r2.as_str()), ("collection", ENDORSE)]).send().await?;
            let v: Value = r.json().await.unwrap_or_default();
            Ok(v["records"].as_array().and_then(|a| a.first()).and_then(|r| r["value"]["agent"].as_str()).map(String::from))
        })
        .await?;
        ctx.reading("r2", &r2);
        ctx.reading("mesh1", &mesh1);
        let r2_handle = ctx.at.also_known_as(&r2).await?.into_iter().find_map(|a| a.strip_prefix("at://").map(String::from)).context("R2 handle")?;
        let peer = ctx.at.record_of(&mesh1, "computer.socialweb.peer", &format!("mesh1.{r2_handle}")).await?.context("mesh1 peer record")?;
        ensure!(peer["iroh"].is_string() && peer.get("lease").is_none(), "mesh1 peer {peer}");
        ensure!(ctx.at.record_of(&r2, ASSOC, &mesh1).await?.is_some(), "R2's grant of mesh1");
        ensure!(ctx.at.also_known_as(&mesh1).await?.contains(&format!("https://ident.link/agent-of/{r2}")));
        let (ok, walk) = rr.bin.run(&["internal", "association", &mesh1]).await?;
        ensure!(ok && walk.contains(&renter), "internal association mesh1: {walk}");

        // step 5: out-of-band loss on the winner's kcp; else an ordinary release (step 4)
        let s = rr.summary()?;
        let winner = s["contract"]["winnerDid"].as_str().unwrap_or_default().to_string();
        let bk = match ctx.target.bidder_by_did(&winner) {
            Some(b) => BidderKcp::connect(b).await?,
            None => None,
        };
        match bk {
            Some(bk) => {
                let prefix = format!("fc-{}", &renter.trim_start_matches("did:plc:")[..12]);
                let vm = bk.kcp.list("ComputeVM").await?.into_iter().find(|v| v["metadata"]["name"].as_str().is_some_and(|n| n.starts_with(&prefix))).context("the outer ComputeVM")?;
                let vm_name = vm["metadata"]["name"].as_str().unwrap().to_string();
                bk.kcp.delete("ComputeVM", &vm_name).await?;
                bk.kcp.wait_gone("ComputeVM", &vm_name, 2 * MIN).await?;
                rr.wait_log("requester_tombstoned", 5 * MIN).await?;
                ctx.reading("out_of_band", true);
            }
            None => {
                rr.release().await?;
                ctx.reading("out_of_band", "skipped: no kcp for the winner");
            }
        }
        for d in [r2.as_str(), mesh1.as_str()] {
            eventually(&format!("{d} tombstoned"), 5 * MIN, Duration::from_secs(5), || async { Ok(ctx.at.tombstoned(d).await?.then_some(())) }).await?;
        }
        rr.wait_log("requester_contract_records_deleted", 3 * MIN).await?;
        ctx.reading("records_checked", ctx.records_gone(&rr.logs(), &renter).await?);
        ctx.check_logs(&rr.logs())?;
        Ok(())
    })
    .await;
}
