//! docs/acceptance/0002-ssh-gateway.md: `ssh <handle>@<gateway>` rents a market VM as that user (discovery). Sign in on
//! the ssh web page, a temporary key on /keys, `ssh -F /dev/null -p 22`, the key removed after. Forms: a transient
//! command; `atirc agent run <name>` with the agent config on stdin (the agent joins the owner channel and answers
//! an observer's prompt, as the guest user); the same held with `LC_SESSION`. The revoke-before-Released evidence is
//! in the gateway's log (not readable from the kit).

use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use testkit::atirc::Observer;
use testkit::requester::oauth_signin_with;
use testkit::target::{check_private, expand};
use testkit::*;

const ACCOUNT: &str = "alice";
const MIN: Duration = Duration::from_secs(60);

/// A signed-in ssh web session and a temporary key on /keys (removed by a cleanup).
struct Ssh {
    key: std::path::PathBuf,
    known: std::path::PathBuf,
    port: String,
    dest: String,
}

impl Ssh {
    async fn new(ctx: &Arc<Ctx>) -> Result<Self> {
        let a = ctx.account(ACCOUNT)?;
        let host = ctx.target.gateway.ssh.clone();
        let web = format!("https://ssh.{host}");
        let key = ctx.dir.join("st_0002_key");
        if !key.exists() {
            let ok = tokio::process::Command::new("ssh-keygen")
                .args(["-q", "-t", "ed25519", "-N", "", "-C", "st-0002", "-f"])
                .arg(&key)
                .stdout(Stdio::null())
                .status()
                .await?
                .success();
            ensure!(ok, "ssh-keygen failed");
        }
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
        let v: Value = serde_json::from_slice(&std::fs::read(&jar)?)?;
        std::fs::remove_file(&jar)?;
        let cookie = v
            .as_array()
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
            .join("; ");
        let added: Value = ctx
            .at
            .http
            .post(format!("{web}/keys"))
            .header("cookie", &cookie)
            .header("origin", &web)
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
        let (http, web2) = (ctx.at.http.clone(), web.clone());
        ctx.defer("remove the temporary key", async move {
            http.delete(format!("{web2}/keys/{rkey}"))
                .header("cookie", cookie)
                .header("origin", web2.as_str())
                .send()
                .await?
                .error_for_status()?;
            Ok(())
        });
        Ok(Self {
            key,
            known: ctx.dir.join("known_hosts"),
            port: ctx.target.gateway.ssh_port.unwrap_or(22).to_string(),
            dest: format!("{}@{host}", a.handle),
        })
    }

    fn cmd(&self, env: &[(&str, &str)], remote: &str) -> tokio::process::Command {
        let mut c = tokio::process::Command::new("ssh");
        c.args(["-F", "/dev/null", "-p", &self.port, "-i"])
            .arg(&self.key);
        c.args([
            "-o",
            "IdentitiesOnly=yes",
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=accept-new",
        ]);
        c.arg("-o")
            .arg(format!("UserKnownHostsFile={}", self.known.display()));
        for (k, v) in env {
            c.arg("-o").arg(format!("SetEnv={k}={v}"));
        }
        c.arg(&self.dest).arg(remote).kill_on_drop(true);
        c
    }
}

fn stamp() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        % 100000
}

#[tokio::test(flavor = "multi_thread")]
async fn transient_command_as_the_user() {
    run("0002", "transient", &[ACCOUNT], |ctx| async move {
        let ssh = Ssh::new(&ctx).await?;
        let t = ctx.t();
        let out = tokio::time::timeout(
            10 * MIN,
            ssh.cmd(&[], "uname -a; id; hostname")
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

/// `atirc agent run <name> < <agent config>`, transient and held (`LC_SESSION`): JOIN, an answer as the guest user.
#[tokio::test(flavor = "multi_thread")]
async fn agent_run_transient_and_held() {
    run("0002", "agent", &[ACCOUNT], |ctx| async move {
        let cfg = expand(
            ctx.target
                .agent_config
                .as_deref()
                .context("target agentConfig")?,
        );
        check_private(&cfg)?;
        let ssh = Ssh::new(&ctx).await?;
        let obs = Observer::start(&ctx, ACCOUNT, &format!("st-obs{}", stamp())).await?;
        let mut out = json!({});
        for (form, name) in [
            ("transient", format!("st{}", stamp())),
            ("held", format!("sth{}", stamp())),
        ] {
            let env: Vec<(&str, &str)> = if form == "held" {
                vec![("LC_SESSION", name.as_str())]
            } else {
                vec![]
            };
            let t = ctx.t();
            let mut child = ssh
                .cmd(&env, &format!("atirc agent run {name}"))
                .stdin(std::fs::File::open(&cfg)?)
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()?;
            let from_agent = |m: &Value| {
                m["nick"] == name.as_str()
                    || m["identity"]
                        .as_str()
                        .is_some_and(|i| i.starts_with(&format!("{name}.")))
            };
            obs.wait_message("the agent joins", 10 * MIN, from_agent)
                .await?;
            let joined = ctx.t() - t;
            let seen = obs.messages().len();
            let tp = ctx.t();
            ensure!(
                obs.publish(&format!("{name}: uname -m; id -un")).await?,
                "observer pub failed"
            );
            eventually(
                "the agent's answer",
                3 * MIN,
                Duration::from_secs(1),
                || async {
                    Ok(obs.messages().into_iter().skip(seen).find(|m| {
                        from_agent(m)
                            && m["text"]
                                .as_str()
                                .is_some_and(|x| x.contains("x86_64") && x.contains(name.as_str()))
                    }))
                },
            )
            .await?;
            let answered = ctx.t() - tp;
            child.kill().await?; // disconnect: the gateway revokes, then releases
            out[form] = json!({ "join_s": joined, "answer_s": answered });
        }
        ctx.reading("agent", out);
        Ok(())
    })
    .await;
}
