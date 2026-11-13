//! target.json: what a run talks to (staging or prod). Path in SWC_TARGET; tests/targets/prod.json is prod.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use serde::Deserialize;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Target {
    /// "prod" or "staging"
    pub name: String,
    pub plc: String,
    /// entryway or PDS for app-password sessions of the accounts (staging: the staging PDS)
    pub pds: String,
    #[serde(default)]
    pub relay: Option<String>,
    #[serde(default)]
    pub jetstream: Option<String>,
    pub gateway: Gateway,
    #[serde(default)]
    pub atirc: Option<String>,
    /// the requester binary: "release" (newest v* tag), "release:<tag>", "sha:<commit>", "path:<file>"
    #[serde(default = "default_requester")]
    pub requester: String,
    #[serde(default)]
    pub bidders: Vec<Bidder>,
    pub accounts: BTreeMap<String, Account>,
    /// hosts a test may administer as root (staging VMs only: `install requester`, a temporary bidder); ssh destinations
    #[serde(default)]
    pub root_hosts: Vec<String>,
    /// staging inside one VM: the `dev up --profile market-network` instance every `run request` joins (`--network`)
    #[serde(default)]
    pub network: Option<NetworkRef>,
    /// a checkout of the requester page repo (atlibs-socialweb-computer-requester): scripts/page-round.mjs (0009)
    #[serde(default)]
    pub page_repo: Option<String>,
    /// atirc agent config (harness, model) for agent VMs; mode 0600, never printed
    #[serde(default)]
    pub agent_config: Option<String>,
    /// a staging network's knotless (dev up --with-knotless)
    #[serde(default)]
    pub knotless: Option<Knotless>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Knotless {
    /// the gateway's knotless/client.env: the appview, the runner, the PLC, the CA, the egress; no secret
    pub env: String,
    /// the published composition the runner's is seeded from (at://<did>/com.fedfork.exec.composition/<rkey>)
    pub source: String,
    /// where its publisher's DID resolves
    #[serde(default = "default_source_plc")]
    pub source_plc: String,
}

fn default_source_plc() -> String {
    "https://plc.directory".into()
}

fn default_requester() -> String {
    "release".into()
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Gateway {
    /// https://k8s.socialweb.computer
    pub url: String,
    /// did:web:k8s.socialweb.computer
    pub aud: String,
    /// ssh gateway host (port 22, `ssh -F /dev/null`)
    pub ssh: String,
    #[serde(default)]
    pub ssh_port: Option<u16>,
    /// the gateway's requester DID (its tenant nodes' VMs on the bidders: 0001 section 6 orphan checks)
    #[serde(default)]
    pub requester_did: Option<String>,
    /// the gateway's own kcp (fault injection, NodeClaim winners); absent on prod unless the lead grants it
    #[serde(default)]
    pub kcp: Option<KcpEndpoint>,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct Bidder {
    pub name: String,
    pub did: String,
    pub operator: String,
    /// the bidder's kcp: host-empty checks, pause, fault injection. Absent: those readings are skipped
    #[serde(default)]
    pub kcp: Option<KcpEndpoint>,
}

/// How to reach one kcp: its kubeconfig (credentials, client certs, CA) and a transport.
#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct KcpEndpoint {
    pub kubeconfig: String,
    /// "/clusters/<workspace>" path to put on the kubeconfig's server URL (when it names the root)
    #[serde(default)]
    pub workspace: Option<String>,
    #[serde(default)]
    pub via: Via,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[serde(rename_all = "camelCase", tag = "kind")]
pub enum Via {
    /// the kubeconfig's server URL as is
    #[default]
    Direct,
    /// the same port on a meshd overlay name (resolved by the overlay's DNS)
    Overlay { name: String },
    /// a dumbpipe ticket that forwards to the kcp front
    Iroh { ticket: String },
    /// an ssh forward through `host` (an ssh alias) to the kubeconfig's host:port (a bidder's front on its loopback)
    Ssh { host: String },
    /// WebRTC: a command that pipes stdin/stdout through a datachannel to the kcp's port, one run per connection.
    /// Requester::webrtc_proxy builds it (`run requester proxy --contract <cc> --tag <tag>`: the Go requester's offerer)
    WebRtc { proxy: Vec<String> },
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Account {
    pub handle: String,
    pub did: String,
    /// shell file exporting <PREFIX>ATPROTO_HANDLE and <PREFIX>ATPROTO_PASSWORD; read here only, never printed
    #[serde(default)]
    pub creds: String,
    /// the guest requester R2 of the VM this runs on (knotless CI): `run request --guest-requester`, as the
    /// renter's contract-scoped agent (its association-ref in /run/socialweb-r2); no creds, no session
    #[serde(default)]
    pub guest_requester: bool,
    /// the oauth-signin.ts --account prefix (ALICE, BOB); none: app password only
    #[serde(default)]
    pub oauth_account: Option<String>,
    /// staging throwaway account: tombstoned at teardown
    #[serde(default)]
    pub throwaway: bool,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct NetworkRef {
    pub instance: String,
    /// the instance's --state-dir; a request's state lives under it too (`run request --network` reads its record there)
    pub state_dir: String,
}

pub fn expand(path: &str) -> PathBuf {
    match path.strip_prefix("~/") {
        Some(rest) => PathBuf::from(std::env::var("HOME").unwrap_or_default()).join(rest),
        None => PathBuf::from(path),
    }
}

impl Target {
    pub fn load() -> Result<Self> {
        let path = std::env::var("SWC_TARGET")
            .context("SWC_TARGET names a target.json (tests/targets/prod.json for prod)")?;
        let path = expand(&path);
        let t: Target = serde_json::from_slice(
            &std::fs::read(&path).with_context(|| path.display().to_string())?,
        )
        .with_context(|| format!("parsing {}", path.display()))?;
        Ok(t)
    }

    pub fn account(&self, name: &str) -> Result<&Account> {
        self.accounts
            .get(name)
            .with_context(|| format!("target {} has no account {name}", self.name))
    }

    pub fn bidder_by_did(&self, did: &str) -> Option<&Bidder> {
        self.bidders.iter().find(|b| b.did == did)
    }
}

/// An account's app password, read from its creds file. Debug never shows it.
pub struct Secret(String);

impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("<secret>")
    }
}

impl Secret {
    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl Account {
    /// Its DID: the guest requester's, once its first `run request` wrote it, from `/run/socialweb-r2/requester-did`.
    pub fn did_now(&self) -> String {
        match self.guest_requester {
            true => std::fs::read_to_string("/run/socialweb-r2/requester-did")
                .map(|d| d.trim().to_string())
                .unwrap_or_default(),
            false => self.did.clone(),
        }
    }

    /// The app password: the first variable ending in ATPROTO_PASSWORD. The file must be owner-only.
    pub fn password(&self) -> Result<Secret> {
        let path = expand(&self.creds);
        check_private(&path)?;
        let text = std::fs::read_to_string(&path).with_context(|| path.display().to_string())?;
        for line in text.lines() {
            let line = line.trim().trim_start_matches("export ").trim();
            if let Some((k, v)) = line.split_once('=')
                && k.ends_with("ATPROTO_PASSWORD")
            {
                return Ok(Secret(
                    v.trim().trim_matches(|c| c == '"' || c == '\'').to_string(),
                ));
            }
        }
        bail!("{}: no *ATPROTO_PASSWORD", path.display())
    }
}

pub fn check_private(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let mode = std::fs::metadata(path)
        .with_context(|| path.display().to_string())?
        .permissions()
        .mode();
    if mode & 0o077 != 0 {
        bail!(
            "{} is mode {:o}: chmod 600 it (owner-only)",
            path.display(),
            mode & 0o777
        );
    }
    Ok(())
}
