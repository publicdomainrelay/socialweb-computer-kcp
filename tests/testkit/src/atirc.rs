//! The owner's atirc mesh node: an AtircNode in the orchestrator's kcp (a HostProcess running the pinned
//! `atirc mesh run` as the account), and `atirc mesh pub|peers|sub` against it.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use serde_json::{Value, json};

use crate::kcp::Kcp;
use crate::requester::Requester;
use crate::{Ctx, eventually};

pub struct OwnerNode {
    pub name: String,
    pub handle: String,
    config_dir: String,
    bin: PathBuf,
    sub_log: PathBuf,
    _sub: tokio::process::Child,
}

/// The owner channel of a handle: #aliceoa_bsky_social.
pub fn channel_of(handle: &str) -> String {
    format!("#{}", handle.replace(['.', '-'], "_"))
}

impl OwnerNode {
    /// Apply AtircNode `name` as `account` with `peers`, wait Running, start a `mesh sub --cursor 0` into a file.
    /// Deleted by a cleanup (with its password env file).
    pub async fn start(
        ctx: &Arc<Ctx>,
        r: &Arc<Requester>,
        account: &str,
        name: &str,
        peers: &[String],
    ) -> Result<Self> {
        let a = ctx.account(account)?;
        let env = ctx.dir.join(format!("{name}-atp.env"));
        {
            use std::io::Write;
            use std::os::unix::fs::OpenOptionsExt;
            let mut f = std::fs::OpenOptions::new()
                .create(true)
                .truncate(true)
                .write(true)
                .mode(0o600)
                .open(&env)?;
            writeln!(f, "ATP_PASSWORD={}", a.password()?.expose())?;
        }
        let kcp = r.kcp().await?;
        let (del_r, del_name, del_env) = (r.clone(), name.to_string(), env.clone());
        ctx.defer(&format!("delete AtircNode {name}"), async move {
            let _ = std::fs::remove_file(&del_env);
            del_r
                .kcp()
                .await?
                .delete("AtircNode", &del_name)
                .await
                .map(|_| ())
        });
        kcp.apply(&json!({
            "apiVersion": "socialweb.computer/v1alpha1", "kind": "AtircNode", "metadata": { "name": name },
            "spec": { "handle": a.handle, "nick": name, "channels": [channel_of(&a.handle)], "peers": peers, "ircPort": 0, "passwordEnvFile": env }
        }))
        .await?;
        let o = kcp
            .wait_phase("AtircNode", name, "Running", Duration::from_secs(180))
            .await?;
        let config_dir = o["status"]["configDir"]
            .as_str()
            .context("AtircNode without status.configDir")?
            .to_string();
        let pid = hostprocess_pid(&kcp, &o).await?;
        let bin = std::fs::read_link(format!("/proc/{pid}/exe"))
            .context("the AtircNode's atirc runs on this host")?;
        let sub_log = ctx.dir.join(format!("{name}-sub.ndjson"));
        let sub = tokio::process::Command::new(&bin)
            .args(["mesh", "sub", "--cursor", "0"])
            .env("ATIRC_CONFIG_DIR", &config_dir)
            .env("ATP_USERNAME", &a.handle)
            .stdout(std::fs::File::create(&sub_log)?)
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true)
            .spawn()?;
        Ok(Self {
            name: name.into(),
            handle: a.handle.clone(),
            config_dir,
            bin,
            sub_log,
            _sub: sub,
        })
    }

    pub async fn mesh(&self, args: &[&str]) -> Result<String> {
        let out = tokio::process::Command::new(&self.bin)
            .arg("mesh")
            .args(args)
            .env("ATIRC_CONFIG_DIR", &self.config_dir)
            .env("ATP_USERNAME", &self.handle)
            .output()
            .await?;
        Ok(format!(
            "{}{}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        ))
    }

    /// The messages the node has seen (ndjson of `mesh sub`).
    pub fn messages(&self) -> Vec<Value> {
        std::fs::read_to_string(&self.sub_log)
            .unwrap_or_default()
            .lines()
            .filter_map(|l| serde_json::from_str(l).ok())
            .collect()
    }

    /// Wait for `<nick> online` from the agent `identity` (<name>.<R2 handle>).
    pub async fn wait_online(&self, identity: &str, timeout: Duration) -> Result<()> {
        self.wait_message("the agent online", timeout, |m| {
            m["identity"] == identity && m["text"].as_str().is_some_and(|t| t.contains("online"))
        })
        .await
        .map(|_| ())
    }

    /// Prompt the agent `<nick>: uname -m` on the mesh; the seconds until it answers with the machine.
    pub async fn prompt_uname(&self, identity: &str, timeout: Duration) -> Result<f64> {
        let nick = identity.split('.').next().unwrap_or(identity);
        let seen = self.messages().len();
        let t = std::time::Instant::now();
        self.mesh(&["pub", &format!("{nick}: uname -m")]).await?;
        eventually(
            "the agent's answer",
            timeout,
            Duration::from_secs(1),
            || async {
                Ok(self.messages().into_iter().skip(seen).find(|m| {
                    m["identity"] == identity
                        && m["text"]
                            .as_str()
                            .is_some_and(|t| t.contains("x86_64") || t.contains("aarch64"))
                }))
            },
        )
        .await?;
        Ok(t.elapsed().as_secs_f64())
    }

    pub async fn wait_message(
        &self,
        what: &str,
        timeout: Duration,
        pred: impl Fn(&Value) -> bool,
    ) -> Result<Value> {
        eventually(what, timeout, Duration::from_secs(1), || async {
            Ok(self.messages().into_iter().find(|m| pred(m)))
        })
        .await
    }
}

async fn hostprocess_pid(kcp: &Kcp, node: &Value) -> Result<i64> {
    if let Some(pid) = node["status"]["runtime"]["pid"].as_i64() {
        return Ok(pid);
    }
    let hp = node["status"]["hostProcess"]
        .as_str()
        .context("AtircNode without status.hostProcess")?;
    let o = kcp
        .get("HostProcess", hp)
        .await?
        .context("the AtircNode's HostProcess is gone")?;
    o["status"]["runtime"]["pid"]
        .as_i64()
        .context("HostProcess without status.runtime.pid")
}

/// The pinned atirc (the requester's staged asset, newest first): token, sub, pub, revoke.
pub fn atirc_bin() -> Result<PathBuf> {
    let runtime = PathBuf::from(std::env::var("HOME")?).join(".cache/socialweb-computer/runtime");
    let mut found: Vec<_> = std::fs::read_dir(&runtime)?
        .flatten()
        .map(|e| e.path().join("atirc-x86_64"))
        .filter(|p| p.is_file())
        .collect();
    found.sort_by_key(|p| std::fs::metadata(p).and_then(|m| m.modified()).ok());
    found
        .pop()
        .context("no staged atirc-x86_64 (run any requester once)")
}

/// An observer on the atirc server: a token minted as the account (`atirc token <nick>`, the normal flow), a
/// `sub` of the owner channel into a file, `pub` as the observer. A cleanup revokes the token.
pub struct Observer {
    bin: PathBuf,
    endpoint: String,
    token: String,
    log: PathBuf,
    _sub: tokio::process::Child,
}

impl Observer {
    pub async fn start(ctx: &Arc<Ctx>, account: &str, nick: &str) -> Result<Arc<Self>> {
        let a = ctx.account(account)?;
        let endpoint = ctx.target.atirc.clone().context("target atirc")?;
        let bin = atirc_bin()?;
        let out = tokio::process::Command::new(&bin)
            .args(["token", nick, "--endpoint", &endpoint])
            .env("ATP_USERNAME", &a.handle)
            .env("ATP_PASSWORD", a.password()?.expose())
            .stdin(std::process::Stdio::null())
            .output()
            .await?;
        anyhow::ensure!(out.status.success(), "atirc token {nick} failed");
        let token = String::from_utf8(out.stdout)?.trim().to_string();
        anyhow::ensure!(!token.is_empty(), "atirc token printed no token");
        let log = ctx.dir.join(format!("observer-{nick}.ndjson"));
        let sub = tokio::process::Command::new(&bin)
            .args([
                "sub",
                "--full",
                "--cursor",
                "-1",
                "-c",
                &channel_of(&a.handle),
            ])
            .env("ATIRC_TOKEN", &token)
            .env("ATIRC_ENDPOINT", &endpoint)
            .stdin(std::process::Stdio::null())
            .stdout(std::fs::File::create(&log)?)
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true)
            .spawn()?;
        let o = Arc::new(Self {
            bin,
            endpoint,
            token,
            log,
            _sub: sub,
        });
        let r = o.clone();
        ctx.defer(&format!("revoke the observer token {nick}"), async move {
            r.revoke().await
        });
        Ok(o)
    }

    async fn run(&self, args: &[&str]) -> Result<bool> {
        Ok(tokio::process::Command::new(&self.bin)
            .args(args)
            .env("ATIRC_TOKEN", &self.token)
            .env("ATIRC_ENDPOINT", &self.endpoint)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .await?
            .success())
    }

    pub async fn publish(&self, text: &str) -> Result<bool> {
        self.run(&["pub", text]).await
    }

    /// `atirc revoke` of the token's own subhandle; idempotent.
    pub async fn revoke(&self) -> Result<()> {
        self.run(&["revoke"]).await?;
        Ok(())
    }

    pub fn messages(&self) -> Vec<Value> {
        std::fs::read_to_string(&self.log)
            .unwrap_or_default()
            .lines()
            .filter_map(|l| serde_json::from_str(l).ok())
            .collect()
    }

    pub async fn wait_message(
        &self,
        what: &str,
        timeout: Duration,
        pred: impl Fn(&Value) -> bool,
    ) -> Result<Value> {
        eventually(what, timeout, Duration::from_secs(1), || async {
            Ok(self.messages().into_iter().find(|m| pred(m)))
        })
        .await
    }
}
