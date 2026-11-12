//! A bidder's own kcp (target bidders[].kcp): host-empty checks (0008 orphan check), pause / resume
//! (0001 sections 1, 5, 7) and fault injection (0007: delete an in-flight VM).

use std::time::Duration;

use anyhow::{Context, Result};
use serde_json::{Value, json};

use crate::kcp::Kcp;
use crate::target::Bidder;

/// The VM kinds a bidder host runs a guest as.
pub const VM_KINDS: [&str; 3] = ["ComputeVM", "FirecrackerVM", "QemuVM"];

pub struct BidderKcp {
    pub bidder: Bidder,
    pub kcp: Kcp,
}

impl BidderKcp {
    /// None when the target names no kcp for this bidder (prod without the lead's grant).
    pub async fn connect(b: &Bidder) -> Result<Option<Self>> {
        match &b.kcp {
            Some(ep) => Ok(Some(Self {
                // admin credentials of a prod bidder: owner-only copies
                bidder: {
                    crate::target::check_private(&crate::target::expand(&ep.kubeconfig))?;
                    b.clone()
                },
                kcp: Kcp::connect(ep)
                    .await
                    .with_context(|| format!("bidder {} kcp", b.name))?,
            })),
            None => Ok(None),
        }
    }

    /// Every VM object on the host (kinds the bidder does not serve count as none).
    pub async fn vms(&self) -> Result<Vec<Value>> {
        let mut out = vec![];
        for k in VM_KINDS {
            match self.kcp.list(k).await {
                Ok(v) => out.extend(v),
                Err(e)
                    if e.downcast_ref::<kube::Error>()
                        .is_some_and(|e| matches!(e, kube::Error::Api(a) if a.code == 404)) => {}
                Err(e) => return Err(e),
            }
        }
        Ok(out)
    }

    /// The VM objects whose JSON names `needle` (a receipt URI, a contract rkey, the requester DID prefix).
    pub async fn vms_of(&self, needle: &str) -> Result<Vec<Value>> {
        Ok(self
            .vms()
            .await?
            .into_iter()
            .filter(|v| v.to_string().contains(needle))
            .collect())
    }

    /// The ComputeVM of a receipt this test's own contract got (`spec.contract.receipt.uri`): fault injection on prod
    /// touches only VMs the test created.
    pub async fn own_vm(&self, receipt: &str) -> Result<String> {
        let vms: Vec<_> = self
            .kcp
            .list("ComputeVM")
            .await?
            .into_iter()
            .filter(|v| v["spec"]["contract"]["receipt"]["uri"] == receipt)
            .collect();
        anyhow::ensure!(
            vms.len() == 1,
            "{} ComputeVMs of {receipt} on {}",
            vms.len(),
            self.bidder.name
        );
        Ok(vms[0]["metadata"]["name"]
            .as_str()
            .unwrap_or_default()
            .to_string())
    }

    /// Pause (`spec.suspend`) and wait for the root's phase: Suspended, or Ready on resume.
    pub async fn suspend(&self, on: bool) -> Result<()> {
        self.kcp
            .merge("MarketBidder", "app", &json!({ "spec": { "suspend": on } }))
            .await?;
        let phase = if on { "Suspended" } else { "Ready" };
        self.kcp
            .wait_phase("MarketBidder", "app", phase, Duration::from_secs(300))
            .await
            .map(|_| ())
    }

    pub async fn did(&self) -> Result<String> {
        let o = self
            .kcp
            .get("MarketBidder", "app")
            .await?
            .context("no MarketBidder app")?;
        Ok(o["status"]["did"].as_str().unwrap_or_default().into())
    }
}
