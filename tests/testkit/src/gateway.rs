//! The gateway's cluster XRPC (com.publicdomainrelay.temp.cluster.*), called with the account's service auth.

use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use atlibs_host::host::Session;
use serde_json::{Value, json};

use crate::{Ctx, eventually};

const N: &str = "com.publicdomainrelay.temp.cluster";

pub struct Gateway {
    ctx: Arc<Ctx>,
    session: Arc<Session>,
}

impl Gateway {
    pub async fn new(ctx: &Arc<Ctx>, account: &str) -> Result<Self> {
        let session = ctx.at.session(ctx.account(account)?).await?;
        Ok(Self {
            ctx: ctx.clone(),
            session,
        })
    }

    pub async fn procedure(&self, method: &str, input: Value) -> Result<Value> {
        let g = &self.ctx.target.gateway;
        self.ctx
            .at
            .service_call(
                &self.session,
                &g.url,
                &g.aud,
                &format!("{N}.{method}"),
                Some(&input),
                &[],
            )
            .await
    }

    pub async fn query(&self, method: &str, params: &[(&str, &str)]) -> Result<Value> {
        let g = &self.ctx.target.gateway;
        self.ctx
            .at
            .service_call(
                &self.session,
                &g.url,
                &g.aud,
                &format!("{N}.{method}"),
                None,
                params,
            )
            .await
    }

    pub async fn cluster(&self, name: &str) -> Result<Option<Value>> {
        let l = self.query("listClusters", &[]).await?;
        Ok(l["clusters"]
            .as_array()
            .into_iter()
            .flatten()
            .find(|c| c["name"] == name)
            .cloned())
    }

    /// createCluster, then wait until listClusters says Ready. A cleanup deletes it.
    pub async fn create_cluster(self: &Arc<Self>, name: &str, timeout: Duration) -> Result<()> {
        let (g, n) = (self.clone(), name.to_string());
        self.ctx
            .defer(&format!("deleteCluster {name}"), async move {
                if g.cluster(&n).await?.is_some() {
                    g.procedure("deleteCluster", json!({ "name": n })).await?;
                }
                Ok(())
            });
        self.procedure("createCluster", json!({ "name": name }))
            .await?;
        eventually(
            &format!("cluster {name} Ready"),
            timeout,
            Duration::from_secs(5),
            || async {
                Ok(self
                    .cluster(name)
                    .await?
                    .filter(|c| c["phase"] == "Ready")
                    .map(|_| ()))
            },
        )
        .await
    }

    /// getKubeconfig (ttlSeconds 0: no expiry), written owner-only into the run's scratch dir; its path.
    pub async fn kubeconfig(&self, name: &str, ttl_seconds: u64, file: &str) -> Result<String> {
        let v = self
            .procedure(
                "getKubeconfig",
                json!({ "name": name, "ttlSeconds": ttl_seconds }),
            )
            .await?;
        let path = self.ctx.dir.join(file);
        {
            use std::os::unix::fs::OpenOptionsExt;
            std::io::Write::write_all(
                &mut std::fs::OpenOptions::new()
                    .create(true)
                    .truncate(true)
                    .write(true)
                    .mode(0o600)
                    .open(&path)?,
                v["kubeconfig"]
                    .as_str()
                    .context("getKubeconfig answered no kubeconfig")?
                    .as_bytes(),
            )?;
        }
        Ok(path.display().to_string())
    }
}
