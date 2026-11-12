use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::watch;

use crate::kube::{KubeClient, KubeError, NodeEvent};
use crate::nodes::{Node, NodeSet};
use crate::roster::{ANNOTATION_ROSTER, Roster};
use crate::routes::{ANNOTATION_DID, ANNOTATION_ENDPOINT, EndpointBytes, RouteTable};
use crate::stats::Stats;

pub const KUBE_RETRY_MIN: Duration = Duration::from_secs(1);
pub const KUBE_RETRY_MAX: Duration = Duration::from_secs(30);
pub const ANNOTATE_RETRY: Duration = Duration::from_secs(5);
pub const NODE_READER_HINT: &str = "the node authorizer lets a node read only its own Node (AuthorizeNodeWithSelectors); apply rust/socialweb-meshd/deploy/node-reader-rbac.yaml to grant group system:nodes get/list/watch on nodes";

#[derive(Clone)]
pub struct SelfIdentity {
    pub node_name: String,
    pub endpoint: EndpointBytes,
    pub endpoint_hex: String,
    pub did: String,
}

#[derive(Debug)]
pub enum SourceError {
    Config(String),
    Forbidden(String),
}

impl std::fmt::Display for SourceError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SourceError::Config(msg) => write!(f, "{msg}"),
            SourceError::Forbidden(msg) => write!(f, "{msg}"),
        }
    }
}

fn publish(
    self_name: &str,
    me: &SelfIdentity,
    nodes: &[Node],
    tx: &watch::Sender<Arc<RouteTable>>,
    stats: &Stats,
) -> Arc<RouteTable> {
    let table = Arc::new(RouteTable::compute(self_name, &me.endpoint, nodes));
    Stats::bump(&stats.node_list_updates);
    let changed = **tx.borrow() != *table;
    if changed {
        tracing::info!(
            event = "routes",
            own_pod_cidrs = ?table.own_cidrs.iter().map(ToString::to_string).collect::<Vec<_>>(),
            routes = ?table.routes.iter().map(|r| format!("{}->{}", r.cidr, r.node)).collect::<Vec<_>>(),
            rejected = ?table.rejected.iter().map(|r| format!("{}: {}", r.node, r.reason)).collect::<Vec<_>>()
        );
        tx.send_replace(Arc::clone(&table));
    }
    table
}

pub async fn run_routes(
    me: SelfIdentity,
    mut kube: Option<watch::Receiver<Arc<Vec<Node>>>>,
    mut roster: watch::Receiver<Arc<Roster>>,
    location: Option<watch::Sender<Option<String>>>,
    tx: watch::Sender<Arc<RouteTable>>,
    stats: Arc<Stats>,
) {
    loop {
        let current = Arc::clone(&roster.borrow_and_update());
        match kube.as_mut() {
            Some(kube) => {
                let nodes = Arc::clone(&kube.borrow_and_update());
                let table = publish(
                    &me.node_name,
                    &me,
                    &current.beside(&nodes, &me.endpoint_hex),
                    &tx,
                    &stats,
                );
                if !table.self_view.seen {
                    tracing::info!(event = "own_node_waiting", node = %me.node_name);
                }
                if let Some(location) = &location {
                    let wanted = nodes
                        .iter()
                        .find(|node| node.metadata.name == me.node_name)
                        .and_then(|node| node.annotation(ANNOTATION_ROSTER))
                        .map(str::to_string);
                    location.send_if_modified(|old| {
                        let changed = *old != wanted;
                        *old = wanted.clone();
                        changed
                    });
                }
            }
            None => match current.self_name(&me.endpoint_hex) {
                Some(name) => {
                    publish(&name, &me, &current.nodes(), &tx, &stats);
                }
                None => {
                    publish(&me.node_name, &me, &[], &tx, &stats);
                }
            },
        }
        let kube_changed = async {
            match kube.as_mut() {
                Some(kube) => kube.changed().await,
                None => std::future::pending().await,
            }
        };
        tokio::select! {
            changed = kube_changed => if changed.is_err() { return },
            changed = roster.changed() => if changed.is_err() { return },
        }
    }
}

fn annotations(me: &SelfIdentity) -> BTreeMap<String, String> {
    BTreeMap::from([
        (ANNOTATION_ENDPOINT.to_string(), me.endpoint_hex.clone()),
        (ANNOTATION_DID.to_string(), me.did.clone()),
    ])
}

pub fn kube_client(path: &Path) -> Result<Option<KubeClient>, SourceError> {
    if !path.exists() {
        return Ok(None);
    }
    match KubeClient::from_kubeconfig(path) {
        Ok(client) => Ok(Some(client)),
        Err(KubeError::Config(msg)) if msg.starts_with("read ") => Ok(None),
        Err(err) => Err(SourceError::Config(err.to_string())),
    }
}

pub async fn run_annotator(
    kubeconfig: PathBuf,
    me: SelfIdentity,
    mut routes: watch::Receiver<Arc<RouteTable>>,
) -> Result<(), SourceError> {
    loop {
        let needs = {
            let table = routes.borrow_and_update();
            table.self_view.seen && !table.self_annotated_with(&me.endpoint_hex, &me.did)
        };
        if needs {
            if let Some(client) = kube_client(&kubeconfig)? {
                match client
                    .patch_node_annotations(&me.node_name, &annotations(&me))
                    .await
                {
                    Ok(()) => {
                        tracing::info!(event = "node_annotated", node = %me.node_name, endpoint = %me.endpoint_hex, did = %me.did);
                    }
                    Err(KubeError::Status { code: 403, body }) => {
                        return Err(SourceError::Forbidden(format!(
                            "patching Node {} was forbidden ({body}); --node-name must be the node the kubeconfig's identity is",
                            me.node_name
                        )));
                    }
                    Err(err) => {
                        tracing::warn!(event = "annotate_failed", node = %me.node_name, error = %err);
                    }
                }
            }
            tokio::select! {
                changed = routes.changed() => if changed.is_err() { return Ok(()) },
                () = tokio::time::sleep(ANNOTATE_RETRY) => {}
            }
        } else if routes.changed().await.is_err() {
            return Ok(());
        }
    }
}

pub async fn run_kube(
    kubeconfig: PathBuf,
    tx: watch::Sender<Arc<Vec<Node>>>,
) -> Result<(), SourceError> {
    let mut retry = KUBE_RETRY_MIN;
    loop {
        let client = match kube_client(&kubeconfig)? {
            Some(client) => client,
            None => {
                tracing::info!(event = "kubeconfig_waiting", path = %kubeconfig.display());
                tokio::time::sleep(retry).await;
                retry = (retry * 2).min(KUBE_RETRY_MAX);
                continue;
            }
        };
        let list = match client.list_nodes().await {
            Ok(list) => list,
            Err(KubeError::Status { code: 403, body }) => {
                tracing::error!(
                    event = "list_nodes_forbidden",
                    error = %body,
                    hint = NODE_READER_HINT,
                    retry_in_ms = retry.as_millis() as u64
                );
                tokio::time::sleep(retry).await;
                retry = (retry * 2).min(KUBE_RETRY_MAX);
                continue;
            }
            Err(err) => {
                tracing::warn!(event = "list_nodes_failed", error = %err, retry_in_ms = retry.as_millis() as u64);
                tokio::time::sleep(retry).await;
                retry = (retry * 2).min(KUBE_RETRY_MAX);
                continue;
            }
        };
        retry = KUBE_RETRY_MIN;
        let version = list.metadata.resource_version.clone().unwrap_or_default();
        let mut nodes = NodeSet::from_list(list);
        tx.send_replace(Arc::new(nodes.to_vec()));

        let watch_started = tokio::time::Instant::now();
        let watched = client
            .watch_nodes(&version, |event| {
                let dirty = match event {
                    NodeEvent::Upsert(node) => {
                        nodes.upsert(*node);
                        true
                    }
                    NodeEvent::Delete(name, _) => {
                        nodes.remove(&name);
                        true
                    }
                    NodeEvent::Bookmark(_) | NodeEvent::Gone => false,
                };
                if dirty {
                    tx.send_replace(Arc::new(nodes.to_vec()));
                }
            })
            .await;
        match watched {
            Ok(_) => tracing::debug!(event = "watch_ended_relisting"),
            Err(err) if err.is_gone() => tracing::info!(event = "watch_expired_relisting"),
            Err(err) => tracing::warn!(event = "watch_failed", error = %err),
        }
        if watch_started.elapsed() < KUBE_RETRY_MIN {
            tokio::time::sleep(KUBE_RETRY_MIN).await;
        }
    }
}
