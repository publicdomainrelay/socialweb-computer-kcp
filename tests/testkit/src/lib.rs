//! The system test harness: a target (target.json), accounts, a requester orchestrator, kcp clients, atproto
//! reads, cleanup that always runs, per-account locks and readings. See README.md.

pub mod atirc;
pub mod atproto;
pub mod bidder;
pub mod gateway;
pub mod kcp;
pub mod requester;
pub mod target;
pub mod tenant;

use std::collections::BTreeMap;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

pub use anyhow::{Context, Result, anyhow, bail, ensure};
use futures::FutureExt;
pub use serde_json::{Value, json};

pub use atproto::{Atproto, home_tmp, runs_dir};
pub use kcp::Kcp;
pub use requester::{Binary, Requester, RunRequest, SignIn};
pub use target::{Account, Target};
pub use tenant::Tenant;

type Cleanup = Pin<Box<dyn Future<Output = Result<()>> + Send>>;

/// The kcp repository root (the runbooks, examples/, scripts/): $SWC_REPO (a staging VM's suite tree), else this
/// build's checkout.
pub fn repo() -> PathBuf {
    if let Ok(r) = std::env::var("SWC_REPO") {
        return PathBuf::from(r);
    }
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .canonicalize()
        .unwrap()
}

pub fn example(name: &str) -> String {
    std::fs::read_to_string(repo().join("examples/infra").join(name)).unwrap()
}

/// One test run: its target, scratch dir, readings and cleanups.
pub struct Ctx {
    pub target: Target,
    pub at: Atproto,
    /// owner-only scratch dir of this run (~/.tmp/swc-system-tests/runs/<runbook>-<test>-<ms>)
    pub dir: PathBuf,
    pub runbook: String,
    pub test: String,
    pub started: Instant,
    readings: Mutex<BTreeMap<String, Value>>,
    cleanups: Mutex<Vec<(String, Cleanup)>>,
    /// every app password this run read: no log may hold one
    secrets: Mutex<Vec<String>>,
}

impl Ctx {
    /// Record one reading (goes to the readings JSON, and to the runbook's last-pass line).
    pub fn reading(&self, key: &str, v: impl serde::Serialize) {
        let v = serde_json::to_value(v).unwrap();
        eprintln!("reading {key} = {v}");
        self.readings.lock().unwrap().insert(key.into(), v);
    }

    /// Seconds since the run started.
    pub fn t(&self) -> f64 {
        (self.started.elapsed().as_millis() as f64 / 100.0).round() / 10.0
    }

    /// Run `f` at the end whether the test passed or not, last registered first. A cleanup must be
    /// idempotent (release an already released VM, delete a deleted record).
    pub fn defer(&self, what: &str, f: impl Future<Output = Result<()>> + Send + 'static) {
        self.cleanups
            .lock()
            .unwrap()
            .push((what.into(), Box::pin(f)));
    }

    pub fn account(&self, name: &str) -> Result<&Account> {
        let a = self.target.account(name)?;
        self.secrets
            .lock()
            .unwrap()
            .push(a.password()?.expose().to_string());
        Ok(a)
    }

    /// Fail when a log holds an app password, an ICE candidate or an age key (the runbooks' plaintext check).
    pub fn check_logs(&self, paths: &[PathBuf]) -> Result<()> {
        let secrets = self.secrets.lock().unwrap().clone();
        for p in paths {
            let Ok(text) = std::fs::read_to_string(p) else {
                continue;
            };
            ensure!(
                !secrets
                    .iter()
                    .any(|s| !s.is_empty() && text.contains(s.as_str())),
                "{} holds an app password",
                p.display()
            );
            for bad in ["a=candidate", "AGE-SECRET-KEY"] {
                ensure!(!text.contains(bad), "{} holds plaintext {bad}", p.display());
            }
        }
        Ok(())
    }
}

impl Ctx {
    /// End the test as skipped (it passes, the readings say why, last-pass writes nothing): what it needs is not in
    /// the target (prod names no bidder kcp unless the lead grants one).
    pub fn skip(&self, why: &str) -> Result<()> {
        eprintln!("SKIPPED {} {}: {why}", self.runbook, self.test);
        self.reading("skipped", why);
        Ok(())
    }

    /// A root host of a staging target (None on prod: root steps run only on staging VMs).
    pub fn root_host(&self) -> Option<String> {
        (self.target.name != "prod")
            .then(|| self.target.root_hosts.first().cloned())
            .flatten()
    }

    /// Run `cmd` as root on `host` (ssh); stdout. `stdin` is written to its stdin (a password, never an argument).
    pub async fn root(&self, host: &str, cmd: &str, stdin: Option<&str>) -> Result<String> {
        use tokio::io::AsyncWriteExt;
        let mut c = tokio::process::Command::new("ssh");
        c.args(["-o", "BatchMode=yes", host, cmd])
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped());
        let mut child = c.spawn()?;
        let mut sin = child.stdin.take().unwrap();
        if let Some(s) = stdin {
            sin.write_all(s.as_bytes()).await?;
        }
        drop(sin);
        let out = child.wait_with_output().await?;
        ensure!(
            out.status.success(),
            "{host}: `{cmd}` exited {}: {}",
            out.status,
            requester::tail(&String::from_utf8_lossy(&out.stderr))
        );
        Ok(String::from_utf8_lossy(&out.stdout).to_string())
    }

    /// The target's bidders whose kcp it names, connected.
    pub async fn bidder_kcps(&self) -> Result<Vec<bidder::BidderKcp>> {
        let mut out = vec![];
        for b in &self.target.bidders {
            if let Some(k) = bidder::BidderKcp::connect(b).await? {
                out.push(k);
            }
        }
        Ok(out)
    }

    /// Every record the logs name in `did`'s repo, less those kept by rule, answers RecordNotFound (3 reads).
    pub async fn records_gone(&self, logs: &[PathBuf], did: &str) -> Result<usize> {
        let uris: Vec<_> = requester::uris_in(logs, did)
            .into_iter()
            .filter(|u| !requester::kept_by_rule(u))
            .collect();
        let left = self.at.still_present(&uris).await?;
        ensure!(
            left.is_empty(),
            "records left in {did}'s repo after release: {left:?}"
        );
        Ok(uris.len())
    }

    /// The renter's release record (`vm.delete` at release_rkey(receipt)) is gone once the release finished (0006: a
    /// CLI release left it).
    pub async fn release_record_gone(&self, did: &str, receipt: &str) -> Result<()> {
        let uri = format!("at://{did}/{VM_DELETE}/{}", release_rkey(receipt));
        let left = self.at.still_present(std::slice::from_ref(&uri)).await?;
        ensure!(
            left.is_empty(),
            "the release's vm.delete is still in the repo: {uri}"
        );
        Ok(())
    }

    /// The ident.link walk of `agent` (identlink, verified sync.getRecord) against the target's PLC.
    pub async fn walk(&self, agent: &str) -> identlink::Walk {
        let io = identlink::HttpIo {
            plc: self.target.plc.trim_end_matches('/').into(),
            ..Default::default()
        };
        identlink::walk(&io, agent, Default::default()).await
    }

    /// The winner's host holds no VM object naming `needle` (0008 orphan check). None: the target names no
    /// kcp for that bidder, so it is not read.
    pub async fn host_empty(&self, winner: &str, needle: &str) -> Result<Option<bool>> {
        let Some(b) = self.target.bidder_by_did(winner) else {
            return Ok(None);
        };
        let Some(bk) = bidder::BidderKcp::connect(b).await? else {
            return Ok(None);
        };
        Ok(Some(bk.vms_of(needle).await?.is_empty()))
    }
}

/// The market's computed rkeys (identlink): a receipt's vm.terminated (bidder repo) and vm.delete (renter repo).
pub use identlink::market::{
    VM_DELETE_NSID as VM_DELETE, VM_TERMINATED_NSID as VM_TERMINATED, release_rkey,
    termination_rkey,
};

/// Before an apply: every ssh public key in a templated spec is this run's `key`, and there is one (a key baked into
/// a file, or a placeholder left in, would make the guest refuse our ssh).
pub fn ensure_keys(spec: &str, key: &str) -> Result<()> {
    let key = key.trim();
    ensure!(key.starts_with("ssh-"), "the run has no ssh public key");
    let mut n = 0;
    for l in spec.lines() {
        let t = l.trim().trim_start_matches("- ").trim().trim_matches(|c| c == '"' || c == '\'');
        if ["ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-", "sk-ssh-"].iter().any(|p| t.starts_with(p)) {
            ensure!(t == key, "the spec carries an ssh key that is not this run's: {}", t.chars().take(40).collect::<String>());
            n += 1;
        }
    }
    ensure!(n > 0, "the spec carries no ssh key");
    Ok(())
}

/// Wait for `f` to answer Some, polling every `every`.
pub async fn eventually<T, F: Future<Output = Result<Option<T>>>>(
    what: &str,
    timeout: Duration,
    every: Duration,
    mut f: impl FnMut() -> F,
) -> Result<T> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(v) = f().await? {
            return Ok(v);
        }
        ensure!(Instant::now() < deadline, "{what}: not within {timeout:?}");
        tokio::time::sleep(every).await;
    }
}

fn dry_run() -> bool {
    std::env::var("SWC_DRY_RUN").is_ok_and(|v| v == "1")
}

/// Hold the per-account lock (one driver per account): a file lock under ~/.tmp/swc-system-tests/locks,
/// released when the process ends.
pub async fn lock_account(handle: &str) -> Result<std::fs::File> {
    let dir = home_tmp("locks");
    std::fs::create_dir_all(&dir)?;
    let f = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(dir.join(format!("{handle}.lock")))?;
    if f.try_lock().is_err() {
        eprintln!("waiting for the lock of {handle} (another driver holds it)");
        return Ok(tokio::task::spawn_blocking(move || f.lock().map(|_| f)).await??);
    }
    Ok(f)
}

/// Run one test: lock its accounts, run `body`, then every cleanup (also on failure or panic), then write
/// the readings. SWC_DRY_RUN=1 stops after the preflight (target, creds files, requester binary) and
/// touches no account.
pub async fn run<F>(runbook: &str, test: &str, accounts: &[&str], body: impl FnOnce(Arc<Ctx>) -> F)
where
    F: Future<Output = Result<()>>,
{
    let target = Target::load().unwrap();
    let mut locks = vec![];
    for a in accounts {
        let acc = target.account(a).unwrap();
        acc.password().unwrap();
        if !dry_run() {
            locks.push(lock_account(&acc.handle).await.unwrap());
        }
    }
    let dir = home_tmp("runs").join(format!("{runbook}-{test}-{}", atlibs_host::now_ms()));
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::set_permissions(&dir, std::os::unix::fs::PermissionsExt::from_mode(0o700)).unwrap();
    let at = Atproto::new(&target);
    let ctx = Arc::new(Ctx {
        target,
        at,
        dir,
        runbook: runbook.into(),
        test: test.into(),
        started: Instant::now(),
        readings: Mutex::default(),
        cleanups: Mutex::default(),
        secrets: Mutex::default(),
    });
    let started_at = atlibs_host::iso_now();
    let bin = Binary::resolve(&ctx.target.requester).await.unwrap();
    ctx.reading(
        "requester",
        json!({ "spec": ctx.target.requester, "version": bin.version }),
    );
    if dry_run() {
        eprintln!(
            "dry run {runbook} {test}: target {} ok, accounts {accounts:?} readable, requester {}",
            ctx.target.name, bin.version
        );
        return;
    }
    let outcome = std::panic::AssertUnwindSafe(body(ctx.clone()))
        .catch_unwind()
        .await;
    let mut failure = match outcome {
        Ok(Ok(())) => None,
        Ok(Err(e)) => Some(format!("{e:#}")),
        Err(p) => Some(
            p.downcast_ref::<String>()
                .cloned()
                .or_else(|| p.downcast_ref::<&str>().map(|s| s.to_string()))
                .unwrap_or_else(|| "panic".into()),
        ),
    };
    let cleanups: Vec<_> = std::mem::take(&mut *ctx.cleanups.lock().unwrap());
    let mut cleanup_errors = vec![];
    for (what, f) in cleanups.into_iter().rev() {
        if let Err(e) = f.await {
            eprintln!("cleanup {what} FAILED: {e:#}");
            cleanup_errors.push(format!("{what}: {e:#}"));
        }
    }
    if failure.is_none() && !cleanup_errors.is_empty() {
        failure = Some(format!("cleanup: {}", cleanup_errors.join("; ")));
    }
    let out = std::env::var("SWC_READINGS")
        .map(PathBuf::from)
        .unwrap_or_else(|_| repo().join("tests/readings"));
    std::fs::create_dir_all(&out).unwrap();
    let file = out.join(format!(
        "{runbook}-{test}-{}.json",
        started_at.replace([':', '.'], "")
    ));
    let doc = json!({
        "runbook": runbook, "test": test, "target": ctx.target.name, "startedAt": started_at, "endedAt": atlibs_host::iso_now(),
        "seconds": ctx.t(), "pass": failure.is_none(), "error": failure, "cleanupErrors": cleanup_errors,
        "readings": *ctx.readings.lock().unwrap(), "scratch": ctx.dir,
    });
    std::fs::write(&file, serde_json::to_string_pretty(&doc).unwrap()).unwrap();
    eprintln!("readings: {}", file.display());
    drop(locks);
    if let Some(f) = failure {
        panic!("{runbook} {test}: {f}");
    }
}
