//! docs/acceptance/0011-atirc.md against the target's atirc server: section 2 (deploy gates), a token minted the
//! normal way (`atirc token`) posts and is refused after its revoke (section 3's revoke, from the client side).
//! Section 1 is the atirc repo's own gates; section 4: the VM agent is 0004, the ssh gateway agent 0002; the
//! server-side `revocations.ndjson` and lease death need the atirc host.

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

/// A minted token posts, its own message comes back on the stream; after `atirc revoke` a post is refused.
#[tokio::test(flavor = "multi_thread")]
async fn s3_token_mint_post_revoke() {
    run("0011", "s3-token", &["john"], |ctx| async move {
        let nick = format!(
            "st-tok{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)?
                .as_secs()
                % 100000
        );
        let obs = testkit::atirc::Observer::start(&ctx, "john", &nick).await?;
        let text = format!("st-0011 {nick}");
        ensure!(obs.publish(&text).await?, "pub with a fresh token failed");
        obs.wait_message(
            "its own post on the stream",
            std::time::Duration::from_secs(60),
            |m| m["text"] == text.as_str(),
        )
        .await?;
        obs.revoke().await?;
        ensure!(
            !obs.publish("after revoke").await?,
            "a revoked token still posts"
        );
        Ok(())
    })
    .await;
}
