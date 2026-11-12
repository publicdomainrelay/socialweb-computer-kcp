//! docs/acceptance/0011-atirc.md section 2 (deploy gates) against the target's atirc server. Section 1 is the atirc
//! repo's own gates; section 4's token-less VM agent is 0004; the minting paths (sections 3, lease death) need an
//! atirc token: not ported.

use testkit::*;

#[tokio::test(flavor = "multi_thread")]
async fn s2_deploy_gates() {
    run("0011", "s2", &[], |ctx| async move {
        let Some(base) = ctx.target.atirc.clone() else {
            return ctx.skip("target atirc not set");
        };
        let http = &ctx.at.http;
        let bin = http
            .get(&base)
            .header("user-agent", "curl/8")
            .send()
            .await?;
        ensure!(bin.status() == 200, "GET / as curl: {}", bin.status());
        let chat = http.get(format!("{base}/chat")).send().await?;
        ensure!(chat.status() == 200, "GET /chat: {}", chat.status());
        let page = http
            .get(&base)
            .header("user-agent", "Mozilla/5.0")
            .send()
            .await?
            .text()
            .await?;
        ensure!(page.contains("/chat"), "the root page does not link /chat");
        let revoke = http
            .post(format!("{base}/xrpc/chat.atirc.revoke"))
            .json(&json!({}))
            .send()
            .await?;
        ensure!(
            revoke.status() == 401,
            "unauthenticated revoke: {}",
            revoke.status()
        );
        Ok(())
    })
    .await;
}
