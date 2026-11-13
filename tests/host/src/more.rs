//! The rest of computer.socialweb.test.*: tenant clusters, the k8s and ssh web pages (OAuth by headless Chrome, a
//! cookie the host keeps), the ssh gateway's temporary key, an atirc observer token, the orchestrator's ssh / restart /
//! cluster / bundle / records / events, the page round (0009), root on a staging VM. Secrets (cookies, tokens,
//! passwords) stay in the host.

use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use atlibs_host::run::{Sub, SubCmd};
use atlibs_kube::Cluster;
use atlibs_ssh::Dest;
use serde_json::{Value, json};
use testkit::requester::{kept_by_rule, oauth_signin_with, tail, uris_in};
use testkit::target::{check_private, expand};
use tokio::io::AsyncBufReadExt;
use tokio::sync::mpsc;

use super::{Swc, str_of};

#[derive(Default)]
pub struct Extra {
    /// origin -> Cookie header of a signed-in page session
    jars: Mutex<HashMap<String, String>>,
    /// (ssh web origin, rkey): temporary keys to remove
    keys: Mutex<Vec<(String, String)>>,
    /// observer nick -> its atirc token
    tokens: Mutex<HashMap<String, String>>,
    /// requester name -> its run requester argv (restart)
    pub argv: Mutex<HashMap<String, Vec<String>>>,
}

fn origin(url: &str) -> Result<String, String> {
    let u = reqwest::Url::parse(url).map_err(|e| e.to_string())?;
    Ok(u.origin().ascii_serialization())
}

fn write_private(path: &PathBuf, text: &str) -> Result<(), String> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .mode(0o600)
        .open(path)
        .map_err(|e| e.to_string())?;
    f.write_all(text.as_bytes()).map_err(|e| e.to_string())
}

fn find_named(dir: &std::path::Path, name: &str, out: &mut Vec<PathBuf>) {
    for e in std::fs::read_dir(dir).into_iter().flatten().flatten() {
        let p = e.path();
        if p.is_dir() {
            find_named(&p, name, out);
        } else if p.file_name().is_some_and(|n| n == name) {
            out.push(p);
        }
    }
}

/// A subscription of the JSON log lines `keep` takes, from the files `logs()` names now (new ones appear), and the
/// child's exit as {msg: "exited", code}.
pub fn tail_sub(
    logs: impl Fn() -> Vec<PathBuf> + Send + 'static,
    mut child: Option<tokio::process::Child>,
    keep: fn(&str) -> bool,
) -> Sub {
    let (tx, messages) = mpsc::unbounded_channel();
    let (commands, mut cmds) = mpsc::unbounded_channel::<SubCmd>();
    tokio::spawn(async move {
        let mut offsets: HashMap<PathBuf, usize> = HashMap::new();
        loop {
            for p in logs() {
                let Ok(text) = std::fs::read_to_string(&p) else {
                    continue;
                };
                let off = offsets.entry(p.clone()).or_default();
                if text.len() < *off {
                    *off = 0;
                }
                let fresh = &text[*off..];
                let Some(end) = fresh.rfind('\n') else {
                    continue;
                };
                for line in fresh[..=end].lines() {
                    let Ok(v) = serde_json::from_str::<Value>(line) else {
                        continue;
                    };
                    if keep(v["msg"].as_str().unwrap_or_default()) && tx.send(Ok(v)).is_err() {
                        return;
                    }
                }
                *off += end + 1;
            }
            if let Some(c) = child.as_mut()
                && let Ok(Some(st)) = c.try_wait()
            {
                let _ = tx.send(Ok(json!({ "msg": "exited", "code": st.code() })));
                child = None;
            }
            tokio::select! {
                cmd = cmds.recv() => match cmd {
                    Some(SubCmd::Send(_, reply)) => { let _ = reply.send(Err("this subscription takes no sends".into())); }
                    Some(SubCmd::Close) | None => return,
                },
                _ = tokio::time::sleep(Duration::from_secs(1)) => {}
            }
        }
    });
    Sub { messages, commands }
}

pub fn market_lines(msg: &str) -> bool {
    msg == "contract"
        || msg == "ssh returned"
        || msg.starts_with("requester_")
        || msg.starts_with("guest_")
        || msg.starts_with("infra_")
}

impl Swc {
    fn requester_state(&self, name: &str) -> PathBuf {
        self.dir.join(format!("requester-{name}"))
    }

    fn atirc(&self) -> Result<PathBuf, String> {
        testkit::atirc::atirc_bin().map_err(|e| e.to_string())
    }

    async fn page_signin(&self, input: &Value) -> Result<Value, String> {
        let url = input["url"].as_str().ok_or("url is required")?;
        let jar = self.dir.join(format!("jar-{}.json", atlibs_host::now_ms()));
        let jar_s = jar.display().to_string();
        let mut extra: Vec<&str> = vec!["--cookie-file", &jar_s];
        if input["startApi"] == true {
            extra.push("--start-api");
        }
        let done = input["done"].as_str().map(String::from);
        if let Some(d) = &done {
            extra.extend(["--done", d.as_str()]);
        }
        let r = oauth_signin_with(self.acct()?, url, &extra).await;
        let cookies: Value = std::fs::read(&jar)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default();
        let _ = std::fs::remove_file(&jar);
        let header = cookies
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
        if header.is_empty() {
            return Err(format!(
                "signing in at {url}: no session cookie ({})",
                r.err().map(|e| e.to_string()).unwrap_or_default()
            ));
        }
        self.extra.jars.lock().unwrap().insert(origin(url)?, header);
        Ok(json!({ "ok": true }))
    }

    /// A request to a signed-in page: its cookie, and Origin = the page's (a cookie POST needs it).
    async fn page_fetch(
        &self,
        url: &str,
        method: &str,
        body: Option<&Value>,
    ) -> Result<Value, String> {
        let o = origin(url)?;
        let cookie = self
            .extra
            .jars
            .lock()
            .unwrap()
            .get(&o)
            .cloned()
            .ok_or_else(|| format!("{o}: not signed in (page.signin first)"))?;
        let mut req = self
            .http
            .request(method.parse().map_err(|_| "bad method")?, url)
            .header("cookie", cookie)
            .header("origin", &o);
        if let Some(b) = body {
            req = req.json(b);
        }
        let r = req.send().await.map_err(|e| e.to_string())?;
        let status = r.status().as_u16();
        let text = r.text().await.unwrap_or_default();
        Ok(
            json!({ "status": status, "body": serde_json::from_str::<Value>(&text).unwrap_or(Value::String(text)) }),
        )
    }

    async fn temp_key(&self) -> Result<Value, String> {
        let host = self.target.gateway.ssh.clone();
        let web = format!("https://ssh.{host}");
        let key = self.dir.join("st_gateway_key");
        if !key.exists() {
            let ok = tokio::process::Command::new("ssh-keygen")
                .args(["-q", "-t", "ed25519", "-N", "", "-C", "swc-test-host", "-f"])
                .arg(&key)
                .stdout(Stdio::null())
                .status()
                .await
                .map_err(|e| e.to_string())?
                .success();
            if !ok {
                return Err("ssh-keygen failed".into());
            }
        }
        let public = std::fs::read_to_string(key.with_extension("pub"))
            .map_err(|e| e.to_string())?
            .trim()
            .to_string();
        let added = self
            .page_fetch(
                &format!("{web}/keys"),
                "POST",
                Some(&json!({ "publicKey": public })),
            )
            .await?;
        let rkey = added["body"]["rkey"]
            .as_str()
            .ok_or_else(|| format!("POST /keys: {}", added))?
            .to_string();
        self.extra.keys.lock().unwrap().push((web, rkey));
        let dest = Dest {
            dest: format!("{}@{host}", self.acct()?.handle),
            port: Some(self.target.gateway.ssh_port.unwrap_or(22)),
            key: Some(key),
            known_hosts: Some(self.dir.join("known_hosts")),
            user_config: false,
        };
        self.ssh.add_dest("gateway", dest);
        if let Some(cfg) = &self.target.agent_config {
            let p = expand(cfg);
            check_private(&p).map_err(|e| e.to_string())?;
            self.ssh.add_stdin_file("agentConfig", p);
        }
        Ok(json!({ "dest": "gateway" }))
    }

    async fn observer_run(&self, nick: &str, args: &[&str]) -> Result<bool, String> {
        let token = self
            .extra
            .tokens
            .lock()
            .unwrap()
            .get(nick)
            .cloned()
            .ok_or_else(|| format!("no observer {nick}"))?;
        let endpoint = self.target.atirc.clone().ok_or("the target has no atirc")?;
        Ok(tokio::process::Command::new(self.atirc()?)
            .args(args)
            .env("ATIRC_TOKEN", token)
            .env("ATIRC_ENDPOINT", endpoint)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()
            .await
            .map_err(|e| e.to_string())?
            .success())
    }

    async fn observer_sub(&self, input: &Value) -> Result<Sub, String> {
        let nick = str_of(input, "nick")?.to_string();
        let a = self.acct()?;
        let endpoint = self.target.atirc.clone().ok_or("the target has no atirc")?;
        let out = tokio::process::Command::new(self.atirc()?)
            .args(["token", &nick, "--endpoint", &endpoint])
            .env("ATP_USERNAME", &a.handle)
            .env("ATP_PASSWORD", self.password()?)
            .stdin(Stdio::null())
            .output()
            .await
            .map_err(|e| e.to_string())?;
        let token = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if !out.status.success() || token.is_empty() {
            return Err(format!("atirc token {nick} failed"));
        }
        self.extra
            .tokens
            .lock()
            .unwrap()
            .insert(nick.clone(), token.clone());
        let chan = format!("#{}", a.handle.replace(['.', '-'], "_"));
        let mut child = tokio::process::Command::new(self.atirc()?)
            .args(["sub", "--full", "--cursor", "-1", "-c", &chan])
            .env("ATIRC_TOKEN", token)
            .env("ATIRC_ENDPOINT", endpoint)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| e.to_string())?;
        let stdout = child.stdout.take().unwrap();
        let (tx, messages) = mpsc::unbounded_channel();
        let (commands, mut cmds) = mpsc::unbounded_channel::<SubCmd>();
        tokio::spawn(async move {
            let mut lines = tokio::io::BufReader::new(stdout).lines();
            loop {
                tokio::select! {
                    l = lines.next_line() => match l {
                        Ok(Some(l)) => if let Ok(v) = serde_json::from_str::<Value>(&l) && tx.send(Ok(v)).is_err() { break },
                        _ => break,
                    },
                    cmd = cmds.recv() => match cmd {
                        Some(SubCmd::Send(_, reply)) => { let _ = reply.send(Err("no sends".into())); }
                        Some(SubCmd::Close) | None => break,
                    },
                }
            }
            let _ = child.kill().await;
        });
        Ok(Sub { messages, commands })
    }

    async fn requester_restart(&self, name: &str) -> Result<Value, String> {
        let argv = self
            .extra
            .argv
            .lock()
            .unwrap()
            .get(name)
            .cloned()
            .ok_or_else(|| format!("no requester {name}"))?;
        let state = self.requester_state(name);
        if let Some((_, mut c)) = self.requesters.lock().await.remove(name) {
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
                return Err(format!("run requester {name} did not stop in 120 s"));
            }
        }
        let ready = state.join("requesters").join(name).join("ready.json");
        if ready.exists() {
            return Err("the ready file stayed after Ctrl-C".into());
        }
        let t = Instant::now();
        let log = std::fs::OpenOptions::new()
            .append(true)
            .create(true)
            .open(state.join(format!("{name}.log")))
            .map_err(|e| e.to_string())?;
        let child = self
            .bin
            .cmd()
            .args(&argv)
            .env("ATPROTO_PASSWORD", self.password()?)
            .stdout(log.try_clone().map_err(|e| e.to_string())?)
            .stderr(log)
            .spawn()
            .map_err(|e| e.to_string())?;
        self.requesters
            .lock()
            .await
            .insert(name.into(), (state.clone(), child));
        let deadline = Instant::now() + Duration::from_secs(300);
        loop {
            if let Ok(b) = std::fs::read(&ready)
                && let Ok(v) = serde_json::from_slice::<Value>(&b)
            {
                return Ok(
                    json!({ "did": v["did"], "seconds": (t.elapsed().as_secs_f64() * 10.0).round() / 10.0 }),
                );
            }
            if Instant::now() > deadline {
                return Err(format!(
                    "run requester {name}: not ready 5 min after the restart"
                ));
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }

    async fn requester_cli(
        &self,
        verb: &str,
        name: &str,
        extra: &[&str],
    ) -> Result<(bool, String), String> {
        let state = self.requester_state(name).display().to_string();
        let mut args = vec!["run", "requester", verb, name, "--state-dir", &state];
        args.extend(extra);
        self.bin.run(&args).await.map_err(|e| e.to_string())
    }

    async fn requester_bundle(&self, input: &Value) -> Result<Value, String> {
        let name = str_of(input, "name")?;
        let contract = str_of(input, "contract")?;
        let cfg = expand(
            self.target
                .agent_config
                .as_deref()
                .ok_or("the target has no agentConfig")?,
        );
        check_private(&cfg).map_err(|e| e.to_string())?;
        let get = |kind: &str, n: &str| {
            let input = json!({ "cluster": format!("requester:{name}"), "apiVersion": "socialweb.computer/v1alpha1", "kind": kind, "name": n });
            atlibs_host::run::Call::call(
                &*self.kube,
                atlibs_kube::GET.into(),
                atlibs_host::run::Request {
                    input: Some(input),
                    ..Default::default()
                },
            )
        };
        let addr =
            get("OpenBaoServer", "requester").await?.unwrap_or_default()["status"]["address"]
                .as_str()
                .ok_or("no OpenBaoServer address")?
                .to_string();
        let d = self
            .requester_state(name)
            .join("requesters")
            .join(name)
            .join("dev")
            .join(name)
            .join("openbaoservers/requester/openbao");
        let runtime = PathBuf::from(std::env::var("HOME").unwrap_or_default())
            .join(".cache/socialweb-computer/runtime");
        let bao = std::fs::read_dir(&runtime)
            .map_err(|e| e.to_string())?
            .flatten()
            .map(|e| e.path().join("bao"))
            .find(|p| p.is_file())
            .ok_or("no bao in the runtime cache")?;
        let bundle = self.dir.join(format!("{contract}-bundle.json"));
        let v = json!([{ "path": "/etc/atirc/atirc.env", "value": "", "mode": "0600" }, { "path": "/etc/atirc/config.toml", "value": std::fs::read_to_string(&cfg).map_err(|e| e.to_string())?, "mode": "0600" }]);
        write_private(&bundle, &v.to_string())?;
        let env = |c: &mut tokio::process::Command| {
            c.env("BAO_ADDR", &addr)
                .env("BAO_CACERT", d.join("tls/ca.crt"))
                .env("BAO_CLIENT_CERT", d.join("clients/requester/client.crt"))
                .env("BAO_CLIENT_KEY", d.join("clients/requester/client.key"));
        };
        let mut login = tokio::process::Command::new(&bao);
        login.args(["write", "-field=token", "auth/cert/login", "name=requester"]);
        env(&mut login);
        let out = login.output().await.map_err(|e| e.to_string())?;
        let mut put = tokio::process::Command::new(&bao);
        put.args([
            "kv",
            "put",
            "-mount=requester",
            &format!("contracts/{contract}/secrets"),
        ])
        .arg(format!("bundle=@{}", bundle.display()));
        env(&mut put);
        put.env("BAO_TOKEN", String::from_utf8_lossy(&out.stdout).trim())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        let ok = out.status.success() && put.status().await.map_err(|e| e.to_string())?.success();
        let _ = std::fs::remove_file(&bundle);
        if ok {
            Ok(json!({ "ok": true }))
        } else {
            Err("the agent bundle did not reach the requester's OpenBao".into())
        }
    }

    /// The rest of the calls; None: not one of these.
    pub(crate) async fn more(&self, what: &str, input: &Value) -> Option<Result<Value, String>> {
        Some(match what {
            "kube.addCluster" => {
                let name = match str_of(input, "name") {
                    Ok(n) => n.to_string(),
                    Err(e) => return Some(Err(e)),
                };
                let path = self.dir.join(format!("tenant-{name}.kubeconfig"));
                let r = write_private(&path, input["kubeconfig"].as_str().unwrap_or_default());
                // viaRoot: a kcp front on a staging VM's loopback, through an ssh forward to that VM
                let via = match (input["viaRoot"] == true, self.target.root_hosts.first()) {
                    (true, Some(h)) if self.target.name != "prod" => {
                        atlibs_kube::Via::Ssh { host: h.clone() }
                    }
                    _ => Default::default(),
                };
                r.map(|_| {
                    self.kube.add_cluster(
                        &format!("tenant:{name}"),
                        Cluster {
                            kubeconfig: path.display().to_string(),
                            workspace: None,
                            via,
                        },
                    );
                    json!({ "cluster": format!("tenant:{name}") })
                })
            }
            // a tenant kubeconfig minted and kept by the host (the module never holds it): over xrpc with the
            // account's service auth, or on the signed-in k8s page; registered as cluster tenant:<as>
            "gateway.kubeconfig" | "page.kubeconfig" => {
                let (Ok(name), Ok(alias)) = (str_of(input, "name"), str_of(input, "as")) else {
                    return Some(Err("name and as are required".into()));
                };
                let ttl = input["ttlSeconds"].as_u64().unwrap_or(3600);
                async {
                    let g = &self.target.gateway;
                    let kc = if what == "gateway.kubeconfig" {
                        let at = testkit::Atproto::new(&self.target);
                        let sess = at.session(self.acct()?).await.map_err(|e| e.to_string())?;
                        let v = at
                            .service_call(
                                &sess,
                                &g.url,
                                &g.aud,
                                "com.publicdomainrelay.temp.cluster.getKubeconfig",
                                Some(&json!({ "name": name, "ttlSeconds": ttl })),
                                &[],
                            )
                            .await
                            .map_err(|e| e.to_string())?;
                        v["kubeconfig"].as_str().map(String::from)
                    } else {
                        let v = self
                            .page_fetch(
                                &format!("{}/api/clusters/kubeconfig", g.url.trim_end_matches('/')),
                                "POST",
                                Some(&json!({ "name": name, "ttlSeconds": ttl })),
                            )
                            .await?;
                        if v["status"] != 200 {
                            return Err(format!(
                                "the page's /api/clusters/kubeconfig: HTTP {}",
                                v["status"]
                            ));
                        }
                        v["body"]["kubeconfig"].as_str().map(String::from)
                    }
                    .ok_or("no kubeconfig in the answer")?;
                    let path = self.dir.join(format!("tenant-{alias}.kubeconfig"));
                    write_private(&path, &kc)?;
                    self.kube.add_cluster(
                        &format!("tenant:{alias}"),
                        Cluster {
                            kubeconfig: path.display().to_string(),
                            workspace: None,
                            via: Default::default(),
                        },
                    );
                    Ok(json!({ "cluster": format!("tenant:{alias}") }))
                }
                .await
            }
            "page.signin" => self.page_signin(input).await,
            // an OAuth sign-in with no page session of ours (a QR session transfer): the account's consent, Authorize
            "oauth.signin" => match input["url"].as_str() {
                Some(url) => {
                    let mut extra = vec![];
                    if let Some(d) = input["done"].as_str() {
                        extra.extend(["--done", d]);
                    }
                    let r = match self.acct() {
                        Ok(a) => oauth_signin_with(a, url, &extra)
                            .await
                            .map_err(|e| e.to_string()),
                        Err(e) => Err(e),
                    };
                    Ok(json!({ "ok": r.is_ok(), "note": r.err() }))
                }
                None => Err("url is required".into()),
            },
            "page.fetch" => match input["url"].as_str() {
                Some(url) => {
                    self.page_fetch(
                        url,
                        input["method"].as_str().unwrap_or("GET"),
                        input.get("body"),
                    )
                    .await
                }
                None => Err("url is required".into()),
            },
            "ssh.tempKey" => self.temp_key().await,
            "atirc.observerPub" => match (str_of(input, "nick"), input["text"].as_str()) {
                (Ok(n), Some(t)) => self
                    .observer_run(n, &["pub", t])
                    .await
                    .map(|ok| json!({ "ok": ok })),
                _ => Err("nick and text are required".into()),
            },
            "atirc.observerRevoke" => match str_of(input, "nick") {
                Ok(n) => self
                    .observer_run(n, &["revoke"])
                    .await
                    .map(|ok| json!({ "ok": ok })),
                Err(e) => Err(e),
            },
            "requester.ssh" => {
                let (Ok(name), Ok(contract), Some(exec)) = (
                    str_of(input, "name"),
                    str_of(input, "contract"),
                    input["exec"].as_str(),
                ) else {
                    return Some(Err("name, contract, exec are required".into()));
                };
                let mut extra = vec!["--contract", contract, "--exec", exec];
                if let Some(t) = input["transport"].as_str() {
                    extra.extend(["--transport", t]);
                }
                self.requester_cli("ssh", name, &extra)
                    .await
                    .map(|(ok, out)| json!({ "ok": ok, "output": out }))
            }
            "requester.restart" => match str_of(input, "name") {
                Ok(n) => self.requester_restart(n).await,
                Err(e) => Err(e),
            },
            "requester.cluster" => {
                let (Ok(name), Ok(cluster)) = (str_of(input, "name"), str_of(input, "cluster"))
                else {
                    return Some(Err("name and cluster are required".into()));
                };
                async {
                    let (ok, out) = self
                        .requester_cli("cluster", name, &["--cluster", cluster])
                        .await?;
                    if !ok {
                        return Err(format!("run requester cluster: {}", tail(&out)));
                    }
                    let deadline = Instant::now() + Duration::from_secs(600);
                    let path = loop {
                        let (ok, out) = self
                            .requester_cli("kubeconfig", name, &["--cluster", cluster])
                            .await?;
                        if let Some(p) = out
                            .lines()
                            .map(str::trim)
                            .find(|l| l.starts_with('/'))
                            .filter(|_| ok)
                        {
                            break p.to_string();
                        }
                        if Instant::now() > deadline {
                            return Err(format!("cluster {cluster}: no kubeconfig in 10 min"));
                        }
                        tokio::time::sleep(Duration::from_secs(5)).await;
                    };
                    self.kube.add_cluster(
                        &format!("tenant:{cluster}"),
                        Cluster {
                            kubeconfig: path,
                            workspace: None,
                            via: Default::default(),
                        },
                    );
                    Ok(json!({ "cluster": format!("tenant:{cluster}") }))
                }
                .await
            }
            "requester.bundle" => self.requester_bundle(input).await,
            "requester.records" => match (str_of(input, "name"), self.acct()) {
                (Ok(name), Ok(a)) => {
                    let mut logs = vec![];
                    find_named(&self.requester_state(name), "core.log", &mut logs);
                    logs.push(self.requester_state(name).join(format!("{name}.log")));
                    Ok(
                        json!({ "uris": uris_in(&logs, &a.did).into_iter().filter(|u| !kept_by_rule(u)).collect::<Vec<_>>() }),
                    )
                }
                (Err(e), _) | (_, Err(e)) => Err(e),
            },
            "page.round" => {
                let Some(repo) = self.target.page_repo.clone() else {
                    return Some(Err("the target has no pageRepo".into()));
                };
                let bidder = input["bidder"].as_str().unwrap_or_default().to_string();
                async {
                    let a = self.acct()?;
                    let out = tokio::time::timeout(
                        Duration::from_secs(900),
                        tokio::process::Command::new("node")
                            .current_dir(expand(&repo))
                            .args(["scripts/page-round.mjs", &bidder, "uname -a; id"])
                            .env("ALICE_ATPROTO_HANDLE", &a.handle)
                            .env("ALICE_ATPROTO_PASSWORD", self.password()?)
                            .stdin(Stdio::null())
                            .output(),
                    )
                    .await
                    .map_err(|_| "page-round did not end in 15 min".to_string())?
                    .map_err(|e| e.to_string())?;
                    let text = format!("{}{}", String::from_utf8_lossy(&out.stdout), String::from_utf8_lossy(&out.stderr));
                    if text.contains(&self.password()?) {
                        return Err("page-round printed the password".into());
                    }
                    Ok(json!({ "ok": out.status.success(), "lines": text.lines().collect::<Vec<_>>() }))
                }
                .await
            }
            "example" => match input["name"]
                .as_str()
                .filter(|n| !n.contains('/') && n.ends_with(".yaml"))
            {
                Some(n) => std::fs::read_to_string(testkit::repo().join("examples/infra").join(n))
                    .map(|t| json!({ "text": t }))
                    .map_err(|e| format!("{n}: {e}")),
                None => Err("name is an examples/infra file".into()),
            },
            "request.r2" => {
                let (Ok(name), Ok(r2)) = (str_of(input, "name"), str_of(input, "r2Name")) else {
                    return Some(Err("name and r2Name are required".into()));
                };
                let mut extra = vec!["--r2-name", r2, "--hold"];
                if input["agent"] == true {
                    extra.push("--agent");
                }
                let args = self.sub_args("r2", name, &extra);
                self.cli(&args).await.and_then(|(ok, out)| {
                    if ok {
                        Ok(json!({}))
                    } else {
                        Err(format!("run request r2: {}", tail(&out)))
                    }
                })
            }
            "request.checkLogs" => match (str_of(input, "name"), self.password()) {
                (Ok(name), Ok(pw)) => {
                    let mut problems = vec![];
                    for p in self.request_logs(name) {
                        let Ok(text) = std::fs::read_to_string(&p) else {
                            continue;
                        };
                        if text.contains(&pw) {
                            problems.push(format!("{}: the app password", p.display()));
                        }
                        for bad in ["a=candidate", "AGE-SECRET-KEY"] {
                            if text.contains(bad) {
                                problems.push(format!("{}: plaintext {bad}", p.display()));
                            }
                        }
                    }
                    Ok(json!({ "ok": problems.is_empty(), "problems": problems }))
                }
                (Err(e), _) | (_, Err(e)) => Err(e),
            },
            "root.exec" => {
                let Some(host) = (self.target.name != "prod")
                    .then(|| self.target.root_hosts.first().cloned())
                    .flatten()
                else {
                    return Some(Err(
                        "root steps run only on a staging target with rootHosts".into(),
                    ));
                };
                let cmd = input["cmd"].as_str().unwrap_or_default().to_string();
                async {
                    let mut c = tokio::process::Command::new("ssh");
                    c.args(["-o", "BatchMode=yes", &host, &cmd]).stdout(Stdio::piped()).stderr(Stdio::piped()).stdin(Stdio::piped());
                    let mut child = c.spawn().map_err(|e| e.to_string())?;
                    let mut sin = child.stdin.take().unwrap();
                    if input["stdinPassword"] == true {
                        use tokio::io::AsyncWriteExt;
                        sin.write_all(self.password()?.as_bytes()).await.map_err(|e| e.to_string())?;
                    }
                    drop(sin);
                    let out = child.wait_with_output().await.map_err(|e| e.to_string())?;
                    Ok(json!({ "exit": out.status.code(), "stdout": String::from_utf8_lossy(&out.stdout), "stderr": tail(&String::from_utf8_lossy(&out.stderr)) }))
                }
                .await
            }
            "root.putBinary" => {
                let Some(host) = (self.target.name != "prod")
                    .then(|| self.target.root_hosts.first().cloned())
                    .flatten()
                else {
                    return Some(Err(
                        "root steps run only on a staging target with rootHosts".into(),
                    ));
                };
                let remote = "/root/swc-st-socialweb-computer";
                async {
                    let ok = tokio::process::Command::new("scp")
                        .args(["-q", "-o", "BatchMode=yes"])
                        .arg(&self.bin.path)
                        .arg(format!("{host}:{remote}"))
                        .status()
                        .await
                        .map_err(|e| e.to_string())?
                        .success();
                    if ok {
                        Ok(json!({ "path": remote }))
                    } else {
                        Err(format!("scp to {host} failed"))
                    }
                }
                .await
            }
            _ => return None,
        })
    }

    pub(crate) async fn more_sub(&self, what: &str, input: &Value) -> Option<Result<Sub, String>> {
        Some(match what {
            "atirc.observer" => self.observer_sub(input).await,
            "requester.events" => match str_of(input, "name") {
                Ok(n) => {
                    let state = self.requester_state(n);
                    let main = state.join(format!("{n}.log"));
                    Ok(tail_sub(
                        move || {
                            let mut v = vec![main.clone()];
                            find_named(&state, "core.log", &mut v);
                            v
                        },
                        None,
                        market_lines,
                    ))
                }
                Err(e) => Err(e),
            },
            _ => return None,
        })
    }

    pub(crate) async fn more_cleanup(&self) -> Vec<String> {
        let mut errors = vec![];
        let nicks: Vec<String> = self.extra.tokens.lock().unwrap().keys().cloned().collect();
        for n in nicks {
            if let Err(e) = self.observer_run(&n, &["revoke"]).await {
                errors.push(format!("revoke observer {n}: {e}"));
            }
        }
        let keys: Vec<_> = std::mem::take(&mut *self.extra.keys.lock().unwrap());
        for (web, rkey) in keys {
            if let Err(e) = self
                .page_fetch(&format!("{web}/keys/{rkey}"), "DELETE", None)
                .await
            {
                errors.push(format!("remove the temporary key: {e}"));
            }
        }
        errors
    }
}

impl Swc {
    /// resources.json: everything this run started or created, how to reach it, no secret (paths only).
    pub async fn resources(&self) -> Value {
        let requesters: Vec<Value> = self
            .requesters
            .lock()
            .await
            .iter()
            .map(|(n, (state, c))| {
                json!({ "name": n, "state": state, "pid": c.id(), "cluster": format!("requester:{n}"), "log": state.join(format!("{n}.log")),
                         "ssh": format!("socialweb-computer run requester ssh {n} --state-dir {} --contract <cc>", state.display()) })
            })
            .collect();
        let requests: Vec<Value> = self
            .requests
            .lock()
            .unwrap()
            .iter()
            .map(|(n, state)| {
                let summary: Value = std::fs::read(state.join("requests").join(n).join("summary.json")).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default();
                json!({ "name": n, "state": state, "receipt": summary["contract"]["receiptUri"], "winner": summary["contract"]["winnerDid"], "logs": self.request_logs(n),
                         "ssh": format!("socialweb-computer run request ssh {n} --state-dir {}", state.display()) })
            })
            .collect();
        json!({
            "account": self.acct().map(|a| a.did.clone()).unwrap_or_default(),
            "requesters": requesters,
            "requests": requests,
            "kube": self.kube.ledger(),
            "clusters": self.kube.clusters(),
            "receipts": *self.receipts.lock().unwrap(),
            "keys": self.extra.keys.lock().unwrap().iter().map(|(w, r)| json!({ "web": w, "rkey": r })).collect::<Vec<_>>(),
            "observers": self.extra.tokens.lock().unwrap().keys().cloned().collect::<Vec<_>>(),
            "envFiles": *self.env_files.lock().unwrap(),
        })
    }

    pub async fn write_resources(&self) {
        let v = self.resources().await;
        let _ = std::fs::write(
            self.dir.join("resources.json"),
            serde_json::to_string_pretty(&v).unwrap_or_default(),
        );
    }
}

/// `swc-test-host cleanup <run>`: from resources.json alone (the host that ran it may be gone): release the
/// invocations, stop the requesters, delete and restore kube objects, remove the temporary keys, revoke observer
/// tokens (a fresh account token revokes the subhandle), remove the env files. The errors.
pub async fn cleanup_from_disk(
    dir: &std::path::Path,
    target: &testkit::target::Target,
    account: &str,
    bin: &testkit::requester::Binary,
) -> Vec<String> {
    let mut errors = vec![];
    let Ok(text) = std::fs::read_to_string(dir.join("resources.json")) else {
        return vec![format!("{}: no resources.json", dir.display())];
    };
    let r: Value = serde_json::from_str(&text).unwrap_or_default();
    for q in r["requests"].as_array().into_iter().flatten() {
        let (n, st) = (
            q["name"].as_str().unwrap_or_default(),
            q["state"].as_str().unwrap_or_default(),
        );
        match bin
            .run(&["run", "request", "release", n, "--state-dir", st])
            .await
        {
            Ok((true, _)) => {}
            Ok((false, out)) => errors.push(format!("release {n}: {}", tail(&out))),
            Err(e) => errors.push(format!("release {n}: {e}")),
        }
    }
    for q in r["requesters"].as_array().into_iter().flatten() {
        if let Some(pid) = q["pid"].as_u64()
            && std::path::Path::new(&format!("/proc/{pid}")).exists()
        {
            let _ = std::process::Command::new("kill")
                .args(["-INT", &pid.to_string()])
                .status();
        }
    }
    let clusters: HashMap<String, Cluster> =
        serde_json::from_value(r["clusters"].clone()).unwrap_or_default();
    let kube = atlibs_kube::Kube::new(clusters, None);
    if let Err(e) = kube.load_ledger(&r["kube"]) {
        errors.push(format!("kube ledger: {e}"));
    }
    errors.extend(kube.cleanup().await);
    let Ok(acct) = target.account(account) else {
        return errors;
    };
    let at = testkit::Atproto::new(target);
    let keys = r["keys"].as_array().is_some_and(|k| !k.is_empty());
    if keys && let Ok(s) = at.session(acct).await {
        for k in r["keys"].as_array().into_iter().flatten() {
            if let Err(e) = at
                .delete_record(
                    &s,
                    "com.publicdomainrelay.temp.badgeBlueKeys",
                    k["rkey"].as_str().unwrap_or_default(),
                )
                .await
            {
                errors.push(format!("temporary key: {e}"));
            }
        }
    }
    if let (Some(endpoint), Ok(atirc)) = (target.atirc.clone(), testkit::atirc::atirc_bin()) {
        for nick in r["observers"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(Value::as_str)
        {
            let pw = acct
                .password()
                .map(|p| p.expose().to_string())
                .unwrap_or_default();
            let out = tokio::process::Command::new(&atirc)
                .args(["token", "--endpoint", &endpoint])
                .env("ATP_USERNAME", &acct.handle)
                .env("ATP_PASSWORD", pw)
                .stdin(Stdio::null())
                .output()
                .await;
            let Ok(out) = out else { continue };
            let token = String::from_utf8_lossy(&out.stdout).trim().to_string();
            let ok = tokio::process::Command::new(&atirc)
                .args(["revoke", &format!("{nick}.{}", acct.handle)])
                .env("ATIRC_TOKEN", token)
                .env("ATIRC_ENDPOINT", &endpoint)
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .status()
                .await
                .is_ok_and(|s| s.success());
            if !ok {
                errors.push(format!("revoke observer {nick}"));
            }
        }
    }
    for p in r["envFiles"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
    {
        let _ = std::fs::remove_file(p);
    }
    errors
}
