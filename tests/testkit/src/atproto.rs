//! PDS, PLC and gateway XRPC through atlibs-host: app-password sessions saved per account under
//! ~/.tmp/swc-system-tests/sessions (createSession is rate limited), service auth, getRecord at computed keys.

use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail};
use atlibs_host::host::{Fetch, Session, mint};
use atlibs_host::registry::Net;
use atlibs_host::store::Store;
use serde_json::{Value, json};

use crate::target::{Account, Target};

pub struct Atproto {
    pub net: Net,
    pub http: reqwest::Client,
    plc: String,
    pds: String,
}

pub fn home_tmp(sub: &str) -> std::path::PathBuf {
    std::path::PathBuf::from(std::env::var("HOME").unwrap_or_default()).join(".tmp/swc-system-tests").join(sub)
}

impl Atproto {
    pub fn new(t: &Target) -> Self {
        atlibs_host::tls();
        let http = reqwest::Client::builder().timeout(Duration::from_secs(30)).build().unwrap();
        let mut net = Net::new(Arc::new(http.clone()) as Arc<dyn Fetch>);
        net.plc = t.plc.clone();
        // staging lives on private overlay addresses
        net.allow_private = t.name != "prod";
        Self { net, http, plc: t.plc.trim_end_matches('/').into(), pds: t.pds.clone() }
    }

    /// The record's value; None for RecordNotFound.
    pub async fn record(&self, uri: &str) -> Result<Option<Value>> {
        Ok(self.net.record(uri).await.map_err(|e| anyhow!(e))?.map(|(_, v)| v))
    }

    pub async fn record_of(&self, did: &str, collection: &str, rkey: &str) -> Result<Option<Value>> {
        self.record(&format!("at://{did}/{collection}/{rkey}")).await
    }

    /// The URIs that still answer (each read three times, as the runbooks do). Empty: all RecordNotFound.
    pub async fn still_present(&self, uris: &[String]) -> Result<Vec<String>> {
        let mut left = vec![];
        for uri in uris {
            for _ in 0..3 {
                if self.record(uri).await?.is_some() {
                    left.push(uri.clone());
                    break;
                }
            }
        }
        Ok(left)
    }

    async fn plc_get(&self, path: &str) -> Result<(u16, Value)> {
        let r = self.http.get(format!("{}/{path}", self.plc)).send().await?;
        let status = r.status().as_u16();
        Ok((status, r.json().await.unwrap_or(Value::Null)))
    }

    /// The PLC document (None: 404, a tombstone on plc.directory).
    pub async fn did_doc(&self, did: &str) -> Result<Option<Value>> {
        let (s, v) = self.plc_get(did).await?;
        Ok((s == 200).then_some(v))
    }

    /// The type of the DID's last PLC op: plc_operation, plc_tombstone, ...
    pub async fn plc_last_type(&self, did: &str) -> Result<String> {
        let (_, v) = self.plc_get(&format!("{did}/log/last")).await?;
        Ok(v["type"].as_str().unwrap_or_default().into())
    }

    pub async fn tombstoned(&self, did: &str) -> Result<bool> {
        Ok(self.plc_last_type(did).await? == "plc_tombstone" && self.did_doc(did).await?.is_none())
    }

    pub async fn also_known_as(&self, did: &str) -> Result<Vec<String>> {
        let doc = self.did_doc(did).await?.with_context(|| format!("{did} has no PLC document"))?;
        Ok(serde_json::from_value(doc["alsoKnownAs"].clone()).unwrap_or_default())
    }

    pub async fn rotation_keys(&self, did: &str) -> Result<Vec<String>> {
        let (_, v) = self.plc_get(&format!("{did}/data")).await?;
        Ok(serde_json::from_value(v["rotationKeys"].clone()).unwrap_or_default())
    }

    /// The account's app-password session, saved under ~/.tmp/swc-system-tests/sessions/<handle>.
    pub async fn session(&self, account: &Account) -> Result<Arc<Session>> {
        let store = Store::open(&home_tmp("sessions").join(&account.handle), "testkit").map_err(|e| anyhow!(e))?;
        let password = account.password()?;
        let fetch: Arc<dyn Fetch> = Arc::new(self.http.clone());
        Session::login(&account.handle, Some(password.expose().to_string()), &self.pds, fetch, Some(store)).await.map_err(|e| anyhow!("signing in {}: {e}", account.handle))
    }

    pub async fn delete_record(&self, s: &Session, collection: &str, rkey: &str) -> Result<()> {
        s.xrpc("com.atproto.repo.deleteRecord", None, Some(&json!({ "repo": s.did(), "collection": collection, "rkey": rkey })), vec![]).await.map_err(|e| anyhow!(e))?;
        Ok(())
    }

    /// An XRPC call to a service, authenticated by a service-auth token the session mints for `aud` + the method.
    pub async fn service_call(&self, s: &Session, base: &str, aud: &str, nsid: &str, input: Option<&Value>, query: &[(&str, &str)]) -> Result<Value> {
        let tok = mint(s, aud, nsid).await.map_err(|e| anyhow!(e))?;
        let tok = tok["token"].as_str().context("getServiceAuth answered no token")?.to_string();
        let url = format!("{}/xrpc/{nsid}", base.trim_end_matches('/'));
        let req = match input {
            Some(body) => self.http.post(url).json(body),
            None => self.http.get(url).query(query),
        };
        let r = req.bearer_auth(tok).send().await?;
        let status = r.status();
        let v: Value = r.json().await.unwrap_or(Value::Null);
        if !status.is_success() {
            bail!("{nsid}: HTTP {status} {}", v);
        }
        Ok(v)
    }
}
