//! computer.socialweb.test.*: the socialweb.computer side of the test host. The requester under test (a release, a
//! commit or a local build) as the account: `run requester` (the orchestrator, its kcp registered as kube cluster
//! `requester:<name>`), `run request` invocations (a subscription of their log events), the owner's atirc mesh node.
//! Every requester and invocation it starts is stopped or released by `cleanup()`, however the run ended.
//! com.fedfork.exec.identlink.{walk,plc}: identlink against the target's PLC.

use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use atlibs_host::BoxFut;
use atlibs_host::run::{Call, Request, Sub, SubCmd};
use atlibs_host::transport::Transport;
use atlibs_kube::{Cluster, Kube};
use serde_json::{Value, json};
use testkit::requester::{Binary, SignIn, identity_args, kept_by_rule, tail, uris_in};
use testkit::target::{Account, Target, check_private, expand};
use tokio::sync::mpsc;

#[path = "knotless.rs"]
mod knotless;
#[path = "more.rs"]
mod more;
pub use more::cleanup_from_disk;

const P: &str = "computer.socialweb.test.";
/// Where the guest requester reads its association ref (the renter writes it), and how long it may take to come.
const R2_REF: &str = "/run/socialweb-r2/association-ref";
const R2_REF_WAIT: Duration = Duration::from_secs(15 * 60);
pub const NSIDS: &[&str] = &[
    "computer.socialweb.test.requester.start",
    "computer.socialweb.test.request.start",
    "computer.socialweb.test.request.ssh",
    "computer.socialweb.test.request.put",
    "computer.socialweb.test.request.agent",
    "computer.socialweb.test.request.summary",
    "computer.socialweb.test.request.release",
    "computer.socialweb.test.request.records",
    "computer.socialweb.test.account.envFile",
    "computer.socialweb.test.atirc.meshSub",
    "computer.socialweb.test.atirc.meshRun",
    "computer.socialweb.test.atirc.meshPub",
    "computer.socialweb.test.atirc.meshPeers",
    "computer.socialweb.test.kube.addCluster",
    "computer.socialweb.test.page.signin",
    "computer.socialweb.test.page.fetch",
    "computer.socialweb.test.page.round",
    "computer.socialweb.test.ssh.tempKey",
    "computer.socialweb.test.atirc.observer",
    "computer.socialweb.test.atirc.observerPub",
    "computer.socialweb.test.atirc.observerRevoke",
    "computer.socialweb.test.requester.ssh",
    "computer.socialweb.test.requester.restart",
    "computer.socialweb.test.requester.cluster",
    "computer.socialweb.test.requester.bundle",
    "computer.socialweb.test.requester.records",
    "computer.socialweb.test.requester.events",
    "computer.socialweb.test.root.exec",
    "computer.socialweb.test.root.putBinary",
    "computer.socialweb.test.example",
    "computer.socialweb.test.gateway.kubeconfig",
    "computer.socialweb.test.page.kubeconfig",
    "computer.socialweb.test.oauth.signin",
    "computer.socialweb.test.request.r2",
    "computer.socialweb.test.request.checkLogs",
    "computer.socialweb.test.knotless.seed",
    "computer.socialweb.test.knotless.sh",
    "computer.socialweb.test.bidder.health",
    "computer.socialweb.test.gateway.log",
    "com.fedfork.exec.identlink.walk",
    "com.fedfork.exec.identlink.plc",
];
/// `run request` flags a module may pass; `@agentConfig` is the target's agent config (owner-only file).
const FLAGS: &[&str] = &[
    "--hold",
    "--agent",
    "--agent-config",
    "--tunnel",
    "--transport",
    "--exec",
    "--guest-tools",
    "--associate",
    "--expires-in",
    "--nested-virt",
    "--cpus",
    "--mem",
];

pub struct Swc {
    pub target: Target,
    pub account: String,
    pub bin: Binary,
    /// the work dir: requester state, kubeconfigs, keys (secrets; never an artifact)
    pub dir: PathBuf,
    /// the dump dir: resources.json beside the ticks (no secret)
    pub dump: PathBuf,
    pub kube: Arc<Kube>,
    /// receipts of this run's contracts: the kube guard lets a test delete only VMs of these
    pub receipts: Arc<Mutex<Vec<String>>>,
    requesters: tokio::sync::Mutex<HashMap<String, (PathBuf, tokio::process::Child)>>,
    requests: Mutex<HashMap<String, PathBuf>>,
    env_files: Mutex<Vec<PathBuf>>,
    http: reqwest::Client,
    ssh: Arc<atlibs_ssh::Ssh>,
    extra: more::Extra,
}

/// A call's input; a subscription's arguments come as params (strings), its structure in the `json` param.
fn input_of(r: &Request) -> Value {
    if let Some(i) = &r.input {
        return i.clone();
    }
    let params = r.params.clone().unwrap_or_default();
    match params
        .get("json")
        .and_then(Value::as_str)
        .and_then(|j| serde_json::from_str::<Value>(j).ok())
    {
        Some(v) => v,
        None => Value::Object(params),
    }
}

fn str_of<'a>(v: &'a Value, k: &str) -> Result<&'a str, String> {
    v[k].as_str()
        .filter(|s| !s.is_empty() && !s.contains(['/', ' ', '\n']))
        .ok_or_else(|| format!("{k} is required (a plain name)"))
}

impl Swc {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        target: Target,
        account: &str,
        bin: Binary,
        dir: PathBuf,
        kube: Arc<Kube>,
        receipts: Arc<Mutex<Vec<String>>>,
        ssh: Arc<atlibs_ssh::Ssh>,
        dump: PathBuf,
    ) -> Arc<Self> {
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .unwrap();
        Arc::new(Self {
            target,
            account: account.into(),
            bin,
            dir,
            dump,
            kube,
            receipts,
            requesters: Default::default(),
            requests: Default::default(),
            env_files: Default::default(),
            http,
            ssh,
            extra: Default::default(),
        })
    }

    pub fn transport(self: &Arc<Self>) -> Transport {
        Transport {
            provides: NSIDS,
            call: self.clone(),
        }
    }

    fn acct(&self) -> Result<&Account, String> {
        self.target
            .account(&self.account)
            .map_err(|e| e.to_string())
    }

    fn password(&self) -> Result<String, String> {
        Ok(self
            .acct()?
            .password()
            .map_err(|e| e.to_string())?
            .expose()
            .to_string())
    }

    fn request_state(&self) -> PathBuf {
        // the guest requester's `run request` holds a dev instance (its PDS, its kcp) whose sockets under
        // requests/<name>/run/ must fit sun_path (107 bytes): a short dir of its own, ~/.swc/<run ms>
        if self.target.network.is_none() && self.acct().is_ok_and(|a| a.guest_requester) {
            let name = self
                .dir
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .to_string();
            let ms = name.rsplit('-').next().unwrap_or_default().to_string();
            return PathBuf::from(std::env::var("HOME").unwrap_or_default())
                .join(".swc")
                .join(ms);
        }
        match &self.target.network {
            Some(n) => expand(&n.state_dir),
            None => self.dir.join("requests"),
        }
    }

    fn request_logs(&self, name: &str) -> Vec<PathBuf> {
        let base = self.request_state().join("requests").join(name);
        vec![
            self.request_state().join(format!("{name}-request.log")),
            base.join("dev").join(name).join("logs/core.log"),
            base.join("logs/core.log"),
        ]
    }

    async fn cli(&self, args: &[String]) -> Result<(bool, String), String> {
        self.bin
            .run(&args.iter().map(String::as_str).collect::<Vec<_>>())
            .await
            .map_err(|e| e.to_string())
    }

    /// Copies one of this host's files to the guest of invocation `name` (stdin of `run request ssh --exec 'cat >
    /// <remote>'`). `what` names the file, never a path: testHost ($SWC_TEST_HOST_STATIC, a static swc-test-host) or
    /// suite (a tar.gz of the repo's examples, scripts, tests/compositions, tests/targets and the module wasm and
    /// module.json files; SWC_REPO on the guest).
    async fn request_put(&self, input: &Value) -> Result<Value, String> {
        let name = str_of(input, "name")?;
        let remote = input["remote"].as_str().unwrap_or_default();
        if !remote.starts_with('/') || remote.contains(['\'', ' ', '\n']) {
            return Err(format!(
                "remote {remote:?}: an absolute path without quotes or spaces"
            ));
        }
        let bytes = match str_of(input, "what")? {
            "testHost" => {
                let p = std::env::var("SWC_TEST_HOST_STATIC")
                    .map_err(|_| "SWC_TEST_HOST_STATIC is not set (tests/build-static-host.sh)")?;
                std::fs::read(&p).map_err(|e| format!("{p}: {e}"))?
            }
            "suite" => {
                let tests = std::env::current_dir().map_err(|e| e.to_string())?;
                let out = tokio::process::Command::new("sh")
                    .args(["-c", "tar -czf - tests/compositions tests/targets tests/modules/*/module.json tests/modules/target/wasm32-unknown-unknown/release/*.wasm examples scripts"])
                    .current_dir(tests.join(".."))
                    .output()
                    .await
                    .map_err(|e| e.to_string())?;
                if !out.status.success() {
                    return Err(format!(
                        "tar of the suite in {}: {}",
                        tests.display(),
                        String::from_utf8_lossy(&out.stderr)
                    ));
                }
                out.stdout
            }
            other => return Err(format!("what {other:?}: testHost or suite")),
        };
        let exec = format!("cat > '{remote}.part' && mv '{remote}.part' '{remote}'");
        let mut child = self
            .bin
            .cmd()
            .args(self.sub_args("ssh", name, &["--exec", &exec]))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|e| e.to_string())?;
        let mut sin = child.stdin.take().unwrap();
        use tokio::io::AsyncWriteExt;
        sin.write_all(&bytes).await.map_err(|e| e.to_string())?;
        drop(sin);
        let out = child.wait_with_output().await.map_err(|e| e.to_string())?;
        if !out.status.success() {
            return Err(format!(
                "put {remote}: {}",
                tail(&String::from_utf8_lossy(&out.stderr))
            ));
        }
        Ok(json!({ "remote": remote, "bytes": bytes.len() }))
    }

    fn sub_args(&self, verb: &str, name: &str, extra: &[&str]) -> Vec<String> {
        let mut v: Vec<String> = ["run", "request", verb, name, "--state-dir"]
            .map(String::from)
            .to_vec();
        v.push(self.request_state().display().to_string());
        v.extend(extra.iter().map(|s| s.to_string()));
        v
    }

    async fn requester_start(&self, input: &Value) -> Result<Value, String> {
        if self.acct()?.guest_requester {
            return Err(
                "the guest requester runs `run request` only, not a long-lived `run requester`"
                    .into(),
            );
        }
        let name = str_of(input, "name")?.to_string();
        let state = self.dir.join(format!("requester-{name}"));
        std::fs::create_dir_all(&state).map_err(|e| e.to_string())?;
        std::fs::set_permissions(&state, std::os::unix::fs::PermissionsExt::from_mode(0o700))
            .map_err(|e| e.to_string())?;
        let mut args: Vec<String> = vec![
            "run".into(),
            "requester".into(),
            "--name".into(),
            name.clone(),
            "--state-dir".into(),
            state.display().to_string(),
        ];
        if input["withGateway"] == true {
            args.push("--with-gateway".into());
        }
        args.push("--".into());
        args.extend(identity_args(
            &self.target,
            self.acct()?,
            SignIn::AppPassword,
        ));
        self.extra
            .argv
            .lock()
            .unwrap()
            .insert(name.clone(), args.clone());
        let log =
            std::fs::File::create(state.join(format!("{name}.log"))).map_err(|e| e.to_string())?;
        let child = self
            .bin
            .cmd()
            .args(&args)
            .env("ATPROTO_PASSWORD", self.password()?)
            .stdout(log.try_clone().map_err(|e| e.to_string())?)
            .stderr(log)
            .spawn()
            .map_err(|e| e.to_string())?;
        self.requesters
            .lock()
            .await
            .insert(name.clone(), (state.clone(), child));
        let ready = state.join("requesters").join(&name).join("ready.json");
        let deadline = Instant::now() + Duration::from_secs(600);
        let r: Value = loop {
            if let Ok(b) = std::fs::read(&ready)
                && let Ok(v) = serde_json::from_slice(&b)
            {
                break v;
            }
            if let Some((_, c)) = self.requesters.lock().await.get_mut(&name)
                && let Ok(Some(st)) = c.try_wait()
            {
                return Err(format!("run requester {name} exited {st} before ready"));
            }
            if Instant::now() > deadline {
                return Err(format!("run requester {name}: no ready file in 10 min"));
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        };
        let kubeconfig = r["kubeconfig"]
            .as_str()
            .ok_or("ready file without kubeconfig")?
            .to_string();
        self.kube.add_cluster(
            &format!("requester:{name}"),
            Cluster {
                kubeconfig,
                workspace: None,
                via: Default::default(),
            },
        );
        Ok(
            json!({ "did": r["did"], "handle": r["handle"], "identityMode": r["identityMode"], "cluster": format!("requester:{name}"),
                    "gateway": r["gateway"], "sshKey": r["sshPublicKey"].as_str().and_then(|p| std::fs::read_to_string(p).ok()).map(|k| k.trim().to_string()) }),
        )
    }

    fn request_args(&self, input: &Value) -> Result<Vec<String>, String> {
        let mut out = vec![];
        for w in input["args"].as_array().into_iter().flatten() {
            let w = w.as_str().ok_or("args are strings")?;
            if w == "@agentConfig" {
                let p = expand(
                    self.target
                        .agent_config
                        .as_deref()
                        .ok_or("the target has no agentConfig")?,
                );
                check_private(&p).map_err(|e| e.to_string())?;
                out.push(p.display().to_string());
            } else if w.starts_with("--") && !FLAGS.contains(&w.split('=').next().unwrap()) {
                return Err(format!("run request flag {w} is not one a test may pass"));
            } else {
                out.push(w.to_string());
            }
        }
        Ok(out)
    }

    /// `run request --name <name> <args> -- <identity>`, its log events as messages until the subscription closes.
    async fn request_start(&self, input: &Value) -> Result<Sub, String> {
        let name = str_of(input, "name")?.to_string();
        let mut args = self.request_args(input)?;
        if let Some(n) = &self.target.network {
            args.extend(["--network".to_string(), n.instance.clone()]);
        }
        let state = self.request_state();
        std::fs::create_dir_all(&state).map_err(|e| e.to_string())?;
        std::fs::set_permissions(&state, std::os::unix::fs::PermissionsExt::from_mode(0o700))
            .map_err(|e| e.to_string())?;
        let logs = self.request_logs(&name);
        let f = std::fs::File::create(&logs[0]).map_err(|e| e.to_string())?;
        // compat: the unchanged client (SWC_COMPAT_REQUESTER: release:<tag>, sha:..., path:...) against new servers
        let bin = match (
            input["compat"] == true,
            std::env::var("SWC_COMPAT_REQUESTER"),
        ) {
            (true, Ok(spec)) => Binary::resolve(&spec).await.map_err(|e| e.to_string())?,
            _ => self.bin.clone(),
        };
        let mut cmd = bin.cmd();
        cmd.args(["run", "request", "--state-dir"])
            .arg(&state)
            .args(["--name", &name])
            .args(&args);
        // R2 rents as the renter's agent: its identity is the association, no password
        if self.acct()?.guest_requester {
            // its own key in the VM it rents, for request.ssh
            cmd.args(["--guest-requester", "--skip-ssh=false"]);
        } else {
            cmd.arg("--")
                .args(identity_args(
                    &self.target,
                    self.acct()?,
                    SignIn::AppPassword,
                ))
                .env("ATPROTO_PASSWORD", self.password()?);
        }
        let mut child = cmd
            .stdout(f.try_clone().map_err(|e| e.to_string())?)
            .stderr(f)
            .spawn()
            .map_err(|e| e.to_string())?;
        self.requests.lock().unwrap().insert(name.clone(), state);
        let (tx, messages) = mpsc::unbounded_channel();
        // which binary runs the invocation (0006 compat: the unchanged CLI's)
        let _ = tx.send(Ok(json!({ "msg": "requester_binary", "version": bin.version, "compat": input["compat"] == true })));
        let (commands, mut cmds) = mpsc::unbounded_channel::<SubCmd>();
        let receipts = self.receipts.clone();
        // R2 waits for its association ref, which the renter writes once it verified R2: one that never comes is an
        // error of its own, not a request that waits until the run's deadline
        let ref_by = self
            .acct()?
            .guest_requester
            .then(|| Instant::now() + R2_REF_WAIT);
        tokio::spawn(async move {
            let mut offsets = vec![0usize; logs.len()];
            let mut exited = false;
            // the run's dev core.log is copied to logs/core.log when the process ends: each line once
            let mut seen = std::collections::HashSet::new();
            loop {
                for (i, p) in logs.iter().enumerate() {
                    let Ok(text) = std::fs::read_to_string(p) else {
                        continue;
                    };
                    if text.len() < offsets[i] {
                        offsets[i] = 0;
                    }
                    let fresh = &text[offsets[i]..];
                    let Some(end) = fresh.rfind('\n') else {
                        continue;
                    };
                    for line in fresh[..=end].lines() {
                        let Ok(v) = serde_json::from_str::<Value>(line) else {
                            continue;
                        };
                        if !seen.insert(line.to_string()) {
                            continue;
                        }
                        // the receipt is known (and the run's VM ours to fault) from its first event, before any summary
                        if let Some(r) = v["receipt"].as_str() {
                            let mut rs = receipts.lock().unwrap();
                            if !rs.iter().any(|x| x == r) {
                                rs.push(r.into());
                            }
                        }
                        let msg = v["msg"].as_str().unwrap_or_default();
                        if (msg == "contract"
                            || msg == "ssh returned"
                            || msg.starts_with("requester_")
                            || msg.starts_with("guest_"))
                            && tx.send(Ok(v)).is_err()
                        {
                            return;
                        }
                    }
                    offsets[i] += end + 1;
                }
                if !exited && let Ok(Some(st)) = child.try_wait() {
                    exited = true;
                    let _ = tx.send(Ok(json!({ "msg": "exited", "code": st.code() })));
                }
                let referred =
                    || std::fs::read_to_string(R2_REF).is_ok_and(|r| !r.trim().is_empty());
                if !exited && ref_by.is_some_and(|by| Instant::now() > by) && !referred() {
                    exited = true;
                    let _ = child.start_kill();
                    let why = format!(
                        "no association ref in {R2_REF} after {} min: the renter's associate failed or never ran (the knotless check's log says why)",
                        R2_REF_WAIT.as_secs() / 60
                    );
                    let _ = tx.send(Ok(json!({ "msg": "exited", "code": null, "error": why })));
                }
                tokio::select! {
                    cmd = cmds.recv() => match cmd {
                        Some(SubCmd::Send(_, reply)) => { let _ = reply.send(Err("request.start takes no sends".into())); }
                        Some(SubCmd::Close) | None => return,
                    },
                    _ = tokio::time::sleep(Duration::from_secs(1)) => {}
                }
            }
        });
        Ok(Sub { messages, commands })
    }

    fn summary(&self, name: &str) -> Result<Value, String> {
        let p = self
            .request_state()
            .join("requests")
            .join(name)
            .join("summary.json");
        let v: Value = serde_json::from_slice(
            &std::fs::read(&p).map_err(|e| format!("{}: {e}", p.display()))?,
        )
        .map_err(|e| e.to_string())?;
        if let Some(r) = v["contract"]["receiptUri"].as_str() {
            let mut rs = self.receipts.lock().unwrap();
            if !rs.iter().any(|x| x == r) {
                rs.push(r.into());
            }
        }
        Ok(v)
    }

    async fn release(&self, name: &str) -> Result<Value, String> {
        let t = Instant::now();
        let (ok, out) = self.cli(&self.sub_args("release", name, &[])).await?;
        if !ok {
            return Err(format!("run request release {name}: {}", tail(&out)));
        }
        Ok(json!({ "seconds": (t.elapsed().as_secs_f64() * 10.0).round() / 10.0 }))
    }

    /// The owner node's atirc and config dir: the AtircNode's HostProcess (same host).
    async fn mesh_node(&self, input: &Value) -> Result<(PathBuf, String), String> {
        let node = str_of(input, "node")?;
        if let Some((bin, dir, _)) = self.extra.nodes.lock().await.get(node) {
            return Ok((bin.clone(), dir.display().to_string()));
        }
        let cluster = input["cluster"].as_str().ok_or("cluster is required")?;
        let get = |kind: &str, name: &str| {
            let input = json!({ "cluster": cluster, "apiVersion": "socialweb.computer/v1alpha1", "kind": kind, "name": name });
            self.kube.call(
                atlibs_kube::GET.into(),
                Request {
                    input: Some(input),
                    ..Default::default()
                },
            )
        };
        let deadline = Instant::now() + Duration::from_secs(180);
        loop {
            let o = get("AtircNode", node).await?.unwrap_or_default();
            if o["status"]["phase"] == "Running" {
                let dir = o["status"]["configDir"]
                    .as_str()
                    .ok_or("AtircNode without configDir")?
                    .to_string();
                let pid = match o["status"]["runtime"]["pid"].as_i64() {
                    Some(p) => p,
                    None => {
                        let hp = o["status"]["hostProcess"]
                            .as_str()
                            .ok_or("AtircNode without hostProcess")?;
                        get("HostProcess", hp).await?.unwrap_or_default()["status"]["runtime"]["pid"].as_i64().ok_or("HostProcess without pid")?
                    }
                };
                let bin = std::fs::read_link(format!("/proc/{pid}/exe"))
                    .map_err(|e| format!("the AtircNode runs on this host: {e}"))?;
                return Ok((bin, dir));
            }
            if Instant::now() > deadline {
                return Err(format!(
                    "AtircNode {node} not Running in 3 min: {}",
                    o["status"]
                ));
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
    }

    async fn mesh(&self, input: &Value, args: &[&str]) -> Result<Value, String> {
        let (bin, dir) = self.mesh_node(input).await?;
        let out = tokio::process::Command::new(bin)
            .arg("mesh")
            .args(args)
            .env("ATIRC_CONFIG_DIR", dir)
            .env("ATP_USERNAME", &self.acct()?.handle)
            .stdin(Stdio::null())
            .output()
            .await
            .map_err(|e| e.to_string())?;
        Ok(
            json!({ "ok": out.status.success(), "output": format!("{}{}", String::from_utf8_lossy(&out.stdout), String::from_utf8_lossy(&out.stderr)) }),
        )
    }

    async fn plc(&self, did: &str) -> Result<Value, String> {
        let base = self.target.plc.trim_end_matches('/');
        let doc = self
            .http
            .get(format!("{base}/{did}"))
            .send()
            .await
            .map_err(|e| e.to_string())?;
        let status = doc.status().as_u16();
        let doc: Value = doc.json().await.unwrap_or_default();
        let last: Value = self
            .http
            .get(format!("{base}/{did}/log/last"))
            .send()
            .await
            .map_err(|e| e.to_string())?
            .json()
            .await
            .unwrap_or_default();
        let data: Value = self
            .http
            .get(format!("{base}/{did}/data"))
            .send()
            .await
            .map_err(|e| e.to_string())?
            .json()
            .await
            .unwrap_or_default();
        Ok(
            json!({ "docStatus": status, "lastType": last["type"], "alsoKnownAs": doc["alsoKnownAs"], "rotationKeys": data["rotationKeys"] }),
        )
    }

    async fn handle(&self, nsid: &str, input: Value) -> Result<Value, String> {
        let what = nsid.strip_prefix(P).unwrap_or(nsid);
        match what {
            "requester.start" => self.requester_start(&input).await,
            "request.put" => self.request_put(&input).await,
            "request.ssh" => {
                let name = str_of(&input, "name")?;
                let exec = input["exec"].as_str().ok_or("exec is required")?;
                let mut extra = vec!["--exec", exec];
                if let Some(t) = input["transport"].as_str() {
                    extra.extend(["--transport", t]);
                }
                let (ok, out) = self.cli(&self.sub_args("ssh", name, &extra)).await?;
                Ok(json!({ "ok": ok, "output": out }))
            }
            "request.agent" => {
                let name = str_of(&input, "name")?;
                let (ok, out) = self.cli(&self.sub_args("agent", name, &[])).await?;
                if ok {
                    Ok(json!({}))
                } else {
                    Err(format!("run request agent: {}", tail(&out)))
                }
            }
            "request.summary" => self.summary(str_of(&input, "name")?),
            "request.release" => self.release(str_of(&input, "name")?).await,
            "request.records" => {
                let name = str_of(&input, "name")?;
                let did = self.acct()?.did_now();
                let mut uris: Vec<String> = uris_in(&self.request_logs(name), &did)
                    .into_iter()
                    .filter(|u| !kept_by_rule(u))
                    .collect();
                if let Ok(s) = self.summary(name)
                    && let Some(r) = s["contract"]["receiptUri"].as_str()
                {
                    uris.push(format!(
                        "at://{did}/{}/{}",
                        identlink::market::VM_DELETE_NSID,
                        identlink::market::release_rkey(r)
                    ));
                }
                Ok(json!({ "uris": uris }))
            }
            "account.envFile" => {
                use std::io::Write;
                use std::os::unix::fs::OpenOptionsExt;
                let p = self
                    .dir
                    .join(format!("atp-{}.env", self.env_files.lock().unwrap().len()));
                let mut f = std::fs::OpenOptions::new()
                    .create(true)
                    .truncate(true)
                    .write(true)
                    .mode(0o600)
                    .open(&p)
                    .map_err(|e| e.to_string())?;
                writeln!(f, "ATP_PASSWORD={}", self.password()?).map_err(|e| e.to_string())?;
                self.env_files.lock().unwrap().push(p.clone());
                Ok(json!({ "path": p }))
            }
            "atirc.meshPub" => {
                let text = input["text"]
                    .as_str()
                    .ok_or("text is required")?
                    .to_string();
                self.mesh(&input, &["pub", &text]).await
            }
            "atirc.meshPeers" => self.mesh(&input, &["peers"]).await,
            "com.fedfork.exec.identlink.walk" => {
                let agent = input["agent"].as_str().ok_or("agent is required")?;
                let io = identlink::HttpIo {
                    plc: self.target.plc.trim_end_matches('/').into(),
                    ..Default::default()
                };
                Ok(
                    serde_json::to_value(identlink::walk(&io, agent, Default::default()).await)
                        .unwrap(),
                )
            }
            "com.fedfork.exec.identlink.plc" => {
                self.plc(input["did"].as_str().ok_or("did is required")?)
                    .await
            }
            other => match self.more(other, &input).await {
                Some(r) => r,
                None => match self.knotless(other, &input).await {
                    Some(r) => r,
                    None => Err(format!("{nsid}: not a call of this host")),
                },
            },
        }
    }

    /// Release every invocation, stop every requester (Ctrl-C), remove the password env files. The errors.
    pub async fn cleanup(&self) -> Vec<String> {
        let mut errors = self.knotless_cleanup().await;
        errors.extend(self.more_cleanup().await);
        let names: Vec<String> = self.requests.lock().unwrap().keys().cloned().collect();
        for n in names {
            // an owner that is settling a release of its own (0005 B's out-of-band loss) stops its kcp under our ask:
            // ask again once it is gone
            let mut r = self.release(&n).await;
            for _ in 0..3 {
                if r.is_ok() {
                    break;
                }
                tokio::time::sleep(Duration::from_secs(20)).await;
                r = self.release(&n).await;
            }
            if let Err(e) = r {
                errors.push(e);
            }
        }
        for (name, (_, mut c)) in self.requesters.lock().await.drain() {
            if let Ok(None) = c.try_wait() {
                if let Some(pid) = c.id() {
                    let _ = std::process::Command::new("kill")
                        .args(["-INT", &pid.to_string()])
                        .status();
                }
                if tokio::time::timeout(Duration::from_secs(120), c.wait())
                    .await
                    .is_err()
                {
                    let _ = c.kill().await;
                    errors.push(format!("run requester {name} did not stop in 120 s"));
                }
            }
        }
        for p in self.env_files.lock().unwrap().drain(..) {
            let _ = std::fs::remove_file(p);
        }
        errors
    }
}

impl Call for Swc {
    fn call(&self, nsid: String, request: Request) -> BoxFut<'_, Result<Option<Value>, String>> {
        Box::pin(async move {
            let r = self.handle(&nsid, input_of(&request)).await.map(Some);
            self.write_resources().await;
            r
        })
    }

    fn subscribe(&self, nsid: String, request: Request) -> BoxFut<'_, Result<Sub, String>> {
        Box::pin(async move {
            let input = input_of(&request);
            match nsid.strip_prefix(P) {
                Some("request.start") => {
                    let r = self.request_start(&input).await;
                    self.write_resources().await;
                    r
                }
                Some("atirc.meshSub") => {
                    let (bin, dir) = self.mesh_node(&input).await?;
                    let mut child = tokio::process::Command::new(bin)
                        .args(["mesh", "sub", "--cursor", "0"])
                        .env("ATIRC_CONFIG_DIR", dir)
                        .env("ATP_USERNAME", &self.acct()?.handle)
                        .stdin(Stdio::null())
                        .stdout(Stdio::piped())
                        .stderr(Stdio::null())
                        .kill_on_drop(true)
                        .spawn()
                        .map_err(|e| e.to_string())?;
                    let out = child.stdout.take().unwrap();
                    let (tx, messages) = mpsc::unbounded_channel();
                    let (commands, mut cmds) = mpsc::unbounded_channel::<SubCmd>();
                    tokio::spawn(async move {
                        use tokio::io::AsyncBufReadExt;
                        let mut lines = tokio::io::BufReader::new(out).lines();
                        loop {
                            tokio::select! {
                                l = lines.next_line() => match l {
                                    Ok(Some(l)) => if let Ok(v) = serde_json::from_str::<Value>(&l) && tx.send(Ok(v)).is_err() { break },
                                    _ => break,
                                },
                                cmd = cmds.recv() => match cmd {
                                    Some(SubCmd::Send(_, reply)) => { let _ = reply.send(Err("meshSub takes no sends".into())); }
                                    Some(SubCmd::Close) | None => break,
                                },
                            }
                        }
                        let _ = child.kill().await;
                    });
                    Ok(Sub { messages, commands })
                }
                Some(other) => match self.more_sub(other, &input).await {
                    Some(r) => r,
                    None => Err(format!("{nsid} is not a subscription of this host")),
                },
                None => Err(format!("{nsid} is not a subscription of this host")),
            }
        })
    }
}
