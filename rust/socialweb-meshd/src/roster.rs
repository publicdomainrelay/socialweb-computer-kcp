use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use tokio::sync::watch;

use crate::nodes::{Node, NodeSpec, ObjectMeta};
use crate::routes::{ANNOTATION_ADDRS, ANNOTATION_ENDPOINT};

pub const ROSTER_POLL: Duration = Duration::from_secs(5);
pub const ROSTER_FETCH_TIMEOUT: Duration = Duration::from_secs(10);
pub const ROSTER_LIMIT: usize = 1 << 20;
pub const ANNOTATION_ROSTER: &str = "socialweb.computer/mesh-roster";

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct Peer {
    pub name: String,
    pub endpoint: String,
    #[serde(default)]
    pub cidrs: Vec<String>,
    #[serde(default)]
    pub addrs: Vec<String>,
    #[serde(default)]
    pub names: Vec<String>,
    #[serde(default)]
    pub txt: Vec<Txt>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct Txt {
    pub name: String,
    pub value: String,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct Roster {
    #[serde(default)]
    pub peers: Vec<Peer>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum Document {
    Record { value: Roster },
    Plain(Roster),
}

pub fn parse(bytes: &[u8]) -> Result<Roster, String> {
    match serde_json::from_slice::<Document>(bytes) {
        Ok(Document::Record { value } | Document::Plain(value)) => Ok(value),
        Err(err) => Err(format!("roster: {err}")),
    }
}

impl Peer {
    fn is(&self, endpoint_hex: &str) -> bool {
        self.endpoint.trim().eq_ignore_ascii_case(endpoint_hex)
    }

    fn node(&self, name: &str) -> Node {
        let mut annotations = BTreeMap::from([(
            ANNOTATION_ENDPOINT.to_string(),
            self.endpoint.trim().to_ascii_lowercase(),
        )]);
        if !self.addrs.is_empty() {
            annotations.insert(ANNOTATION_ADDRS.to_string(), self.addrs.join(","));
        }
        Node {
            metadata: ObjectMeta {
                name: name.to_string(),
                annotations,
                resource_version: None,
            },
            spec: NodeSpec {
                pod_cidr: None,
                pod_cidrs: self.cidrs.clone(),
            },
        }
    }
}

impl Roster {
    pub fn self_name(&self, endpoint_hex: &str) -> Option<String> {
        self.peers
            .iter()
            .find(|peer| peer.is(endpoint_hex))
            .map(|peer| peer.name.clone())
    }

    pub fn nodes(&self) -> Vec<Node> {
        self.peers
            .iter()
            .map(|peer| peer.node(&peer.name))
            .collect()
    }

    pub fn beside(&self, kube: &[Node], endpoint_hex: &str) -> Vec<Node> {
        let names: BTreeSet<&str> = kube.iter().map(|n| n.metadata.name.as_str()).collect();
        let endpoints: BTreeSet<String> = kube
            .iter()
            .filter_map(|n| n.annotation(ANNOTATION_ENDPOINT))
            .map(str::to_ascii_lowercase)
            .collect();
        let mut out = kube.to_vec();
        for peer in &self.peers {
            let endpoint = peer.endpoint.trim().to_ascii_lowercase();
            if peer.is(endpoint_hex)
                || names.contains(peer.name.as_str())
                || endpoints.contains(&endpoint)
            {
                continue;
            }
            out.push(peer.node(&peer.name));
        }
        out
    }
}

pub fn http_client() -> Result<reqwest::Client, String> {
    let roots = rustls::RootCertStore {
        roots: webpki_roots::TLS_SERVER_ROOTS.to_vec(),
    };
    let tls = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .map_err(|err| err.to_string())?
    .with_root_certificates(roots)
    .with_no_client_auth();
    reqwest::Client::builder()
        .tls_backend_preconfigured(tls)
        .timeout(ROSTER_FETCH_TIMEOUT)
        .build()
        .map_err(|err| format!("http client: {err}"))
}

fn chain(err: &dyn std::error::Error) -> String {
    let mut text = err.to_string();
    let mut next = err.source();
    while let Some(cause) = next {
        text.push_str(&format!(": {cause}"));
        next = cause.source();
    }
    text
}

pub async fn load(http: &reqwest::Client, location: &str) -> Result<Roster, String> {
    if !(location.starts_with("https://") || location.starts_with("http://")) {
        let bytes = tokio::fs::read(location)
            .await
            .map_err(|err| format!("read {location}: {err}"))?;
        return parse(&bytes);
    }
    let mut response = http
        .get(location)
        .header("accept", "application/json")
        .send()
        .await
        .map_err(|err| format!("GET {location}: {}", chain(&err)))?;
    if !response.status().is_success() {
        return Err(format!("GET {location}: {}", response.status()));
    }
    let mut body = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|err| format!("GET {location}: {}", chain(&err)))?
    {
        body.extend_from_slice(&chunk);
        if body.len() > ROSTER_LIMIT {
            return Err(format!("GET {location}: larger than {ROSTER_LIMIT} bytes"));
        }
    }
    parse(&body)
}

pub async fn run(
    http: reqwest::Client,
    mut location: watch::Receiver<Option<String>>,
    tx: watch::Sender<Arc<Roster>>,
) {
    let mut last_error = String::new();
    let mut followed = true;
    loop {
        let current = location.borrow_and_update().clone();
        let next = match &current {
            Some(location) => load(&http, location).await,
            None => Ok(Roster::default()),
        };
        match next {
            Ok(roster) => {
                last_error.clear();
                tx.send_if_modified(|old| {
                    if **old == roster {
                        return false;
                    }
                    tracing::info!(
                        event = "roster",
                        location = current.as_deref().unwrap_or(""),
                        peers = ?roster.peers.iter().map(|p| p.name.as_str()).collect::<Vec<_>>()
                    );
                    *old = Arc::new(roster);
                    true
                });
            }
            Err(err) if err != last_error => {
                tracing::warn!(event = "roster_unreadable", error = %err);
                last_error = err;
            }
            Err(_) => {}
        }
        tokio::select! {
            changed = location.changed(), if followed => followed = changed.is_ok(),
            () = tokio::time::sleep(ROSTER_POLL) => {}
        }
    }
}
