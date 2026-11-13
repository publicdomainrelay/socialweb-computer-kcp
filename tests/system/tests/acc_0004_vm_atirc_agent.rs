//! docs/acceptance/0004-vm-atirc-agent.md: an atirc agent in a market VM (CLI path, discovery, no bidders list).

use std::time::Duration;

use testkit::atirc::OwnerNode;
use testkit::target::{check_private, expand};
use testkit::*;

const ASSOC: &str = "com.publicdomainrelay.temp.market.association";
const ENDORSE: &str = "link.ident.endorsement";
const MIN: Duration = Duration::from_secs(60);

/// 0004 Steps (CLI), "Check the pair + grant chain", "Release and its checks", Pass criteria.
#[tokio::test(flavor = "multi_thread")]
async fn agent_vm_chain_answer_release() {
    run("0004", "cli-agent", &["alice"], |ctx| async move {
        let cfg = ctx.target.agent_config.clone().context("target agentConfig: the atirc agent config (harness, model)")?;
        check_private(&expand(&cfg))?;
        let cfg = expand(&cfg).display().to_string();
        let renter = ctx.account("alice")?.did.clone();

        // the owner mesh node runs in the orchestrator requester's kcp
        let owner = Requester::start(&ctx, "alice", "owner", SignIn::from_env(), &[]).await?;

        let name = format!("st-agent-{}", atlibs_now_s());
        let rr = RunRequest::start(&ctx, "alice", &name, &["--hold", "--agent-config", &cfg, "--agent=/etc/atirc/atirc.env"]).await?;
        rr.wait_phase("Held", 10 * MIN).await?;
        ctx.reading("held_s", ctx.t());
        rr.wait_log("requester_agent_mesh_key_written", 2 * MIN).await?;
        rr.wait_exit(5 * MIN).await?;
        let unit = rr.ssh(None, "systemctl is-active atirc-agent.service; stat -c '%U %a' /etc/atirc/iroh_key.hex /etc/atirc/atirc.env /etc/atirc/config.toml").await?;
        ensure!(unit.contains("inactive"), "atirc-agent.service active before agentStart: {unit}");
        ensure!(unit.matches("root 600").count() == 3, "/etc/atirc files not root 0600: {unit}");

        let t_start = ctx.t();
        rr.agent().await?;
        let written = rr.wait_log("requester_association_written", 5 * MIN).await?;
        let r2 = written["subject"].as_str().context("requester_association_written without subject")?.to_string();
        ctx.reading("association_after_agent_start_s", ctx.t() - t_start);
        let peer = rr.wait_log("requester_agent_identity", 5 * MIN).await?["peer"].as_str().context("no peer")?.to_string();
        let (identity, agent) = peer.split_once('@').context("peer is <identity>@<agent DID>")?;
        ctx.reading("r2", &r2);
        ctx.reading("agent", agent);

        // leftovers after any failure: release first, then the renter's endorsement and grant at rkey = R2
        {
            let (ctx2, rr2, r2c) = (ctx.clone(), rr.clone(), r2.clone());
            ctx.defer("renter endorsement + grant", async move {
                rr2.release().await?;
                let s = ctx2.at.session(ctx2.target.account("alice")?).await?;
                ctx2.at.delete_record(&s, ENDORSE, &r2c).await?;
                ctx2.at.delete_record(&s, ASSOC, &r2c).await
            });
        }

        // the pair + grant chain (read only)
        let grant = ctx.at.record_of(&renter, ASSOC, &r2).await?.context("renter grant at rkey R2")?;
        ensure!(grant["agent"] == r2.as_str() && grant["contract"].is_string() && grant["expiresAt"].is_string(), "renter grant {grant}");
        ensure!(ctx.at.record_of(&renter, ENDORSE, &r2).await?.context("renter endorsement")?["agent"] == r2.as_str());
        let g2 = ctx.at.record_of(&r2, ASSOC, agent).await?.context("R2 grant at rkey agent")?;
        ensure!(g2["agent"] == agent && g2["contract"] == grant["contract"] && g2["expiresAt"] == grant["expiresAt"], "R2 grant {g2}");
        ensure!(ctx.at.record_of(&r2, ENDORSE, agent).await?.context("R2 endorsement")?["agent"] == agent);
        let pr = ctx.at.record_of(agent, "computer.socialweb.peer", identity).await?.context("peer record in the agent repo")?;
        ensure!(pr["iroh"].is_string() && pr.get("lease").is_none(), "peer record {pr}");
        ensure!(ctx.at.also_known_as(&r2).await?.contains(&format!("https://ident.link/agent-of/{renter}")));
        ensure!(ctx.at.also_known_as(agent).await?.contains(&format!("https://ident.link/agent-of/{r2}")));
        for d in [r2.as_str(), agent] {
            let keys = ctx.at.rotation_keys(d).await?;
            ensure!(keys.len() == 2, "{d} rotationKeys {keys:?}: want [renter key, VM localPDS key]");
        }
        // the walk (identlink, verified getRecord): agent -> R2 (depth 1) -> renter (the root), the grants' expiresAt
        let walk = ctx.walk(agent).await;
        let grant_exp = chrono::DateTime::parse_from_rfc3339(grant["expiresAt"].as_str().unwrap_or_default())?;
        ensure!(
            walk.errors.is_empty() && walk.roots == [renter.clone()] && walk.ancestors.iter().any(|a| a.did == r2 && a.depth == 1) && walk.expires_at.map(|e| e.timestamp()) == Some(grant_exp.timestamp()),
            "walk {walk:?}"
        );

        // the owner node reaches the agent; it answers a prompt on the mesh
        let node = OwnerNode::start(&ctx, &owner, "alice", "st-owner", std::slice::from_ref(&peer)).await?;
        node.wait_online(identity, 5 * MIN).await?;
        ctx.reading("online_after_agent_start_s", ctx.t() - t_start);
        ensure!(node.mesh(&["peers"]).await?.contains(identity), "the owner node does not list {identity}");
        ctx.reading("answer_s", node.prompt_uname(identity, 3 * MIN).await?);

        // release: fast, records gone, both DIDs tombstoned
        let winner = rr.summary()?["contract"]["winnerDid"].as_str().unwrap_or_default().to_string();
        let receipt = rr.summary()?["contract"]["receiptUri"].as_str().unwrap_or_default().to_string();
        ctx.reading("winner", &winner);
        let secs = rr.release().await?;
        ctx.reading("release_s", secs);
        // fast release: r2Released ~9 s after the release, the CLI stops ~5 s after Released; the guest tombstones
        ensure!(secs < 60.0, "release took {secs} s (fast release: ~15 s, not the 3 min timeout)");
        rr.wait_log("requester_guest_requester_released", MIN).await?;
        rr.wait_log("requester_guest_dids_tombstoned", MIN).await?;
        ensure!(rr.logs().iter().all(|p| requester::log_lines(p, "requester_guest_requester_release_timeout").is_empty()), "the release waited for the guest's timeout");
        let left = ctx.at.still_present(&[format!("at://{renter}/{ENDORSE}/{r2}"), format!("at://{renter}/{ASSOC}/{r2}")]).await?;
        ensure!(left.is_empty(), "left after release: {left:?}");
        for d in [r2.as_str(), agent] {
            eventually(&format!("{d} tombstoned"), 3 * MIN, Duration::from_secs(5), || async { Ok(ctx.at.tombstoned(d).await?.then_some(())) }).await?;
        }
        ctx.reading("records_checked", ctx.records_gone(&rr.logs(), &renter).await?);
        ctx.release_record_gone(&renter, &receipt).await?;
        ctx.reading("host_empty", ctx.host_empty(&winner, receipt.rsplit('/').next().unwrap_or("-")).await?);
        ctx.check_logs(&rr.logs())?;
        for p in rr.logs() {
            let text = std::fs::read_to_string(&p).unwrap_or_default();
            ensure!(!text.contains("chat.atirc.mint") && !text.contains("atirc_minted"), "{}: an atirc mint for a token-less agent", p.display());
        }
        Ok(())
    })
    .await;
}

fn atlibs_now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
}
