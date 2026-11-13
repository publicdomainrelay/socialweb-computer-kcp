//! docs/acceptance/0012-infra-quickstart.md on the target's market (the runbook's own network is the staging target):
//! every examples/infra file passes a server dry run in the requester, vm.yaml ssh, deno-sandbox.yaml stdout, a
//! `run requester` restart keeps both Held, delete releases them. k8s-cluster.yaml and the agent join are 0013 C and A.

use std::time::Duration;

use testkit::*;

const MIN: Duration = Duration::from_secs(60);
const ACCOUNT: &str = "alice";
const EXAMPLES: [&str; 6] = [
    "vm.yaml",
    "deno-sandbox.yaml",
    "agent-vm.yaml",
    "mixed-prod.yaml",
    "mixed.yaml",
    "agent-start.yaml",
];

#[tokio::test(flavor = "multi_thread")]
async fn examples_vm_deno_restart_release() {
    run("0012", "examples", &[ACCOUNT], |ctx| async move {
        let did = ctx.account(ACCOUNT)?.did.clone();
        let r = Requester::start(&ctx, ACCOUNT, "qs", SignIn::from_env(), &[]).await?;
        let kcp = r.kcp().await?;
        let key = std::fs::read_to_string(
            r.ready()?["sshPublicKey"]
                .as_str()
                .context("no sshPublicKey")?,
        )?;
        let fill = |s: String| {
            s.replace(
                "ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop",
                key.trim(),
            )
        };
        for f in EXAMPLES {
            kcp.apply_yaml_dry(&fill(example(f)))
                .await
                .with_context(|| format!("{f}: server dry run"))?;
        }
        for (kind, name) in [
            ("Infra", "quickstart-vm"),
            ("EphemeralInfra", "quickstart-deno"),
        ] {
            let rr = r.clone();
            ctx.defer(&format!("delete {kind} {name}"), async move {
                let k = rr.kcp().await?;
                k.delete(kind, name).await?;
                k.wait_gone(kind, name, 5 * MIN).await
            });
        }
        let t = ctx.t();
        let vm = fill(example("vm.yaml"));
        ensure_keys(&vm, &key)?;
        kcp.apply_yaml(&vm).await?;
        kcp.apply_yaml(&example("deno-sandbox.yaml")).await?;
        kcp.wait_phase("Infra", "quickstart-vm", "Deployed", 10 * MIN)
            .await?;
        ctx.reading("vm_deployed_s", ctx.t() - t);
        ensure!(
            r.ssh("infra-quickstart-vm-vm1", None, "cat /root/quickstart")
                .await?
                .contains("quickstart-vm-ok")
        );
        let sbx = kcp
            .wait(
                "ComputeContract",
                "einfra-quickstart-deno-sbx1",
                10 * MIN,
                "sandbox stdout",
                |o| o.is_some_and(|o| o["status"]["contract"]["sandbox"]["stdout"].is_string()),
            )
            .await?
            .unwrap();
        let sb = &sbx["status"]["contract"]["sandbox"];
        ensure!(
            sb["exitCode"] == 0
                && sb["stdout"]
                    .as_str()
                    .is_some_and(|s| s.contains("quickstart-deno-ok sum=10")),
            "sandbox {sb}"
        );

        let again = r.restart().await?;
        ensure!(
            again["did"] == did.as_str(),
            "the DID changed across the restart"
        );
        let cc = kcp
            .get("ComputeContract", "infra-quickstart-vm-vm1")
            .await?
            .context("vm1")?;
        ensure!(
            cc["status"]["phase"] == "Held",
            "vm1 is {} after the restart",
            cc["status"]["phase"]
        );
        ensure!(
            r.ssh("infra-quickstart-vm-vm1", None, "cat /root/quickstart")
                .await?
                .contains("quickstart-vm-ok")
        );

        for (kind, name) in [
            ("Infra", "quickstart-vm"),
            ("EphemeralInfra", "quickstart-deno"),
        ] {
            kcp.delete(kind, name).await?;
            kcp.wait_gone(kind, name, 5 * MIN).await?;
        }
        requester::wait_log(&r.logs(), "requester_contract_records_deleted", 2 * MIN).await?;
        ctx.reading("records_checked", ctx.records_gone(&r.logs(), &did).await?);
        ctx.check_logs(&r.logs())?;
        Ok(())
    })
    .await;
}
