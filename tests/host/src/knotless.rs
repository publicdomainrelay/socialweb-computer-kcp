//! computer.socialweb.test.knotless.*: a staging network's knotless (dev up --with-knotless) as the target's account,
//! its gateway owner. `seed` publishes the knotless runner's composition, its modules and the bindings in the
//! account's repo (the runner trusts only it), from the published composition the target's `knotless.source` names
//! (its modules' wasm checked against their pins), its records renamed into the account's repo, discovering bidders by
//! vouches only (no relay: a requester module takes https relays, the network's is http); `sh` runs a script with the knotless CLI, git
//! and the network's env (client.env, the account's password). Every record seeded and every `cleanup` script a
//! call names is removed or run by `knotless_cleanup()`.

use std::collections::HashMap;
use std::sync::Arc;

use atlibs_host::registry::{self, COMPOSITION, MODULE, Net};
use serde_json::{Value, json};
use testkit::requester::tail;

use super::Swc;

const BINDING: &str = "com.fedfork.exec.binding";

fn env_file(path: &str) -> Result<HashMap<String, String>, String> {
    let text = std::fs::read_to_string(path)
        .map_err(|e| format!("{path}: {e} (dev up --with-knotless writes it)"))?;
    Ok(text
        .lines()
        .filter_map(|l| l.split_once('='))
        .map(|(k, v)| (k.trim().to_string(), v.trim().to_string()))
        .collect())
}

struct Pds {
    http: reqwest::Client,
    base: String,
    did: String,
    jwt: String,
}

impl Pds {
    async fn login(base: &str, did: &str, password: &str) -> Result<Pds, String> {
        let http = reqwest::Client::new();
        let r = http
            .post(format!("{base}/xrpc/com.atproto.server.createSession"))
            .json(&json!({ "identifier": did, "password": password }))
            .send()
            .await
            .map_err(|e| e.to_string())?;
        let status = r.status();
        let body: Value = r.json().await.map_err(|e| e.to_string())?;
        let jwt = body["accessJwt"]
            .as_str()
            .ok_or_else(|| format!("createSession {did}: {status} {}", body["error"]))?
            .to_string();
        Ok(Pds {
            http,
            base: base.trim_end_matches('/').into(),
            did: did.into(),
            jwt,
        })
    }

    async fn post(&self, nsid: &str, body: reqwest::Body, kind: &str) -> Result<Value, String> {
        let r = self
            .http
            .post(format!("{}/xrpc/{nsid}", self.base))
            .bearer_auth(&self.jwt)
            .header("content-type", kind)
            .body(body)
            .send()
            .await
            .map_err(|e| e.to_string())?;
        let status = r.status();
        let v: Value = r.json().await.unwrap_or_default();
        if !status.is_success() {
            return Err(format!("{nsid}: {status} {} {}", v["error"], v["message"]));
        }
        Ok(v)
    }

    async fn put(
        &self,
        collection: &str,
        rkey: &str,
        record: &Value,
    ) -> Result<(String, String), String> {
        let body = json!({ "repo": self.did, "collection": collection, "rkey": rkey, "record": record, "validate": false });
        let v = self
            .post(
                "com.atproto.repo.putRecord",
                body.to_string().into(),
                "application/json",
            )
            .await?;
        Ok((
            v["uri"].as_str().unwrap_or_default().into(),
            v["cid"].as_str().unwrap_or_default().into(),
        ))
    }

    async fn delete(&self, uri: &str) -> Result<(), String> {
        let (_, collection, rkey) =
            registry::split_uri(uri).ok_or_else(|| format!("{uri} is not a record URI"))?;
        let body = json!({ "repo": self.did, "collection": collection, "rkey": rkey });
        self.post(
            "com.atproto.repo.deleteRecord",
            body.to_string().into(),
            "application/json",
        )
        .await
        .map(|_| ())
    }
}

impl Swc {
    fn knotless_env(&self) -> Result<HashMap<String, String>, String> {
        let k =
            self.target.knotless.as_ref().ok_or(
                "the target names no knotless (a staging network's dev up --with-knotless)",
            )?;
        env_file(&k.env)
    }

    async fn knotless_pds(&self) -> Result<Pds, String> {
        let env = self.knotless_env()?;
        let did = env
            .get("KNOTLESS_IDENTIFIER")
            .ok_or("client.env names no KNOTLESS_IDENTIFIER")?;
        if *did != self.acct()?.did {
            return Err(format!(
                "the knotless runner's account is {did}, not the target account {}",
                self.acct()?.did
            ));
        }
        Pds::login(
            env.get("KNOTLESS_PDS")
                .ok_or("client.env names no KNOTLESS_PDS")?,
            did,
            &self.password()?,
        )
        .await
    }

    async fn knotless_seed(&self, input: &Value) -> Result<Value, String> {
        let k = self
            .target
            .knotless
            .as_ref()
            .ok_or("the target names no knotless")?;
        let env = self.knotless_env()?;
        let pds = self.knotless_pds().await?;
        let mut prod = Net::new(Arc::new(reqwest::Client::new()));
        prod.plc = k.source_plc.clone();
        let (_, mut comp) = registry::composition(&prod, &k.source, None).await?;
        let (_, _, libs) = registry::published(&prod, &k.source, None).await?;
        let (publisher, _, _) =
            registry::split_uri(&k.source).ok_or("knotless.source is not a record URI")?;
        let (from, to) = (format!("at://{publisher}/"), format!("at://{}/", pds.did));
        let mut seeded = vec![];
        for (_, lib) in &libs {
            let uri = lib
                .uri
                .clone()
                .ok_or_else(|| format!("{}: no record URI", lib.key))?;
            let (did, _, _) =
                registry::split_uri(&uri).ok_or_else(|| format!("{uri} is not a record URI"))?;
            let pinned = registry::version_rkey(&lib.key, &lib.pin())
                .map(|v| format!("at://{did}/{MODULE}/{v}"));
            let found = match &pinned {
                Some(at) => prod.record(at).await?,
                None => None,
            };
            let (_, mut record) = match found {
                Some(r) => r,
                None => prod
                    .record(&uri)
                    .await?
                    .ok_or_else(|| format!("{uri} not found"))?,
            };
            let blob = pds
                .post(
                    "com.atproto.repo.uploadBlob",
                    lib.wasm.clone().into(),
                    "application/wasm",
                )
                .await?;
            if blob["blob"]["ref"] != record["module"]["ref"] {
                return Err(format!(
                    "{uri}: its wasm uploads as {}, not {}",
                    blob["blob"]["ref"], record["module"]["ref"]
                ));
            }
            if let Some(client) = &lib.client {
                let (c_uri, c_cid) = pds
                    .put(registry::CLIENT, &client.rkey, &client.record)
                    .await?;
                record["session"] = json!({ "uri": c_uri, "cid": c_cid });
                seeded.push(c_uri);
            }
            if let Some(o) = record.as_object_mut() {
                o.remove("source");
            }
            let record: Value = serde_json::from_str(&record.to_string().replace(&from, &to))
                .map_err(|e| e.to_string())?;
            let mut rkeys = vec![lib.key.clone()];
            rkeys.extend(registry::version_rkey(&lib.key, &lib.pin()));
            for rkey in rkeys {
                seeded.push(pds.put(MODULE, &rkey, &record).await?.0);
            }
        }
        for inst in comp["instances"].as_array_mut().into_iter().flatten() {
            if let Some(m) = inst["module"].as_str().map(|m| m.replace(&from, &to)) {
                inst["module"] = json!(m);
            }
            if inst["module"].as_str().and_then(|m| m.rsplit('/').next()) == Some("requester") {
                inst["input"]["discovery_relays"] = json!([]);
            }
        }
        let (_, _, rkey) =
            registry::split_uri(&k.source).ok_or("knotless.source is not a record URI")?;
        if comp.to_string().contains(&from) {
            return Err(format!(
                "the composition still names {from} after the rewrite"
            ));
        }
        let (c_uri, cid) = pds.put(COMPOSITION, rkey, &comp).await?;
        seeded.push(c_uri.clone());
        let version = registry::composition_version(rkey, &cid)
            .ok_or("the composition's CID makes no version rkey")?;
        seeded.push(pds.put(COMPOSITION, &version, &comp).await?.0);
        let mut bindings = json!({});
        for (rkey, inputs) in input["bindings"].as_object().into_iter().flatten() {
            let record = json!({ "$type": BINDING, "composition": { "uri": c_uri, "cid": cid }, "inputs": inputs, "createdAt": atlibs_host::iso_now() });
            let (uri, _) = pds.put(BINDING, rkey, &record).await?;
            bindings[rkey] = json!(uri);
            seeded.push(uri);
        }
        self.extra
            .knotless
            .lock()
            .unwrap()
            .extend(seeded.iter().map(|u| format!("record {u}")));
        Ok(
            json!({ "composition": { "uri": c_uri, "cid": cid }, "modules": libs.len(), "bindings": bindings }),
        )
    }

    async fn knotless_sh(&self, script: &str, timeout: u64) -> Result<Value, String> {
        let env = self.knotless_env()?;
        let work = self.dir.join("knotless");
        std::fs::create_dir_all(&work).map_err(|e| e.to_string())?;
        let bin = env.get("KNOTLESS_BIN").cloned().unwrap_or_default();
        let mut c = tokio::process::Command::new("sh");
        c.args(["-euc", script])
            .current_dir(&work)
            .env_clear()
            .envs(&env)
            .env("PATH", format!("{bin}:/usr/local/bin:/usr/bin:/bin"))
            .env("HOME", &work)
            .env("KNOTLESS_CONFIG_DIR", work.join("config"))
            .env("KNOTLESS_CACHE_DIR", work.join("cache"))
            .env("KNOTLESS_PASSWORD", self.password()?)
            .env("GIT_AUTHOR_NAME", "swc-test")
            .env("GIT_AUTHOR_EMAIL", "swc-test@knotless.invalid")
            .env("GIT_COMMITTER_NAME", "swc-test")
            .env("GIT_COMMITTER_EMAIL", "swc-test@knotless.invalid")
            .stdin(std::process::Stdio::null())
            .kill_on_drop(true);
        let out = tokio::time::timeout(std::time::Duration::from_secs(timeout), c.output())
            .await
            .map_err(|_| format!("the script ran past {timeout} s"))?
            .map_err(|e| e.to_string())?;
        let stdout = String::from_utf8_lossy(&out.stdout);
        let keep = stdout.len().saturating_sub(65536);
        Ok(
            json!({ "exit": out.status.code(), "stdout": &stdout[stdout.ceil_char_boundary(keep)..], "stderr": tail(&String::from_utf8_lossy(&out.stderr)) }),
        )
    }

    pub(crate) async fn knotless(
        &self,
        what: &str,
        input: &Value,
    ) -> Option<Result<Value, String>> {
        Some(match what {
            "knotless.seed" => self.knotless_seed(input).await,
            "knotless.sh" => {
                if let Some(c) = input["cleanup"].as_str() {
                    self.extra.knotless.lock().unwrap().push(format!("sh {c}"));
                }
                match input["script"].as_str() {
                    Some(s) => {
                        self.knotless_sh(s, input["timeoutSecs"].as_u64().unwrap_or(600))
                            .await
                    }
                    None => Ok(json!({})),
                }
            }
            _ => return None,
        })
    }

    /// Every cleanup script, newest first, then every seeded record deleted. The errors.
    pub(crate) async fn knotless_cleanup(&self) -> Vec<String> {
        let todo: Vec<String> = std::mem::take(&mut *self.extra.knotless.lock().unwrap());
        let mut errors = vec![];
        let mut pds = None;
        for t in todo.iter().rev() {
            if let Some(script) = t.strip_prefix("sh ") {
                match self.knotless_sh(script, 300).await {
                    Ok(o) if o["exit"] == 0 => {}
                    Ok(o) => errors.push(format!(
                        "knotless cleanup {script}: exit {} {}",
                        o["exit"], o["stderr"]
                    )),
                    Err(e) => errors.push(format!("knotless cleanup {script}: {e}")),
                }
            } else if let Some(uri) = t.strip_prefix("record ") {
                if pds.is_none() {
                    pds = Some(self.knotless_pds().await);
                }
                if let Some(Ok(p)) = &pds
                    && let Err(e) = p.delete(uri).await
                {
                    errors.push(format!("delete {uri}: {e}"));
                }
            }
        }
        if let Some(Err(e)) = pds {
            errors.push(format!("knotless cleanup: {e}"));
        }
        errors
    }
}
