//! A tenant k3s cluster (gateway or requester-gateway kubeconfig): deployments, pods, nodes.

use std::time::Duration;

use anyhow::{Context, Result};
use k8s_openapi::api::apps::v1::Deployment;
use k8s_openapi::api::core::v1::{Node, Pod};
use kube::api::{Api, DeleteParams, ListParams, LogParams, Patch, PatchParams};
use serde_json::json;

use crate::eventually;
use crate::kcp::Kcp;

pub struct Tenant {
    pub kcp: Kcp,
}

impl Tenant {
    pub async fn connect(kubeconfig: &str) -> Result<Self> {
        Ok(Self { kcp: Kcp::kubeconfig(kubeconfig).await? })
    }

    fn deployments(&self) -> Api<Deployment> {
        Api::namespaced(self.kcp.client.clone(), "default")
    }

    fn pods(&self) -> Api<Pod> {
        Api::namespaced(self.kcp.client.clone(), "default")
    }

    pub async fn apply_deployment(&self, yaml: &str) -> Result<String> {
        let d: Deployment = serde_yaml::from_str(yaml)?;
        let name = d.metadata.name.clone().context("deployment without a name")?;
        self.deployments().patch(&name, &PatchParams::apply("swc-system-tests").force(), &Patch::Apply(&d)).await?;
        Ok(name)
    }

    pub async fn scale(&self, name: &str, n: i32) -> Result<()> {
        self.deployments().patch(name, &PatchParams::default(), &Patch::Merge(json!({ "spec": { "replicas": n } }))).await?;
        Ok(())
    }

    pub async fn delete_deployment(&self, name: &str) -> Result<()> {
        match self.deployments().delete(name, &DeleteParams::default()).await {
            Ok(_) => Ok(()),
            Err(kube::Error::Api(e)) if e.code == 404 => Ok(()),
            Err(e) => Err(e.into()),
        }
    }

    /// (pod, node) of every Ready pod matching `selector`.
    pub async fn ready_pods(&self, selector: &str) -> Result<Vec<(String, String)>> {
        let pods = self.pods().list(&ListParams::default().labels(selector)).await?;
        Ok(pods
            .items
            .into_iter()
            .filter(|p| p.status.as_ref().and_then(|s| s.conditions.as_ref()).is_some_and(|c| c.iter().any(|c| c.type_ == "Ready" && c.status == "True")))
            .map(|p| (p.metadata.name.unwrap_or_default(), p.spec.and_then(|s| s.node_name).unwrap_or_default()))
            .collect())
    }

    pub async fn wait_ready(&self, selector: &str, n: usize, timeout: Duration) -> Result<Vec<(String, String)>> {
        eventually(&format!("{n} Ready pods {selector}"), timeout, Duration::from_secs(5), || async {
            let p = self.ready_pods(selector).await?;
            Ok((p.len() == n).then_some(p))
        })
        .await
    }

    pub async fn nodes(&self) -> Result<Vec<String>> {
        Ok(Api::<Node>::all(self.kcp.client.clone()).list(&ListParams::default()).await?.items.into_iter().filter_map(|n| n.metadata.name).collect())
    }

    pub async fn wait_nodes(&self, n: usize, timeout: Duration) -> Result<Vec<String>> {
        eventually(&format!("{n} nodes"), timeout, Duration::from_secs(5), || async {
            let ns = self.nodes().await?;
            Ok((ns.len() == n).then_some(ns))
        })
        .await
    }

    pub async fn logs(&self, pod: &str) -> Result<String> {
        Ok(self.pods().logs(pod, &LogParams::default()).await?)
    }
}
