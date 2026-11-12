//! docs/acceptance/0002-ssh-gateway.md: a transient `ssh <handle>@<gateway> '<command>'` rents a market VM as that user
//! (the last-pass form): sign in on the ssh web page, add a temporary key on /keys, ssh on port 22, remove the key.
//! The `atirc agent run` form needs an atirc token to watch the nick join: not ported.

use std::process::Stdio;
use std::time::Duration;

use testkit::requester::oauth_signin_with;
use testkit::*;

const ACCOUNT: &str = "alice";

fn cookie_header(jar: &std::path::Path) -> Result<String> {
    let v: Value = serde_json::from_slice(&std::fs::read(jar)?)?;
    Ok(v.as_array()
        .into_iter()
        .flatten()
        .map(|c| {
            format!(
                "{}={}",
                c["name"].as_str().unwrap_or_default(),
                c["value"].as_str().unwrap_or_default()
            )
        })
        .collect::<Vec<_>>()
        .join("; "))
}

#[tokio::test(flavor = "multi_thread")]
async fn transient_command_as_the_user() {
    run("0002", "transient", &[ACCOUNT], |ctx| async move {
        let a = ctx.account(ACCOUNT)?;
        let host = ctx.target.gateway.ssh.clone();
        let web = format!("https://ssh.{host}");
        let key = ctx.dir.join("st_0002_key");
        let ok = tokio::process::Command::new("ssh-keygen")
            .args(["-q", "-t", "ed25519", "-N", "", "-C", "st-0002", "-f"])
            .arg(&key)
            .stdout(Stdio::null())
            .status()
            .await?
            .success();
        ensure!(ok, "ssh-keygen failed");
        let public = std::fs::read_to_string(key.with_extension("pub"))?
            .trim()
            .to_string();
        let jar = ctx.dir.join("ssh-web-jar.json");
        oauth_signin_with(
            a,
            &format!("{web}/"),
            &["--start-api", "--cookie-file", &jar.display().to_string()],
        )
        .await?;
        let cookie = cookie_header(&jar)?;

        let added: Value = ctx
            .at
            .http
            .post(format!("{web}/keys"))
            .header("cookie", &cookie)
            .json(&json!({ "publicKey": public }))
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        let rkey = added["rkey"]
            .as_str()
            .context("POST /keys answered no rkey")?
            .to_string();
        {
            let (http, web, cookie, rkey, jar) = (
                ctx.at.http.clone(),
                web.clone(),
                cookie.clone(),
                rkey.clone(),
                jar.clone(),
            );
            ctx.defer("remove the temporary key", async move {
                http.delete(format!("{web}/keys/{rkey}"))
                    .header("cookie", cookie)
                    .send()
                    .await?
                    .error_for_status()?;
                std::fs::remove_file(jar)?;
                Ok(())
            });
        }

        let t = ctx.t();
        let port = ctx.target.gateway.ssh_port.unwrap_or(22).to_string();
        let known = ctx.dir.join("known_hosts");
        let out = tokio::time::timeout(
            Duration::from_secs(600),
            tokio::process::Command::new("ssh")
                .args(["-F", "/dev/null", "-p", &port, "-i"])
                .arg(&key)
                .args([
                    "-o",
                    "IdentitiesOnly=yes",
                    "-o",
                    "BatchMode=yes",
                    "-o",
                    "StrictHostKeyChecking=accept-new",
                ])
                .arg("-o")
                .arg(format!("UserKnownHostsFile={}", known.display()))
                .arg(format!("{}@{host}", a.handle))
                .arg("uname -a; id; hostname")
                .stdin(Stdio::null())
                .output(),
        )
        .await
        .context("ssh did not end in 10 min")??;
        let text = String::from_utf8_lossy(&out.stdout).to_string();
        ensure!(
            out.status.success() && text.contains("Linux") && text.contains("uid="),
            "ssh exit {}: {}",
            out.status,
            requester::tail(&text)
        );
        ctx.reading("ssh_s", ctx.t() - t);
        Ok(())
    })
    .await;
}
