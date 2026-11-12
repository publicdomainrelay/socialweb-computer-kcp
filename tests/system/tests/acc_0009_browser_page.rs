//! docs/acceptance/0009-browser-page.md A3 / A6: the page's requester module in headless Chrome (the requester page repo's
//! scripts/page-round.mjs, an app-password session in place of the OAuth popup) rents a VM over each bidder's webrtc
//! endpoint, opens a shell over the guest datachannel, releases and deletes what it wrote. A6 is A3 per bidder, so this
//! runs per target bidder. Needs target pageRepo (skipped otherwise). A2 (the OAuth form) and A7 (/chat): not ported.

use std::time::Duration;

use testkit::target::expand;
use testkit::*;

#[tokio::test(flavor = "multi_thread")]
async fn a3_vm_over_webrtc_per_bidder() {
    run("0009", "A3", &["alice"], |ctx| async move {
        let Some(repo) = ctx.target.page_repo.clone() else {
            return ctx
                .skip("target pageRepo (atlibs-socialweb-computer-requester checkout) not set");
        };
        let a = ctx.account("alice")?;
        let mut out = json!({});
        for b in &ctx.target.bidders {
            let log = ctx.dir.join(format!("page-round-{}.log", b.name));
            let f = std::fs::File::create(&log)?;
            let st = tokio::time::timeout(
                Duration::from_secs(900),
                tokio::process::Command::new("node")
                    .current_dir(expand(&repo))
                    .args(["scripts/page-round.mjs", &b.did, "uname -a; id"])
                    .env("ALICE_ATPROTO_HANDLE", &a.handle)
                    .env("ALICE_ATPROTO_PASSWORD", a.password()?.expose())
                    .stdin(std::process::Stdio::null())
                    .stdout(f.try_clone()?)
                    .stderr(f)
                    .status(),
            )
            .await
            .context("page-round did not end in 15 min")??;
            ctx.check_logs(std::slice::from_ref(&log))?;
            let text = std::fs::read_to_string(&log)?;
            let line = |k: &str| {
                text.lines()
                    .find(|l| l.contains(k))
                    .unwrap_or_default()
                    .to_string()
            };
            ensure!(
                st.success() && line("contract").contains("ok"),
                "{}: {}",
                b.name,
                line("contract")
            );
            ensure!(
                line("shell").contains("Linux") && line("shell").contains("uid=0"),
                "{}: shell {}",
                b.name,
                line("shell")
            );
            ensure!(
                line("https submit* calls").trim_end().ends_with(" 0"),
                "{}: {}",
                b.name,
                line("https submit* calls")
            );
            ensure!(
                !line("deleted, now").contains(",200]"),
                "{}: a record the run wrote is still there: {}",
                b.name,
                line("deleted, now")
            );
            ensure!(
                line("ingress.offer records left")
                    .trim_end()
                    .ends_with(" 0"),
                "{}: {}",
                b.name,
                line("ingress.offer records left")
            );
            out[&b.name] = json!(line("contract"));
        }
        ctx.reading("per_bidder", out);
        Ok(())
    })
    .await;
}
