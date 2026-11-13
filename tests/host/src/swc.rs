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

#[path = "more.rs"]
mod more;
pub use more::cleanup_from_disk;

const P: &str = "computer.socialweb.test.";
pub const NSIDS: &[&str] = &[
    "computer.socialweb.test.requester.start",
    "computer.socialweb.test.requester.infra",
    "computer.socialweb.test.staging.meshKeys",
    "computer.socialweb.test.staging.meshJoin",
    "computer.socialweb.test.staging.probe",
    "computer.socialweb.test.staging.apply",
    "computer.socialweb.test.request.start",
    "computer.socialweb.test.request.ssh",
    "computer.socialweb.test.request.agent",
    "computer.socialweb.test.request.summary",
    "computer.socialweb.test.request.release",
    "computer.socialweb.test.request.records",
    "computer.socialweb.test.account.envFile",
    "computer.socialweb.test.atirc.meshSub",
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
    "computer.socialweb.test.oauth.signin",
    "computer.socialweb.test.request.r2",
    "computer.socialweb.test.request.checkLogs",
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
];

pub struct Swc {
    pub target: Target,
    pub account: String,
    pub bin: Binary,
    pub dir: PathBuf,
    pub kube: Arc<Kube>,
    /// receipts of this run's contracts: the kube guard lets a test delete only VMs of these
    pub receipts: Arc<Mutex<Vec<String>>>,
    requesters: tokio::sync::Mutex<HashMap<String, (PathBuf, tokio::process::Child)>>,
    requests: Mutex<HashMap<String, PathBuf>>,
    env_files: Mutex<Vec<PathBuf>>,
    /// requester name -> its kcp's kubeconfig
    kubeconfigs: Mutex<HashMap<String, String>>,
    /// (requester, kind, name) of each Infra/EphemeralInfra this run applied: released by cleanup()
    infras: Mutex<Vec<(String, String, String)>>,
    /// this host's meshd on a staging overlay (sudo: it makes a tun), stopped by cleanup()
    meshd: tokio::sync::Mutex<Option<tokio::process::Child>>,
    http: reqwest::Client,
    ssh: Arc<atlibs_ssh::Ssh>,
    extra: more::Extra,
}

fn input_of(r: &Request) -> Value {
    r.input
        .clone()
        .unwrap_or_else(|| Value::Object(r.params.clone().unwrap_or_default()))
}

fn str_of<'a>(v: &'a Value, k: &str) -> Result<&'a str, String> {
    v[k].as_str()
        .filter(|s| !s.is_empty() && !s.contains(['/', ' ', '\n']))
        .ok_or_else(|| format!("{k} is required (a plain name)"))
}

impl Swc {
    pub fn new(
        target: Target,
        account: &str,
        bin: Binary,
        dir: PathBuf,
        kube: Arc<Kube>,
        receipts: Arc<Mutex<Vec<String>>>,
        ssh: Arc<atlibs_ssh::Ssh>,
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
            kube,
            receipts,
            requesters: Default::default(),
            requests: Default::default(),
            env_files: Default::default(),
            kubeconfigs: Default::default(),
            infras: Default::default(),
            meshd: Default::default(),
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
        self.dir.join("requests")
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

    fn sub_args(&self, verb: &str, name: &str, extra: &[&str]) -> Vec<String> {
        let mut v: Vec<String> = ["run", "request", verb, name, "--state-dir"]
            .map(String::from)
            .to_vec();
        v.push(self.request_state().display().to_string());
        v.extend(extra.iter().map(|s| s.to_string()));
        v
    }

    async fn requester_start(&self, input: &Value) -> Result<Value, String> {
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
        (self.kubeconfigs.lock().unwrap()).insert(name.clone(), kubeconfig.clone());
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

    /// Applies an Infra or EphemeralInfra into requester `name`'s kcp, `@meshKey:<peer>` in a contract's userData
    /// replaced by that peer's meshd secret (the module never holds one). cleanup() releases it.
    async fn requester_infra(&self, input: &Value) -> Result<Value, String> {
        let name = str_of(input, "name")?;
        let kubeconfig = (self.kubeconfigs.lock().unwrap().get(name).cloned())
            .ok_or(format!("no requester {name}"))?;
        let mut obj = input["object"].clone();
        let kind = obj["kind"].as_str().unwrap_or_default().to_string();
        if !matches!(kind.as_str(), "Infra" | "EphemeralInfra") {
            return Err(format!(
                "requester.infra applies Infra or EphemeralInfra, not {kind}"
            ));
        }
        let iname = str_of(&obj["metadata"], "name")?.to_string();
        for c in obj["spec"]["contracts"]
            .as_array_mut()
            .into_iter()
            .flatten()
        {
            if let Some(ud) = c["userData"].as_str() {
                c["userData"] = json!(self.fill(ud, &input["vars"])?);
            }
        }
        let kcp = testkit::kcp::Kcp::kubeconfig(&kubeconfig)
            .await
            .map_err(|e| e.to_string())?;
        (self.infras.lock().unwrap()).push((name.into(), kind, iname.clone()));
        kcp.apply(&obj).await.map_err(|e| e.to_string())?;
        Ok(json!({ "name": iname }))
    }

    /// `{{secret:NAME}}`: the run's secret NAME (32 random bytes, hex, made 0600 on first use); `{{key}}`: `vars[key]`;
    /// then `@meshKey:<peer>`. Secrets stay on the host: the module passes names, never values.
    fn fill(&self, text: &str, vars: &Value) -> Result<String, String> {
        let (mut out, mut rest) = (String::new(), text);
        while let Some(i) = rest.find("{{") {
            out.push_str(&rest[..i]);
            let j = i + rest[i..].find("}}").ok_or("unclosed {{")?;
            let key = &rest[i + 2..j];
            let v = match key.strip_prefix("secret:") {
                Some(name) => self.secret(name)?,
                None => match &vars[key] {
                    Value::String(v) => v.clone(),
                    Value::Number(n) => n.to_string(),
                    _ => return Err(format!("{{{{{key}}}}}: no value")),
                },
            };
            out.push_str(&v);
            rest = &rest[j + 2..];
        }
        out.push_str(rest);
        self.with_mesh_keys(&out)
    }

    fn secret(&self, name: &str) -> Result<String, String> {
        if name.is_empty()
            || !name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        {
            return Err(format!("{name}: not a secret name"));
        }
        let dir = self.dir.join("secrets");
        let path = dir.join(name);
        if let Ok(v) = std::fs::read_to_string(&path) {
            return Ok(v);
        }
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        let mut b = [0u8; 32];
        getrandom::fill(&mut b).map_err(|e| e.to_string())?;
        let v: String = b.iter().map(|x| format!("{x:02x}")).collect();
        std::fs::write(&path, &v).map_err(|e| e.to_string())?;
        std::fs::set_permissions(&path, std::os::unix::fs::PermissionsExt::from_mode(0o600))
            .map_err(|e| e.to_string())?;
        Ok(v)
    }

    /// Applies `tests/compositions/staging/<file>` (a JSON array of objects) to kube `cluster`, filled host-side
    /// (`fill`): atlibs-kube's apply, so its cleanup deletes them. Only the applied names come back.
    async fn staging_apply(&self, input: &Value) -> Result<Value, String> {
        let (cluster, file) = (str_of(input, "cluster")?, str_of(input, "file")?);
        if file.contains('/') || !file.ends_with(".json") {
            return Err(format!(
                "{file}: a .json file in tests/compositions/staging"
            ));
        }
        let path = testkit::repo()
            .join("tests/compositions/staging")
            .join(file);
        let text =
            std::fs::read_to_string(&path).map_err(|e| format!("{}: {e}", path.display()))?;
        let objects: Vec<Value> = serde_json::from_str(&self.fill(&text, &input["vars"])?)
            .map_err(|e| format!("{file}: {e}"))?;
        let mut applied = vec![];
        for o in objects {
            let what = format!(
                "{}/{}",
                o["kind"].as_str().unwrap_or("?"),
                o["metadata"]["name"].as_str().unwrap_or("?")
            );
            let req = Request {
                input: Some(json!({ "cluster": cluster, "object": o })),
                ..Default::default()
            };
            self.kube
                .call("com.fedfork.exec.kube.apply".into(), req)
                .await
                .map_err(|e| format!("{what}: {e}"))?;
            applied.push(what);
        }
        Ok(json!({ "applied": applied }))
    }

    fn with_mesh_keys(&self, text: &str) -> Result<String, String> {
        let mut out = text.to_string();
        while let Some(at) = out.find("@meshKey:") {
            let end = out[at + 9..]
                .find(|c: char| !(c.is_ascii_alphanumeric() || c == '-'))
                .map_or(out.len(), |e| at + 9 + e);
            let peer = &out[at + 9..end];
            let key = std::fs::read_to_string(self.dir.join("mesh").join(format!("{peer}.key")))
                .map_err(|e| format!("@meshKey:{peer}: {e}"))?;
            out.replace_range(at..end, key.trim());
        }
        Ok(out)
    }

    /// A meshd secret per peer, kept 0600 in the run's dir; only the endpoints (ed25519 public keys) come back.
    fn mesh_keys(&self, input: &Value) -> Result<Value, String> {
        let dir = self.dir.join("mesh");
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        let mut peers = serde_json::Map::new();
        for n in input["names"].as_array().into_iter().flatten() {
            let n = n.as_str().ok_or("names are strings")?;
            if n.is_empty() || !n.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-') {
                return Err(format!("{n}: not a peer name"));
            }
            let mut secret = [0u8; 32];
            getrandom::fill(&mut secret).map_err(|e| e.to_string())?;
            let public = ed25519_dalek::SigningKey::from_bytes(&secret)
                .verifying_key()
                .to_bytes();
            let hex = |b: &[u8]| b.iter().map(|x| format!("{x:02x}")).collect::<String>();
            let path = dir.join(format!("{n}.key"));
            std::fs::write(&path, hex(&secret)).map_err(|e| e.to_string())?;
            std::fs::set_permissions(&path, std::os::unix::fs::PermissionsExt::from_mode(0o600))
                .map_err(|e| e.to_string())?;
            peers.insert(n.into(), json!(hex(&public)));
        }
        Ok(json!({ "endpoints": peers }))
    }

    /// This host joins the overlay as `peer` (its meshd secret from meshKeys): `socialweb-meshd run` (`$SWC_MESHD`) on the
    /// roster, serving the zone on `address`:53. Back once /healthz answers (routes synced).
    async fn mesh_join(&self, input: &Value) -> Result<Value, String> {
        let (peer, address) = (str_of(input, "peer")?, str_of(input, "address")?);
        let (roster, zone) = (str_of(input, "roster")?, str_of(input, "zone")?);
        let meshd = std::env::var("SWC_MESHD").unwrap_or_else(|_| "socialweb-meshd".into());
        let secret = self.dir.join("mesh").join(format!("{peer}.key"));
        let log = std::fs::File::create(self.dir.join("meshd.log")).map_err(|e| e.to_string())?;
        let metrics = "127.0.0.1:19791";
        let child = tokio::process::Command::new("sudo")
            .args(["-n", &meshd, "run", "--secret-file"])
            .arg(&secret)
            .args([
                "--roster",
                roster,
                "--dns-zone",
                zone,
                "--dns-listen",
                &format!("{address}:53"),
            ])
            .args(["--tun-name", "swcstg0", "--metrics-listen", metrics])
            .stdout(log.try_clone().map_err(|e| e.to_string())?)
            .stderr(log)
            .spawn()
            .map_err(|e| format!("{meshd}: {e}"))?;
        *self.meshd.lock().await = Some(child);
        let deadline = Instant::now() + Duration::from_secs(120);
        loop {
            let ok = self
                .http
                .get(format!("http://{metrics}/healthz"))
                .send()
                .await;
            if ok.is_ok_and(|r| r.status().is_success()) {
                return Ok(json!({ "peer": peer, "address": address }));
            }
            if let Some(c) = self.meshd.lock().await.as_mut()
                && let Ok(Some(st)) = c.try_wait()
            {
                return Err(format!(
                    "meshd exited {st}: {}",
                    self.dir.join("meshd.log").display()
                ));
            }
            if Instant::now() > deadline {
                return Err("meshd not ready in 2 min".into());
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
    }

    /// Each `<name>.<zone>` from the overlay DNS at `dns` (ip:port), retried until `timeoutS`: {resolved, failed}.
    async fn probe(&self, input: &Value) -> Result<Value, String> {
        let (dns, zone) = (str_of(input, "dns")?, str_of(input, "zone")?);
        let (ip, port) = dns.rsplit_once(':').ok_or("dns is ip:port")?;
        let deadline =
            Instant::now() + Duration::from_secs(input["timeoutS"].as_u64().unwrap_or(60));
        let mut resolved = serde_json::Map::new();
        let names: Vec<&str> = input["names"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(Value::as_str)
            .collect();
        loop {
            let pending: Vec<&str> = names
                .iter()
                .copied()
                .filter(|n| !resolved.contains_key(*n))
                .collect();
            for n in pending {
                let fqdn = format!("{n}.{zone}");
                let out = tokio::process::Command::new("dig")
                    .args([
                        "+short",
                        "+time=2",
                        "+tries=1",
                        &format!("@{ip}"),
                        "-p",
                        port,
                        &fqdn,
                        "A",
                    ])
                    .output()
                    .await
                    .map_err(|e| format!("dig: {e}"))?;
                let text = String::from_utf8_lossy(&out.stdout);
                if let Some(a) = text
                    .lines()
                    .find(|l| l.parse::<std::net::Ipv4Addr>().is_ok())
                {
                    resolved.insert(n.to_string(), json!(a));
                }
            }
            let failed: Vec<&str> = names
                .iter()
                .copied()
                .filter(|n| !resolved.contains_key(*n))
                .collect();
            if failed.is_empty() || Instant::now() > deadline {
                return Ok(json!({ "resolved": resolved, "failed": failed }));
            }
            tokio::time::sleep(Duration::from_secs(5)).await;
        }
    }

    /// Sets every contract of each Infra this run applied to Released, then waits (15 min) until none is Held.
    async fn release_infras(&self) -> Vec<String> {
        let mut errors = vec![];
        let infras: Vec<_> = self.infras.lock().unwrap().drain(..).collect();
        for (requester, kind, name) in infras {
            let Some(kubeconfig) = self.kubeconfigs.lock().unwrap().get(&requester).cloned() else {
                continue;
            };
            let released = async {
                let kcp = testkit::kcp::Kcp::kubeconfig(&kubeconfig).await?;
                let Some(obj) = kcp.get(&kind, &name).await? else {
                    return anyhow::Ok(());
                };
                let mut contracts = obj["spec"]["contracts"].clone();
                for c in contracts.as_array_mut().into_iter().flatten() {
                    c["state"] = json!("Released");
                }
                kcp.merge(&kind, &name, &json!({ "spec": { "contracts": contracts } }))
                    .await?;
                let deadline = Instant::now() + Duration::from_secs(900);
                loop {
                    let o = kcp.get(&kind, &name).await?.unwrap_or_default();
                    let held: Vec<String> =
                        (o["status"]["contracts"].as_array().into_iter().flatten())
                            .filter(|c| !matches!(c["phase"].as_str(), Some("Released" | "Failed")))
                            .map(|c| format!("{} {}", c["name"], c["phase"]))
                            .collect();
                    if held.is_empty() {
                        return Ok(());
                    }
                    if Instant::now() > deadline {
                        anyhow::bail!("not released in 15 min: {}", held.join(", "));
                    }
                    tokio::time::sleep(Duration::from_secs(5)).await;
                }
            };
            if let Err(e) = released.await {
                errors.push(format!("{kind} {name}: {e:#}"));
            }
        }
        errors
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
        let args = self.request_args(input)?;
        let state = self.request_state();
        std::fs::create_dir_all(&state).map_err(|e| e.to_string())?;
        std::fs::set_permissions(&state, std::os::unix::fs::PermissionsExt::from_mode(0o700))
            .map_err(|e| e.to_string())?;
        let logs = self.request_logs(&name);
        let f = std::fs::File::create(&logs[0]).map_err(|e| e.to_string())?;
        // compat: the unchanged client (SWC_COMPAT_REQUESTER: release:<tag>, sha:..., path:...) against new servers
        let bin = match (input["compat"] == true, std::env::var("SWC_COMPAT_REQUESTER")) {
            (true, Ok(spec)) => Binary::resolve(&spec).await.map_err(|e| e.to_string())?,
            _ => self.bin.clone(),
        };
        let mut child = bin
            .cmd()
            .args(["run", "request", "--state-dir"])
            .arg(&state)
            .args(["--name", &name])
            .args(&args)
            .arg("--")
            .args(identity_args(
                &self.target,
                self.acct()?,
                SignIn::AppPassword,
            ))
            .env("ATPROTO_PASSWORD", self.password()?)
            .stdout(f.try_clone().map_err(|e| e.to_string())?)
            .stderr(f)
            .spawn()
            .map_err(|e| e.to_string())?;
        self.requests.lock().unwrap().insert(name.clone(), state);
        let (tx, messages) = mpsc::unbounded_channel();
        let (commands, mut cmds) = mpsc::unbounded_channel::<SubCmd>();
        tokio::spawn(async move {
            let mut offsets = vec![0usize; logs.len()];
            let mut exited = false;
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
        let cluster = input["cluster"].as_str().ok_or("cluster is required")?;
        let node = str_of(input, "node")?;
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
            "requester.infra" => self.requester_infra(&input).await,
            "staging.meshKeys" => self.mesh_keys(&input),
            "staging.meshJoin" => self.mesh_join(&input).await,
            "staging.probe" => self.probe(&input).await,
            "staging.apply" => self.staging_apply(&input).await,
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
                let did = self.acct()?.did.clone();
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
                None => Err(format!("{nsid}: not a call of this host")),
            },
        }
    }

    /// Release every invocation and Infra, stop every requester (Ctrl-C), remove the password env files. The errors.
    pub async fn cleanup(&self) -> Vec<String> {
        let mut errors = self.more_cleanup().await;
        errors.extend(self.release_infras().await);
        let names: Vec<String> = self.requests.lock().unwrap().keys().cloned().collect();
        for n in names {
            if let Err(e) = self.release(&n).await {
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
        if let Some(mut c) = self.meshd.lock().await.take()
            && let Ok(None) = c.try_wait()
        {
            if let Some(pid) = c.id() {
                let _ = std::process::Command::new("kill")
                    .args(["-INT", &pid.to_string()])
                    .status();
            }
            if tokio::time::timeout(Duration::from_secs(30), c.wait())
                .await
                .is_err()
            {
                errors.push("meshd did not stop in 30 s".into());
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
