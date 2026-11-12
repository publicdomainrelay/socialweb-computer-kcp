//! docs/acceptance/0013-mixed-prod.md: mixed-prod.yaml through a long-lived `run requester` (A) and the gateway in the
//! requester kcp (C). Discovery: the bidderDids pins are taken out. B (`install requester`) needs root on the requester
//! host: not ported.

use std::time::Duration;

use testkit::atirc::OwnerNode;
use testkit::target::{check_private, expand};
use testkit::*;

const MIN: Duration = Duration::from_secs(60);
const ACCOUNT: &str = "bob";

/// mixed-prod.yaml with the requester's ssh key and no bidder pin.
fn mixed_prod(ssh_key: &str) -> String {
    example("mixed-prod.yaml")
        .lines()
        .filter(|l| !l.contains("bidderDids:") && !l.contains("REPLACE-BIDDER-DID"))
        .collect::<Vec<_>>()
        .join("\n")
        .replace(
            "ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop",
            ssh_key.trim(),
        )
}

/// A4b: agent1's bundle (placeholder env file + the agent config) into the requester's own OpenBao.
async fn agent1_bundle(ctx: &Ctx, r: &Requester) -> Result<()> {
    let cfg = expand(
        ctx.target
            .agent_config
            .as_deref()
            .context("target agentConfig")?,
    );
    check_private(&cfg)?;
    let kcp = r.kcp().await?;
    let addr = kcp
        .get("OpenBaoServer", "requester")
        .await?
        .context("OpenBaoServer requester")?["status"]["address"]
        .as_str()
        .context("no address")?
        .to_string();
    let d = r
        .state
        .join("requesters")
        .join(&r.name)
        .join("dev")
        .join(&r.name)
        .join("openbaoservers/requester/openbao");
    let runtime =
        std::path::PathBuf::from(std::env::var("HOME")?).join(".cache/socialweb-computer/runtime");
    let bao = std::fs::read_dir(&runtime)?
        .flatten()
        .map(|e| e.path().join("bao"))
        .find(|p| p.is_file())
        .context("no bao in the runtime cache")?;
    let bundle = ctx.dir.join("agent1-bundle.json");
    {
        use std::os::unix::fs::OpenOptionsExt;
        let v = json!([{ "path": "/etc/atirc/atirc.env", "value": "", "mode": "0600" }, { "path": "/etc/atirc/config.toml", "value": std::fs::read_to_string(&cfg)?, "mode": "0600" }]);
        std::io::Write::write_all(
            &mut std::fs::OpenOptions::new()
                .create(true)
                .truncate(true)
                .write(true)
                .mode(0o600)
                .open(&bundle)?,
            v.to_string().as_bytes(),
        )?;
    }
    let env = |c: &mut tokio::process::Command| {
        c.env("BAO_ADDR", &addr)
            .env("BAO_CACERT", d.join("tls/ca.crt"))
            .env("BAO_CLIENT_CERT", d.join("clients/requester/client.crt"))
            .env("BAO_CLIENT_KEY", d.join("clients/requester/client.key"));
    };
    let mut login = tokio::process::Command::new(&bao);
    login.args(["write", "-field=token", "auth/cert/login", "name=requester"]);
    env(&mut login);
    let out = login.output().await?;
    ensure!(out.status.success(), "bao cert login failed");
    let mut put = tokio::process::Command::new(&bao);
    put.args([
        "kv",
        "put",
        "-mount=requester",
        "contracts/einfra-mixedprod-agent1/secrets",
    ])
    .arg(format!("bundle=@{}", bundle.display()));
    env(&mut put);
    put.env("BAO_TOKEN", String::from_utf8(out.stdout)?.trim())
        .stdout(std::process::Stdio::null());
    let ok = put.status().await?.success();
    std::fs::remove_file(&bundle)?;
    ensure!(ok, "bao kv put of agent1's bundle failed");
    Ok(())
}

/// 0013 A1-A11: run requester, mixed-prod Deployed, vm1 ssh (iroh, webrtc), sbx1 stdout, agent1 answers, restart keeps
/// everything Held, delete releases and deletes every record.
#[tokio::test(flavor = "multi_thread")]
async fn a_run_requester_mixed_prod() {
    run("0013", "A", &[ACCOUNT], |ctx| async move {
        let did = ctx.account(ACCOUNT)?.did.clone();
        let r = Requester::start(&ctx, ACCOUNT, "mp", SignIn::from_env(), &[]).await?;
        let ready = r.ready()?;
        ensure!(ready["did"] == did.as_str(), "ready file DID {} is not {did}", ready["did"]);
        ctx.reading("identity_mode", &ready["identityMode"]);
        let kcp = r.kcp().await?;
        let mr = kcp.get("MarketRequester", "app").await?.context("MarketRequester app")?;
        ensure!(mr["status"]["phase"] == "Ready" && mr["status"]["did"] == did.as_str(), "MarketRequester {}", mr["status"]);

        // vouches are preconditions: some bidder operator vouches the account (else no bids)
        let mut vouched = vec![];
        for b in &ctx.target.bidders {
            if ctx.at.record_of(&b.operator, "sh.tangled.graph.vouch", &did).await?.is_some() {
                vouched.push(b.name.clone());
            }
        }
        ensure!(!vouched.is_empty(), "no bidder operator vouches {did}");
        ctx.reading("vouched_by_operators_of", &vouched);

        agent1_bundle(&ctx, &r).await?;
        let key = std::fs::read_to_string(ready["sshPublicKey"].as_str().context("no sshPublicKey")?)?;
        let t_apply = ctx.t();
        {
            let rr = r.clone();
            ctx.defer("delete EphemeralInfra mixedprod", async move {
                let k = rr.kcp().await?;
                k.delete("EphemeralInfra", "mixedprod").await?;
                k.wait_gone("EphemeralInfra", "mixedprod", 5 * MIN).await
            });
        }
        kcp.apply_yaml(&mixed_prod(&key)).await?;
        kcp.wait_phase("EphemeralInfra", "mixedprod", "Deployed", 10 * MIN).await?;
        ctx.reading("deployed_s", ctx.t() - t_apply);
        let mut winners = json!({});
        for c in ["vm1", "sbx1", "agent1"] {
            let cc = kcp.get("ComputeContract", &format!("einfra-mixedprod-{c}")).await?.context(c)?;
            let w = cc["status"]["contract"]["winner"]["did"].as_str().unwrap_or_default().to_string();
            ensure!(ctx.target.bidder_by_did(&w).is_some(), "{c} won by {w}, not a target bidder");
            winners[c] = json!(w);
        }
        ctx.reading("winners", &winners);

        // A6, A7
        ensure!(r.ssh("einfra-mixedprod-vm1", None, "cat /root/mixedprod").await?.contains("mixedprod-vm-ok"));
        ensure!(r.ssh("einfra-mixedprod-vm1", Some("webrtc"), "uname -m").await?.contains("x86_64"));
        // the WebRTC transport of testkit (a kcp in a guest is reached the same way): vm1's ssh tag through
        // `run requester proxy`, the bridge carries the guest sshd's banner
        {
            let testkit::target::Via::WebRtc { proxy } = r.webrtc_proxy("einfra-mixedprod-vm1", "ssh") else { unreachable!() };
            let (port, _bridge) = testkit::kcp::stdio_bridge(proxy).await?;
            let mut tcp = tokio::net::TcpStream::connect(("127.0.0.1", port)).await?;
            let mut banner = [0u8; 8];
            tokio::time::timeout(2 * MIN, tokio::io::AsyncReadExt::read_exact(&mut tcp, &mut banner)).await??;
            ensure!(&banner == b"SSH-2.0-", "the WebRTC bridge did not carry the guest's ssh banner");
        }
        let sbx = kcp.get("ComputeContract", "einfra-mixedprod-sbx1").await?.context("sbx1")?;
        let sb = &sbx["status"]["contract"]["sandbox"];
        ensure!(sb["exitCode"] == 0 && sb["stdout"].as_str().is_some_and(|s| s.contains("mixedprod-deno-ok sum=10 on linux/x86_64")), "sbx1 sandbox {sb}");

        // A8
        let logs = r.logs();
        requester::wait_log(&logs, "requester_agent_mesh_key_written", 5 * MIN).await?;
        let t_start = ctx.t();
        kcp.apply_yaml(&example("agent-start.yaml").replace("quickstart-agent-agent1-start", "mixedprod-agent1-start").replace("einfra-quickstart-agent-agent1", "einfra-mixedprod-agent1")).await?;
        let r2 = requester::wait_log(&r.logs(), "requester_association_written", 5 * MIN).await?["subject"].as_str().unwrap_or_default().to_string();
        let peer = requester::wait_log(&r.logs(), "requester_agent_identity", 5 * MIN).await?["peer"].as_str().unwrap_or_default().to_string();
        let (identity, agent) = peer.split_once('@').context("peer")?;
        ctx.reading("association_after_agent_start_s", ctx.t() - t_start);
        let node = OwnerNode::start(&ctx, &r, ACCOUNT, "st-owner", std::slice::from_ref(&peer)).await?;
        node.wait_online(identity, 5 * MIN).await?;
        ctx.reading("online_after_agent_start_s", ctx.t() - t_start);
        ctx.reading("answer_s", node.prompt_uname(identity, 3 * MIN).await?);

        // A9: Ctrl-C and start again
        let again = r.restart().await?;
        ensure!(again["did"] == did.as_str(), "the DID changed across the restart");
        for c in ["vm1", "sbx1", "agent1"] {
            let cc = kcp.get("ComputeContract", &format!("einfra-mixedprod-{c}")).await?.context(c)?;
            ensure!(cc["status"]["phase"] == "Held", "{c} is {} after the restart", cc["status"]["phase"]);
        }
        ensure!(r.ssh("einfra-mixedprod-vm1", None, "cat /root/mixedprod").await?.contains("mixedprod-vm-ok"));
        ctx.reading("answer_after_restart_s", node.prompt_uname(identity, 3 * MIN).await?);

        // A10: delete
        let t_del = ctx.t();
        kcp.delete("EphemeralInfra", "mixedprod").await?;
        kcp.wait_gone("EphemeralInfra", "mixedprod", 5 * MIN).await?;
        ctx.reading("delete_s", ctx.t() - t_del);
        for c in ["vm1", "sbx1", "agent1"] {
            ensure!(kcp.get("ComputeContract", &format!("einfra-mixedprod-{c}")).await?.is_none(), "{c} left after the delete");
        }
        requester::wait_log(&r.logs(), "requester_contract_records_deleted", 2 * MIN).await?;
        ctx.reading("records_checked", ctx.records_gone(&r.logs(), &did).await?);
        for d in [r2.as_str(), agent] {
            eventually(&format!("{d} tombstoned"), 3 * MIN, Duration::from_secs(5), || async { Ok(ctx.at.tombstoned(d).await?.then_some(())) }).await?;
        }
        let mut empty = json!({});
        for (c, w) in winners.as_object().unwrap() {
            empty[c] = json!(ctx.host_empty(w.as_str().unwrap(), &format!("einfra-mixedprod-{c}")).await?);
        }
        ctx.reading("hosts_empty", empty);
        ctx.check_logs(&r.logs())?;
        Ok(())
    })
    .await;
}

/// 0013 C1-C6: run requester --with-gateway, cluster c1, k8s-cluster.yaml on a market node, scale to zero.
#[tokio::test(flavor = "multi_thread")]
async fn c_gateway_in_requester_kcp() {
    run("0013", "C", &[ACCOUNT], |ctx| async move {
        let did = ctx.account(ACCOUNT)?.did.clone();
        let r =
            Requester::start(&ctx, ACCOUNT, "mk", SignIn::from_env(), &["--with-gateway"]).await?;
        ensure!(
            !r.ready()?["gateway"].is_null(),
            "the ready file names no gateway"
        );
        let st = r.state.display().to_string();
        let (ok, out) = r
            .bin
            .run(&[
                "run",
                "requester",
                "cluster",
                "mk",
                "--cluster",
                "c1",
                "--state-dir",
                &st,
            ])
            .await?;
        ensure!(ok, "run requester cluster: {}", requester::tail(&out));
        let kc = eventually(
            "the c1 kubeconfig",
            5 * MIN,
            Duration::from_secs(5),
            || async {
                let (ok, out) = r
                    .bin
                    .run(&[
                        "run",
                        "requester",
                        "kubeconfig",
                        "mk",
                        "--cluster",
                        "c1",
                        "--state-dir",
                        &st,
                    ])
                    .await?;
                Ok(ok
                    .then(|| {
                        out.lines()
                            .map(str::trim)
                            .find(|l| l.starts_with('/'))
                            .map(String::from)
                    })
                    .flatten())
            },
        )
        .await?;
        let t = Tenant::connect(&kc).await?;
        let t_apply = ctx.t();
        let name = t.apply_deployment(&example("k8s-cluster.yaml")).await?;
        {
            let kc = kc.clone();
            ctx.defer("delete deployment quickstart-k8s", async move {
                Tenant::connect(&kc)
                    .await?
                    .delete_deployment("quickstart-k8s")
                    .await
            });
        }
        let pods = t.wait_ready("app=quickstart-k8s", 1, 15 * MIN).await?;
        ctx.reading("pod_ready_s", ctx.t() - t_apply);
        ensure!(t.logs(&pods[0].0).await?.contains("quickstart-k8s-ok"));
        ctx.reading("node", &pods[0].1);

        let t_del = ctx.t();
        t.delete_deployment(&name).await?;
        t.wait_nodes(0, 10 * MIN).await?;
        ctx.reading("nodes_zero_s", ctx.t() - t_del);
        // the infra mirror (computer.socialweb.infra/self, nodes-<hex>) goes with the node
        eventually(
            "the infra mirror records gone",
            3 * MIN,
            Duration::from_secs(10),
            || async {
                let infra: Vec<_> = requester::uris_in(&r.logs(), &did)
                    .into_iter()
                    .filter(|u| u.contains("/computer.socialweb.infra/"))
                    .collect();
                Ok(ctx.at.still_present(&infra).await?.is_empty().then_some(()))
            },
        )
        .await?;
        ctx.reading("records_checked", ctx.records_gone(&r.logs(), &did).await?);
        ctx.check_logs(&r.logs())?;
        Ok(())
    })
    .await;
}
