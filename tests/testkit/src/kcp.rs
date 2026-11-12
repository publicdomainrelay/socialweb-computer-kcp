//! One kcp (or a tenant k3s) as a kube-rs client: the kubeconfig's credentials over a transport (target::Via).
//! Every kind of ours is cluster-scoped; read and written as dynamic objects.

use std::time::{Duration, Instant};

use anyhow::{Context, Result, anyhow, bail};
use kube::api::{Api, ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch, PatchParams};
use kube::config::{KubeConfigOptions, Kubeconfig};
use serde_json::Value;

use crate::target::{KcpEndpoint, Via, expand};

pub const GROUP: &str = "socialweb.computer";
const MANAGER: &str = "swc-system-tests";

pub struct Kcp {
    pub client: kube::Client,
    /// the dumbpipe forward of an iroh transport; killed on drop
    _pipe: Option<tokio::process::Child>,
}

fn plural(kind: &str) -> String {
    let k = kind.to_lowercase();
    match k.strip_suffix('y') {
        Some(stem) => format!("{stem}ies"),
        None => format!("{k}s"),
    }
}

/// socialweb.computer/v1alpha1 `kind`.
pub fn resource(kind: &str) -> ApiResource {
    ApiResource::from_gvk_with_plural(&GroupVersionKind::gvk(GROUP, "v1alpha1", kind), &plural(kind))
}

async fn free_port() -> Result<u16> {
    Ok(tokio::net::TcpListener::bind("127.0.0.1:0").await?.local_addr()?.port())
}

impl Kcp {
    pub async fn kubeconfig(path: &str) -> Result<Self> {
        Self::connect(&KcpEndpoint { kubeconfig: path.into(), workspace: None, via: Via::Direct }).await
    }

    pub async fn connect(ep: &KcpEndpoint) -> Result<Self> {
        let path = expand(&ep.kubeconfig);
        let kc = Kubeconfig::read_from(&path).with_context(|| path.display().to_string())?;
        let mut cfg = kube::Config::from_custom_kubeconfig(kc, &KubeConfigOptions::default()).await?;
        let url = cfg.cluster_url.clone();
        let host = url.host().context("kubeconfig server has no host")?.to_string();
        let port = url.port_u16().unwrap_or(443);
        let mut path_q = url.path().trim_end_matches('/').to_string();
        if let Some(ws) = &ep.workspace {
            path_q = format!("/clusters/{ws}");
        }
        let mut pipe = None;
        let authority = match &ep.via {
            Via::Direct => format!("{host}:{port}"),
            Via::Overlay { name } => format!("{name}:{port}"),
            Via::Iroh { ticket } => {
                let local = free_port().await?;
                let child = tokio::process::Command::new("dumbpipe")
                    .args(["connect-tcp", "--addr", &format!("127.0.0.1:{local}"), ticket])
                    .stdout(std::process::Stdio::null())
                    .stderr(std::process::Stdio::null())
                    .kill_on_drop(true)
                    .spawn()
                    .context("dumbpipe on PATH")?;
                pipe = Some(child);
                let deadline = Instant::now() + Duration::from_secs(30);
                while tokio::net::TcpStream::connect(("127.0.0.1", local)).await.is_err() {
                    if Instant::now() > deadline {
                        bail!("dumbpipe forward to the kcp did not open in 30 s");
                    }
                    tokio::time::sleep(Duration::from_millis(200)).await;
                }
                format!("127.0.0.1:{local}")
            }
            // ponytail: no WebRTC connector yet; add atlibs-webrtc as a hyper connector when a kcp is only reachable so.
            Via::WebRtc { signal } => bail!("kcp over WebRTC ({signal}): the atlibs-webrtc connector is not wired yet"),
        };
        cfg.cluster_url = format!("https://{authority}{path_q}").parse()?;
        if !matches!(ep.via, Via::Direct) {
            cfg.tls_server_name = Some(host);
        }
        Ok(Self { client: kube::Client::try_from(cfg)?, _pipe: pipe })
    }

    /// GET a raw path under the server URL's /clusters/... prefix (`/readyz`).
    pub async fn raw(&self, path: &str) -> Result<String> {
        Ok(self.client.request_text(http::Request::get(path).body(vec![])?).await?)
    }

    pub fn api(&self, kind: &str) -> Api<DynamicObject> {
        Api::all_with(self.client.clone(), &resource(kind))
    }

    pub async fn get(&self, kind: &str, name: &str) -> Result<Option<Value>> {
        Ok(self.api(kind).get_opt(name).await?.map(|o| serde_json::to_value(o).unwrap()))
    }

    pub async fn list(&self, kind: &str) -> Result<Vec<Value>> {
        Ok(self.api(kind).list(&ListParams::default()).await?.items.into_iter().map(|o| serde_json::to_value(o).unwrap()).collect())
    }

    /// Server-side apply of one object of ours (apiVersion socialweb.computer/v1alpha1).
    pub async fn apply(&self, obj: &Value) -> Result<Value> {
        let kind = obj["kind"].as_str().context("object without kind")?;
        let name = obj["metadata"]["name"].as_str().context("object without metadata.name")?;
        let out = self.api(kind).patch(name, &PatchParams::apply(MANAGER).force(), &Patch::Apply(obj)).await.with_context(|| format!("apply {kind}/{name}"))?;
        Ok(serde_json::to_value(out)?)
    }

    /// Apply every document of a YAML stream (an examples/ file after its substitutions).
    pub async fn apply_yaml(&self, yaml: &str) -> Result<Vec<Value>> {
        let mut out = vec![];
        for doc in serde_yaml::Deserializer::from_str(yaml) {
            let v: Value = serde::Deserialize::deserialize(doc)?;
            if !v.is_null() {
                out.push(self.apply(&v).await?);
            }
        }
        Ok(out)
    }

    pub async fn merge(&self, kind: &str, name: &str, patch: &Value) -> Result<Value> {
        Ok(serde_json::to_value(self.api(kind).patch(name, &PatchParams::default(), &Patch::Merge(patch)).await?)?)
    }

    /// Delete; Ok(false) when it was already gone.
    pub async fn delete(&self, kind: &str, name: &str) -> Result<bool> {
        match self.api(kind).delete(name, &DeleteParams::default()).await {
            Ok(_) => Ok(true),
            Err(kube::Error::Api(e)) if e.code == 404 => Ok(false),
            Err(e) => Err(e.into()),
        }
    }

    /// Poll until `pred` holds for the object (None: it does not exist). ponytail: 2 s polling, a watch if it matters.
    pub async fn wait(&self, kind: &str, name: &str, timeout: Duration, what: &str, pred: impl Fn(Option<&Value>) -> bool) -> Result<Option<Value>> {
        let deadline = Instant::now() + timeout;
        loop {
            let o = self.get(kind, name).await?;
            if pred(o.as_ref()) {
                return Ok(o);
            }
            if Instant::now() > deadline {
                let phase = o.as_ref().map(|o| format!("{} {}", o["status"]["phase"], o["status"]["message"])).unwrap_or_else(|| "absent".into());
                return Err(anyhow!("{kind}/{name}: not {what} after {timeout:?} (now {phase})"));
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
    }

    pub async fn wait_phase(&self, kind: &str, name: &str, phase: &str, timeout: Duration) -> Result<Value> {
        let o = self.wait(kind, name, timeout, phase, |o| o.is_some_and(|o| o["status"]["phase"] == phase)).await?;
        Ok(o.unwrap())
    }

    pub async fn wait_gone(&self, kind: &str, name: &str, timeout: Duration) -> Result<()> {
        self.wait(kind, name, timeout, "gone", |o| o.is_none()).await.map(|_| ())
    }
}
