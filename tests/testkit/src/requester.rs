//! The orchestrator: a real Go requester (`socialweb-computer run requester` / `run request`) of a release,
//! a commit or a local build, signed in as a target account. Tests drive it through its kcp and its CLI.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::{Duration, Instant};

use anyhow::{Context, Result, anyhow, bail, ensure};
use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::kcp::Kcp;
use crate::target::{Account, Target, expand};
use crate::{Ctx, eventually, home_tmp, repo};

const BUCKET: &str = "https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com";

#[derive(Clone, Debug)]
pub struct Binary {
    pub path: PathBuf,
    /// `socialweb-computer version` (commit, dirty state)
    pub version: String,
}

fn arch() -> &'static str {
    if cfg!(target_arch = "aarch64") {
        "linux-arm64"
    } else {
        "linux-amd64"
    }
}

const ORIGIN: &str = "https://github.com/publicdomainrelay/socialweb-computer-kcp";

/// The newest v* tag on GitHub (v0.0.0-rcN sorts by N).
fn newest_tag() -> Result<String> {
    let out = std::process::Command::new("git")
        .args(["ls-remote", "--tags", "--refs", ORIGIN, "v*"])
        .output()?;
    ensure!(
        out.status.success(),
        "git ls-remote origin: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    let key = |t: &str| {
        t.rsplit_once("-rc")
            .and_then(|(_, n)| n.parse::<u64>().ok())
            .unwrap_or(0)
    };
    String::from_utf8(out.stdout)?
        .lines()
        .filter_map(|l| l.split_once("refs/tags/").map(|(_, t)| t.to_string()))
        .max_by_key(|t| key(t))
        .context("origin has no v* tag")
}

impl Binary {
    /// "release" | "release:<tag>" | "sha:<full commit>" | "path:<file>"; downloads are checked against SHA256SUMS
    /// and cached under ~/.tmp/swc-system-tests/bin.
    pub async fn resolve(spec: &str) -> Result<Self> {
        let prefix = match spec.split_once(':') {
            _ if spec == "release" => format!("releases/{}", newest_tag()?),
            Some(("release", tag)) => format!("releases/{tag}"),
            Some(("sha", sha)) => format!("main/{sha}"),
            Some(("path", p)) => return Self::at(expand(p)),
            _ => bail!(
                "requester {spec:?}: want release, release:<tag>, sha:<commit> or path:<file>"
            ),
        };
        let file = format!("socialweb-computer-{}", arch());
        let path = home_tmp("bin").join(prefix.replace('/', "-")).join(&file);
        if !path.exists() {
            let http = reqwest::Client::new();
            let base = format!("{BUCKET}/{prefix}/binaries");
            let sums = http
                .get(format!("{base}/SHA256SUMS"))
                .send()
                .await?
                .error_for_status()?
                .text()
                .await?;
            let want = sums
                .lines()
                .find_map(|l| l.strip_suffix(&format!("  {file}")))
                .with_context(|| format!("{base}/SHA256SUMS lists no {file}"))?;
            let bytes = http
                .get(format!("{base}/{file}"))
                .send()
                .await?
                .error_for_status()?
                .bytes()
                .await?;
            let got = format!("{:x}", Sha256::digest(&bytes));
            ensure!(
                got == want,
                "{base}/{file}: sha256 {got}, SHA256SUMS says {want}"
            );
            std::fs::create_dir_all(path.parent().unwrap())?;
            let tmp = path.with_extension("tmp");
            std::fs::write(&tmp, &bytes)?;
            std::fs::set_permissions(&tmp, std::os::unix::fs::PermissionsExt::from_mode(0o755))?;
            std::fs::rename(&tmp, &path)?;
        }
        Self::at(path)
    }

    fn at(path: PathBuf) -> Result<Self> {
        let out = std::process::Command::new(&path)
            .arg("version")
            .output()
            .with_context(|| path.display().to_string())?;
        ensure!(out.status.success(), "{} version failed", path.display());
        let v = String::from_utf8_lossy(&out.stdout)
            .lines()
            .next()
            .unwrap_or_default()
            .trim_start_matches("socialweb-computer ")
            .to_string();
        Ok(Self { path, version: v })
    }

    pub fn cmd(&self) -> tokio::process::Command {
        let mut c = tokio::process::Command::new(&self.path);
        c.stdin(Stdio::null());
        c
    }

    /// Run to completion; (exit ok, stdout + stderr). The output never holds a secret of ours (no secret goes in).
    pub async fn run(&self, args: &[&str]) -> Result<(bool, String)> {
        let out = self.cmd().args(args).output().await?;
        let mut text = String::from_utf8_lossy(&out.stdout).to_string();
        text.push_str(&String::from_utf8_lossy(&out.stderr));
        Ok((out.status.success(), text))
    }
}

/// How the requester signs in as its account.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum SignIn {
    /// pdsAccount: the app password in ATPROTO_PASSWORD of the one command
    AppPassword,
    /// OAuth QR, driven by scripts/oauth-signin.ts (headless Chrome) from the creds file
    OAuthQr,
}

impl SignIn {
    /// SWC_SIGNIN=oauth-qr: OAuth QR (headless Chrome); default: the app password.
    pub fn from_env() -> Self {
        if std::env::var("SWC_SIGNIN").is_ok_and(|v| v == "oauth-qr") {
            SignIn::OAuthQr
        } else {
            SignIn::AppPassword
        }
    }
}

pub fn identity_args(t: &Target, a: &Account, mode: SignIn) -> Vec<String> {
    match mode {
        SignIn::AppPassword => vec![
            "--atproto-identity-mode".into(),
            "pdsAccount".into(),
            "--atproto-handle".into(),
            a.handle.clone(),
            "--atproto-pds-url".into(),
            t.pds.clone(),
        ],
        SignIn::OAuthQr => vec!["--atproto-oauth-qr".into()],
    }
}

/// Sign a QR sign-in URL in with the creds file (the URL never reaches a log of ours).
pub async fn oauth_signin(a: &Account, url: &str) -> Result<()> {
    oauth_signin_with(a, url, &[]).await
}

/// `oauth_signin` with more oauth-signin.ts options (`--start-api`, `--cookie-file <owner-only file>`).
pub async fn oauth_signin_with(a: &Account, url: &str, extra: &[&str]) -> Result<()> {
    let prefix = a
        .oauth_account
        .as_deref()
        .context("the account has no oauthAccount (creds prefix) for OAuth")?;
    let out = tokio::process::Command::new("deno")
        .current_dir(repo())
        .args([
            "run",
            "-A",
            "--no-lock",
            "scripts/oauth-signin.ts",
            "--creds",
        ])
        .arg(expand(&a.creds))
        .args(["--account", prefix, "--url", url])
        .args(extra)
        .stdout(Stdio::null())
        .stdin(Stdio::null())
        .output()
        .await?;
    ensure!(
        out.status.success(),
        "oauth-signin.ts exited {}",
        out.status
    );
    Ok(())
}

/// The JSON lines of a log whose msg is `msg`.
pub fn log_lines(path: &Path, msg: &str) -> Vec<Value> {
    std::fs::read_to_string(path)
        .unwrap_or_default()
        .lines()
        .filter(|l| l.contains(msg))
        .filter_map(|l| serde_json::from_str::<Value>(l).ok())
        .filter(|v| v["msg"] == msg)
        .collect()
}

pub async fn wait_log(paths: &[PathBuf], msg: &str, timeout: Duration) -> Result<Value> {
    eventually(
        &format!("log line {msg}"),
        timeout,
        Duration::from_secs(1),
        || async {
            Ok(paths
                .iter()
                .find_map(|p| log_lines(p, msg).into_iter().next()))
        },
    )
    .await
}

/// Every at://<did>/... URI the logs name (the records a run wrote in that repo).
pub fn uris_in(paths: &[PathBuf], did: &str) -> Vec<String> {
    let needle = format!("at://{did}/");
    let mut out = vec![];
    for p in paths {
        let text = std::fs::read_to_string(p).unwrap_or_default();
        for (i, _) in text.match_indices(&needle) {
            let uri: String = text[i..]
                .chars()
                .take_while(|c| !matches!(c, '"' | ' ' | '\\' | ',' | ')' | '\n'))
                .collect();
            if uri.split('/').count() == 5 && !out.contains(&uri) {
                out.push(uri);
            }
        }
    }
    out
}

/// Records that stay by rule after a release: vouches (Retain), the signer key, the operator associations of bidders.
pub fn kept_by_rule(uri: &str) -> bool {
    ["/sh.tangled.graph.vouch/", "badgeBlueKeys", "/app.bsky."]
        .iter()
        .any(|k| uri.contains(k))
}

/// A long-lived `run requester` (the orchestrator): its kcp, its ready file, its CLI helpers.
pub struct Requester {
    pub bin: Binary,
    pub name: String,
    pub state: PathBuf,
    pub log: PathBuf,
    args: Vec<String>,
    password: Option<String>,
    child: tokio::sync::Mutex<Option<tokio::process::Child>>,
}

impl Requester {
    /// Start `run requester --name <name>` in a new owner-only state dir under the run's scratch dir, sign in,
    /// wait for the ready file. Stopped (Ctrl-C) by a cleanup.
    pub async fn start(
        ctx: &std::sync::Arc<Ctx>,
        account: &str,
        name: &str,
        mode: SignIn,
        extra: &[&str],
    ) -> Result<std::sync::Arc<Self>> {
        let a = ctx.account(account)?;
        let bin = Binary::resolve(&ctx.target.requester).await?;
        let state = ctx.dir.join(format!("requester-{name}"));
        std::fs::create_dir_all(&state)?;
        std::fs::set_permissions(&state, std::os::unix::fs::PermissionsExt::from_mode(0o700))?;
        let mut args: Vec<String> = ["run", "requester", "--name", name, "--state-dir"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        args.push(state.display().to_string());
        args.extend(extra.iter().map(|s| s.to_string()));
        args.push("--".into());
        args.extend(identity_args(&ctx.target, a, mode));
        let r = std::sync::Arc::new(Self {
            bin,
            name: name.into(),
            log: state.join(format!("{name}.log")),
            state,
            args,
            password: (mode == SignIn::AppPassword)
                .then(|| a.password().map(|p| p.expose().to_string()))
                .transpose()?,
            child: tokio::sync::Mutex::new(None),
        });
        let stop = r.clone();
        ctx.defer(&format!("stop requester {name}"), async move {
            stop.stop().await
        });
        r.spawn().await?;
        if mode == SignIn::OAuthQr {
            let url = eventually(
                "the sign-in URL",
                Duration::from_secs(300),
                Duration::from_secs(3),
                || async {
                    let (ok, out) = r
                        .bin
                        .run(&[
                            "run",
                            "requester",
                            "signin",
                            &r.name,
                            "--state-dir",
                            &r.state.display().to_string(),
                        ])
                        .await?;
                    Ok(ok
                        .then(|| {
                            out.lines()
                                .find(|l| l.starts_with("https://"))
                                .map(str::to_string)
                        })
                        .flatten())
                },
            )
            .await?;
            oauth_signin(a, &url).await?;
        }
        r.wait_ready(Duration::from_secs(600)).await?;
        Ok(r)
    }

    async fn spawn(&self) -> Result<()> {
        let log = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.log)?;
        let mut c = self.bin.cmd();
        c.args(&self.args).stdout(log.try_clone()?).stderr(log);
        if let Some(p) = &self.password {
            c.env("ATPROTO_PASSWORD", p);
        }
        *self.child.lock().await = Some(c.spawn()?);
        Ok(())
    }

    pub fn ready_path(&self) -> PathBuf {
        self.state
            .join("requesters")
            .join(&self.name)
            .join("ready.json")
    }

    pub fn ready(&self) -> Result<Value> {
        Ok(serde_json::from_slice(&std::fs::read(self.ready_path())?)?)
    }

    pub async fn wait_ready(&self, timeout: Duration) -> Result<Value> {
        eventually(
            "the requester's ready file",
            timeout,
            Duration::from_secs(1),
            || async {
                if let Some(c) = self.child.lock().await.as_mut()
                    && let Some(st) = c.try_wait()?
                {
                    bail!(
                        "run requester {} exited {st} before ready; see {}",
                        self.name,
                        self.log.display()
                    );
                }
                Ok(self.ready().ok())
            },
        )
        .await
    }

    /// The requester workspace's kcp (kube-rs over the ready file's kubeconfig).
    pub async fn kcp(&self) -> Result<Kcp> {
        Kcp::kubeconfig(
            self.ready()?["kubeconfig"]
                .as_str()
                .context("ready file without kubeconfig")?,
        )
        .await
    }

    pub fn did(&self) -> Result<String> {
        Ok(self.ready()?["did"]
            .as_str()
            .context("ready file without did")?
            .into())
    }

    /// The core logs of the requester (its own log and the core.log files under the state dir).
    pub fn logs(&self) -> Vec<PathBuf> {
        let mut out = vec![self.log.clone()];
        find_named(&self.state, "core.log", &mut out);
        out
    }

    /// Ctrl-C: the kcp stops, the ready file goes, the state stays.
    pub async fn stop(&self) -> Result<()> {
        let mut g = self.child.lock().await;
        if let Some(c) = g.as_mut()
            && c.try_wait()?.is_none()
        {
            if let Some(pid) = c.id() {
                std::process::Command::new("kill")
                    .args(["-INT", &pid.to_string()])
                    .status()?;
            }
            if tokio::time::timeout(Duration::from_secs(120), c.wait())
                .await
                .is_err()
            {
                c.kill().await?;
                bail!(
                    "run requester {} did not stop in 120 s after SIGINT",
                    self.name
                );
            }
        }
        *g = None;
        Ok(())
    }

    /// Ctrl-C and the same command again (0013 A9).
    pub async fn restart(&self) -> Result<Value> {
        self.stop().await?;
        ensure!(
            !self.ready_path().exists(),
            "the ready file stayed after Ctrl-C"
        );
        self.spawn().await?;
        self.wait_ready(Duration::from_secs(300)).await
    }

    /// The WebRTC transport to a port a Held contract's guest tunnels as `tag` (a kcp in the guest): this requester's
    /// `run requester proxy` dials the datachannel (signalling through its own kcp) once per connection.
    pub fn webrtc_proxy(&self, contract: &str, tag: &str) -> crate::target::Via {
        let mut proxy = vec![self.bin.path.display().to_string()];
        proxy.extend(["run", "requester", "proxy", &self.name, "--state-dir"].map(String::from));
        proxy.push(self.state.display().to_string());
        proxy.extend(["--contract", contract, "--tag", tag].map(String::from));
        crate::target::Via::WebRtc { proxy }
    }

    /// `run requester ssh <name> --contract <cc> [--transport webrtc] --exec <cmd>`.
    pub async fn ssh(&self, contract: &str, transport: Option<&str>, exec: &str) -> Result<String> {
        let state = self.state.display().to_string();
        let mut args = vec![
            "run",
            "requester",
            "ssh",
            &self.name,
            "--state-dir",
            &state,
            "--contract",
            contract,
            "--exec",
            exec,
        ];
        if let Some(t) = transport {
            args.extend(["--transport", t]);
        }
        let (ok, out) = self.bin.run(&args).await?;
        ensure!(
            ok,
            "run requester ssh {contract} ({transport:?}) failed: {}",
            tail(&out)
        );
        Ok(out)
    }
}

fn find_named(dir: &Path, name: &str, out: &mut Vec<PathBuf>) {
    for e in std::fs::read_dir(dir).into_iter().flatten().flatten() {
        let p = e.path();
        if p.is_dir() {
            find_named(&p, name, out);
        } else if p.file_name().is_some_and(|n| n == name) {
            out.push(p);
        }
    }
}

pub fn tail(s: &str) -> String {
    let lines: Vec<_> = s.lines().collect();
    lines[lines.len().saturating_sub(15)..].join("\n")
}

/// One `run request` invocation (its own kcp; the CLI path of the runbooks).
pub struct RunRequest {
    pub bin: Binary,
    pub name: String,
    pub state: PathBuf,
    pub log: PathBuf,
    child: tokio::sync::Mutex<Option<tokio::process::Child>>,
}

impl RunRequest {
    /// `run request --state-dir <dir> --name <name> <args> -- <identity>` in the background (app password).
    /// A cleanup releases it.
    pub async fn start(
        ctx: &std::sync::Arc<Ctx>,
        account: &str,
        name: &str,
        args: &[&str],
    ) -> Result<std::sync::Arc<Self>> {
        let bin = Binary::resolve(&ctx.target.requester).await?;
        Self::start_with(ctx, bin, account, name, args).await
    }

    /// `start` with another binary (0006 compat: the previous release's CLI against new servers).
    pub async fn start_with(
        ctx: &std::sync::Arc<Ctx>,
        bin: Binary,
        account: &str,
        name: &str,
        args: &[&str],
    ) -> Result<std::sync::Arc<Self>> {
        let a = ctx.account(account)?;
        let state = ctx.dir.join("requests");
        std::fs::create_dir_all(&state)?;
        std::fs::set_permissions(&state, std::os::unix::fs::PermissionsExt::from_mode(0o700))?;
        let log = state.join(format!("{name}-request.log"));
        let f = std::fs::File::create(&log)?;
        let mut c = bin.cmd();
        c.args(["run", "request", "--state-dir"])
            .arg(&state)
            .args(["--name", name])
            .args(args)
            .arg("--");
        c.args(identity_args(&ctx.target, a, SignIn::AppPassword))
            .env("ATPROTO_PASSWORD", a.password()?.expose());
        c.stdout(f.try_clone()?).stderr(f);
        let r = std::sync::Arc::new(Self {
            bin,
            name: name.into(),
            state,
            log,
            child: tokio::sync::Mutex::new(Some(c.spawn()?)),
        });
        let rel = r.clone();
        ctx.defer(&format!("release {name}"), async move {
            rel.release().await.map(|_| ())
        });
        Ok(r)
    }

    /// The CLI log plus the invocation's core.log (while it runs, and after release).
    pub fn logs(&self) -> Vec<PathBuf> {
        let base = self.state.join("requests").join(&self.name);
        vec![
            self.log.clone(),
            base.join("dev").join(&self.name).join("logs/core.log"),
            base.join("logs/core.log"),
        ]
    }

    /// The contract phase lines of the CLI log; waits for `phase`.
    pub async fn wait_phase(&self, phase: &str, timeout: Duration) -> Result<Value> {
        eventually(
            &format!("{} {phase}", self.name),
            timeout,
            Duration::from_secs(1),
            || async {
                if let Some(c) = self.child.lock().await.as_mut()
                    && let Some(st) = c.try_wait()?
                    && !st.success()
                {
                    bail!(
                        "run request {} exited {st}: {}",
                        self.name,
                        tail(&std::fs::read_to_string(&self.log).unwrap_or_default())
                    );
                }
                Ok(log_lines(&self.log, "contract")
                    .into_iter()
                    .find(|l| l["phase"] == phase))
            },
        )
        .await
    }

    pub async fn wait_log(&self, msg: &str, timeout: Duration) -> Result<Value> {
        wait_log(&self.logs(), msg, timeout).await
    }

    fn sub(&self, verb: &str, extra: &[&str]) -> Vec<String> {
        let mut v: Vec<String> = ["run", "request", verb, &self.name, "--state-dir"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        v.push(self.state.display().to_string());
        v.extend(extra.iter().map(|s| s.to_string()));
        v
    }

    /// `run request agent <name>`: publishes guest.agentStart.
    pub async fn agent(&self) -> Result<()> {
        let args = self.sub("agent", &[]);
        let (ok, out) = self
            .bin
            .run(&args.iter().map(String::as_str).collect::<Vec<_>>())
            .await?;
        ensure!(ok, "run request agent: {}", tail(&out));
        Ok(())
    }

    /// `run request ssh <name> [--transport webrtc] --exec <cmd>` (iroh first by default).
    pub async fn ssh(&self, transport: Option<&str>, exec: &str) -> Result<String> {
        let mut extra = vec!["--exec", exec];
        if let Some(t) = transport {
            extra.extend(["--transport", t]);
        }
        let args = self.sub("ssh", &extra);
        let (ok, out) = self
            .bin
            .run(&args.iter().map(String::as_str).collect::<Vec<_>>())
            .await?;
        ensure!(ok, "run request ssh: {}", tail(&out));
        Ok(out)
    }

    /// Wait for the --hold process to exit (it exits after its first ssh unless held).
    pub async fn wait_exit(&self, timeout: Duration) -> Result<()> {
        if let Some(c) = self.child.lock().await.as_mut() {
            tokio::time::timeout(timeout, c.wait())
                .await
                .map_err(|_| {
                    anyhow!("run request {} still running after {timeout:?}", self.name)
                })??;
        }
        Ok(())
    }

    /// `run request release`; idempotent. Returns the seconds it took.
    pub async fn release(&self) -> Result<f64> {
        let t = Instant::now();
        let args = self.sub("release", &[]);
        let (ok, out) = self
            .bin
            .run(&args.iter().map(String::as_str).collect::<Vec<_>>())
            .await?;
        if let Some(mut c) = self.child.lock().await.take()
            && tokio::time::timeout(Duration::from_secs(300), c.wait())
                .await
                .is_err()
        {
            c.kill().await?;
        }
        ensure!(ok, "run request release {}: {}", self.name, tail(&out));
        Ok(t.elapsed().as_secs_f64())
    }

    pub fn summary(&self) -> Result<Value> {
        Ok(serde_json::from_slice(&std::fs::read(
            self.state
                .join("requests")
                .join(&self.name)
                .join("summary.json"),
        )?)?)
    }
}
