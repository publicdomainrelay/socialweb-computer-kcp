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
    /// atirc agent config (harness, model) for agent VMs; mode 0600, never printed
    #[serde(default)]
    pub agent_config: Option<String>,
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
    /// atlibs-webrtc signaling URL
    WebRtc { signal: String },
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Account {
    pub handle: String,
    pub did: String,
    /// shell file exporting <PREFIX>ATPROTO_HANDLE and <PREFIX>ATPROTO_PASSWORD; read here only, never printed
    pub creds: String,
    /// the oauth-signin.ts --account prefix (ALICE, BOB); none: app password only
    #[serde(default)]
    pub oauth_account: Option<String>,
    /// staging throwaway account: tombstoned at teardown
    #[serde(default)]
    pub throwaway: bool,
}

pub fn expand(path: &str) -> PathBuf {
    match path.strip_prefix("~/") {
        Some(rest) => PathBuf::from(std::env::var("HOME").unwrap_or_default()).join(rest),
        None => PathBuf::from(path),
    }
}

impl Target {
    pub fn load() -> Result<Self> {
        let path = std::env::var("SWC_TARGET").context("SWC_TARGET names a target.json (tests/targets/prod.json for prod)")?;
        let path = expand(&path);
        let t: Target = serde_json::from_slice(&std::fs::read(&path).with_context(|| path.display().to_string())?)
            .with_context(|| format!("parsing {}", path.display()))?;
        Ok(t)
    }

    pub fn account(&self, name: &str) -> Result<&Account> {
        self.accounts.get(name).with_context(|| format!("target {} has no account {name}", self.name))
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
                return Ok(Secret(v.trim().trim_matches(|c| c == '"' || c == '\'').to_string()));
            }
        }
        bail!("{}: no *ATPROTO_PASSWORD", path.display())
    }
}

pub fn check_private(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let mode = std::fs::metadata(path).with_context(|| path.display().to_string())?.permissions().mode();
    if mode & 0o077 != 0 {
        bail!("{} is mode {:o}: chmod 600 it (owner-only)", path.display(), mode & 0o777);
    }
    Ok(())
}
