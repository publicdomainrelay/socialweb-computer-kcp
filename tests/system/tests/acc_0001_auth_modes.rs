//! docs/acceptance/0001-auth-modes.md sections 3-6 against the gateway (k8s.socialweb.computer): a tenant cluster,
//! a no-expiry kubeconfig and its revoke, then a deployment with required pod anti-affinity scaled one step at a time
//! 1 -> 2 -> 3 -> 2 -> 1 -> 0, each replica on its own node (one market VM). Discovery: no bidder is paused or pinned.
//! Sections 1, 2, 7, 8 (bidder pause, typed settings) need the bidders' kcps (target bidders[].kcp): see
//! acc_0001_bidder_pause.rs. Section 9 is 0003.

use std::collections::BTreeSet;
use std::sync::Arc;
use std::time::Duration;

use k8s_openapi::api::core::v1::Namespace;
use kube::api::{Api, ListParams};
use testkit::gateway::Gateway;
use testkit::*;

const MIN: Duration = Duration::from_secs(60);
const SCALE_CHECK: &str = r#"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: scale-check
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: scale-check
  template:
    metadata:
      labels:
        app: scale-check
    spec:
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector:
                matchLabels:
                  app: scale-check
              topologyKey: kubernetes.io/hostname
      containers:
        - name: run
          image: docker.io/library/busybox:1.36.1
          command: ["sh", "-c", "sleep 3600"]
"#;

async fn namespaces(t: &Tenant) -> Result<usize> {
    Ok(Api::<Namespace>::all(t.kcp.client.clone()).list(&ListParams::default()).await?.items.len())
}

#[tokio::test(flavor = "multi_thread")]
async fn s3_s6_tenant_cluster_stepwise() {
    run("0001", "s3-s6", &["alice"], |ctx| async move {
        let gw = Arc::new(Gateway::new(&ctx, "alice").await?);
        let cl = format!("st-{}", std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?.as_secs());

        // 3: create, kubeconfig, readyz through the /clusters/... server path
        let t0 = ctx.t();
        gw.create_cluster(&cl, 5 * MIN).await?;
        ctx.reading("cluster_ready_s", ctx.t() - t0);
        let kc = gw.kubeconfig(&cl, 28800, "kubeconfig").await?;
        let t = Tenant::connect(&kc).await?;
        ensure!(t.kcp.raw("/readyz").await?.trim() == "ok", "/readyz is not ok");
        ctx.reading("namespaces", namespaces(&t).await?);

        // 3a over xrpc: a no-expiry kubeconfig works, its revoke refuses it, the 8 h one still works
        let ne = gw.kubeconfig(&cl, 0, "noexpiry.kubeconfig").await?;
        namespaces(&Tenant::connect(&ne).await?).await?;
        let ks = gw.query("listKubeconfigs", &[("name", &cl)]).await?;
        let serial = ks["kubeconfigs"]
            .as_array()
            .into_iter()
            .flatten()
            .filter(|k| k["noExpiry"] == true && k["revoked"] != true)
            .max_by_key(|k| k["createdAt"].as_str().unwrap_or_default().to_string())
            .and_then(|k| k["serial"].as_str())
            .context("listKubeconfigs has no live noExpiry kubeconfig")?
            .to_string();
        ensure!(gw.procedure("revokeKubeconfig", json!({ "name": cl, "serial": serial })).await?["revoked"] == true);
        eventually("the revoked kubeconfig refused", Duration::from_secs(30), Duration::from_secs(2), || async {
            Ok(namespaces(&Tenant::connect(&ne).await?).await.is_err().then_some(()))
        })
        .await?;
        namespaces(&t).await?;

        // 4-6: one replica per node, one step at a time
        let name = t.apply_deployment(SCALE_CHECK).await?;
        {
            let kc = kc.clone();
            ctx.defer("delete deployment scale-check", async move { Tenant::connect(&kc).await?.delete_deployment("scale-check").await });
        }
        let mut steps = vec![];
        for n in [1usize, 2, 3, 2, 1, 0] {
            let ts = ctx.t();
            t.scale(&name, n as i32).await?;
            // scale-down: the provider holds an idle node for two minutes before it releases it
            let nodes = t.wait_nodes(n, 15 * MIN).await?;
            let pods = t.wait_ready("app=scale-check", n, 15 * MIN).await?;
            let on: BTreeSet<_> = pods.iter().map(|(_, node)| node.clone()).collect();
            ensure!(on.len() == n, "{n} replicas on {} nodes: {pods:?}", on.len());
            ensure!(on.iter().all(|x| nodes.contains(x)), "pods on nodes not in the cluster: {pods:?} {nodes:?}");
            steps.push(json!({ "replicas": n, "seconds": ctx.t() - ts, "nodes": nodes }));
        }
        ctx.reading("steps", &steps);
        t.delete_deployment(&name).await?;
        gw.procedure("deleteCluster", json!({ "name": cl })).await?;
        let td = ctx.t();
        eventually("the cluster gone", 5 * MIN, Duration::from_secs(5), || async { Ok(gw.cluster(&cl).await?.is_none().then_some(())) }).await?;
        ctx.reading("cluster_deleted_s", ctx.t() - td);
        ensure!(namespaces(&t).await.is_err(), "the 8 h kubeconfig still works after deleteCluster");
        Ok(())
    })
    .await;
}
