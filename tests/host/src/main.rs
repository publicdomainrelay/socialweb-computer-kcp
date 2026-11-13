//! swc-test-host <composition dir> [--account <name>] [--dry-run]: runs a test composition (its composition.json;
//! modules from tests/modules, built with `cargo wasm`) against SWC_TARGET as the account, with atlibs-kube (the
//! target's bidder and gateway kcps, the requesters it starts), atlibs-ssh, atlibs-iroh, identlink and the socialweb
//! handler. The root instance `test` runs entry `run`. Cleanup always runs after (invocations released, requesters
//! stopped, kube objects deleted or restored); the readings land in tests/readings as the Rust tests' do.

mod swc;

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use anyhow::{Context, Result, anyhow};
use atlibs_host::compose::{self, Composition, ModuleLoader};
use atlibs_host::host::Fetch;
use atlibs_host::run::{Module, Options};
use atlibs_host::store::Store;
use atlibs_host::wasm::Wasm;
use atlibs_kube::{Cluster, Kube, Via};
use serde_json::{Value, json};
use testkit::requester::Binary;
use testkit::target::{KcpEndpoint, Target};

fn cluster_of(ep: &KcpEndpoint) -> Cluster {
    let via = match &ep.via {
        testkit::target::Via::Direct => Via::Direct,
        testkit::target::Via::Overlay { name } => Via::Overlay { name: name.clone() },
        testkit::target::Via::Ssh { host } => Via::Ssh { host: host.clone() },
        testkit::target::Via::Iroh { ticket } => Via::Iroh {
            ticket: ticket.clone(),
        },
        testkit::target::Via::WebRtc { proxy } => Via::Proxy {
            command: proxy.clone(),
        },
    };
    Cluster {
        kubeconfig: testkit::target::expand(&ep.kubeconfig)
            .display()
            .to_string(),
        workspace: ep.workspace.clone(),
        via,
    }
}

/// A module of tests/modules by its key: module.json + the release wasm.
fn loader(modules: PathBuf) -> ModuleLoader {
    Arc::new(move |key: String, _pin: Option<String>| {
        let modules = modules.clone();
        Box::pin(async move {
            let dir = modules.join(&key);
            let manifest: Value = serde_json::from_slice(
                &std::fs::read(dir.join("module.json")).map_err(|e| format!("{key}: {e}"))?,
            )
            .map_err(|e| e.to_string())?;
            let wasm = modules
                .join("target/wasm32-unknown-unknown/release")
                .join(format!("{}.wasm", key.replace('-', "_")));
            let bytes = std::fs::read(&wasm)
                .map_err(|e| format!("{}: {e} (cargo wasm in tests/modules)", wasm.display()))?;
            let mut m = Module::from_manifest(bytes, &manifest)?;
            m.id = Some(dir.display().to_string());
            m.key = Some(key);
            Ok(m)
        })
    })
}

async fn main_() -> Result<bool> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let comp_dir = PathBuf::from(
        args.first()
            .context("usage: swc-test-host <composition dir> [--account <name>] [--dry-run]")?,
    );
    let flag = |f: &str| {
        args.iter()
            .position(|a| a == f)
            .and_then(|i| args.get(i + 1))
            .cloned()
    };
    let account = flag("--account").unwrap_or_else(|| "alice".into());
    let dry = args.iter().any(|a| a == "--dry-run");
    let tests = testkit::repo().join("tests");
    let manifest: Value = serde_json::from_slice(
        &std::fs::read(comp_dir.join("composition.json")).context("composition.json")?,
    )?;
    let runbook = comp_dir.file_name().unwrap().to_string_lossy().to_string();

    let target = Target::load()?;
    let acc = target.account(&account)?;
    acc.password()?;
    let bin = Binary::resolve(&target.requester).await?;
    let comp = compose::load(&manifest, &loader(tests.join("modules")))
        .await
        .map_err(|e| anyhow!(e))?;
    let dir =
        testkit::home_tmp("runs").join(format!("{runbook}-composition-{}", atlibs_host::now_ms()));
    std::fs::create_dir_all(&dir)?;
    std::fs::set_permissions(&dir, std::os::unix::fs::PermissionsExt::from_mode(0o700))?;

    // transports: kube (bidder and gateway kcps; requesters add theirs), ssh, iroh, the socialweb handler
    let receipts: Arc<Mutex<Vec<String>>> = Default::default();
    let mut clusters = HashMap::new();
    for b in &target.bidders {
        if let Some(ep) = &b.kcp {
            clusters.insert(format!("bidder:{}", b.name), cluster_of(ep));
        }
    }
    if let Some(ep) = &target.gateway.kcp {
        clusters.insert("gateway".into(), cluster_of(ep));
    }
    let rs = receipts.clone();
    // fault injection reaches only this run's own VMs; a bidder root's spec is patched only to be restored
    let guard: atlibs_kube::Guard =
        Arc::new(move |_cluster: &str, verb: &str, obj: &Value| {
            match (verb, obj["kind"].as_str()) {
                ("delete", Some("ComputeVM" | "FirecrackerVM" | "QemuVM")) => {
                    obj["spec"]["contract"]["receipt"]["uri"]
                        .as_str()
                        .is_some_and(|r| rs.lock().unwrap().iter().any(|x| x == r))
                }
                ("patch", Some("MarketBidder")) => true,
                _ => false,
            }
        });
    let kube = Kube::new(clusters, Some(guard));
    let ssh = atlibs_ssh::Ssh::new(HashMap::new(), HashMap::new());
    let swc = swc::Swc::new(
        Target::load()?,
        &account,
        bin.clone(),
        dir.clone(),
        kube.clone(),
        receipts,
        ssh.clone(),
    );
    let transports = vec![
        kube.transport(),
        ssh.transport(),
        atlibs_iroh::transport(testkit::home_tmp("iroh_ed25519")),
        swc.transport(),
    ];
    let provides = Arc::new(atlibs_host::transport::provides(&transports));
    compose::check(&comp, &provides).map_err(|e| anyhow!(e))?;
    if dry {
        println!(
            "dry run {runbook}: composition ok ({} instances), requester {}",
            comp.modules.len(),
            bin.version
        );
        return Ok(true);
    }
    // one instance's entry, signed out (public reads only): `--instance assert --entry tombstoned --input '{...}'`
    if let Some(instance) = flag("--instance") {
        let entry = flag("--entry").context("--instance needs --entry")?;
        let input: serde_json::Map<String, Value> =
            serde_json::from_str(&flag("--input").unwrap_or_else(|| "{}".into()))?;
        let store = Store::open(&dir.join("store"), "swc-test-host").map_err(|e| anyhow!(e))?;
        let fetch: Arc<dyn Fetch> = Arc::new(reqwest::Client::new());
        let mut calls = HashMap::new();
        for (name, module) in &comp.modules {
            calls.insert(
                name.clone(),
                atlibs_host::registry::module_call(
                    &store,
                    fetch.clone(),
                    None,
                    &transports,
                    module,
                    &format!("{runbook}#{name}"),
                )
                .await
                .map_err(|e| anyhow!(e))?,
            );
        }
        let call_for: compose::CallFor = Arc::new(move |n: &str| calls.get(n).cloned());
        let comp = Arc::new(Composition {
            manifest: comp.manifest.clone(),
            modules: comp.modules.clone(),
        });
        let options = Options {
            provides: Some(provides),
            ..Default::default()
        };
        let ran = compose::run_compose(
            comp,
            Wasm::new(5000),
            None,
            &instance,
            &entry,
            input,
            call_for,
            HashMap::new(),
            None,
            options,
        )
        .await
        .map_err(|e| anyhow!(e))?;
        println!("{}", serde_json::to_string_pretty(&ran)?);
        return Ok(ran.get("error").is_none_or(Value::is_null));
    }
    let _lock = testkit::lock_account(&acc.handle).await?;

    let at = testkit::Atproto::new(&target);
    let session = at.session(acc).await?;
    let store = Store::open(&dir.join("store"), "swc-test-host").map_err(|e| anyhow!(e))?;
    let fetch: Arc<dyn Fetch> = Arc::new(at.http.clone());
    let mut calls = HashMap::new();
    for (name, module) in &comp.modules {
        calls.insert(
            name.clone(),
            atlibs_host::registry::module_call(
                &store,
                fetch.clone(),
                Some(session.clone()),
                &transports,
                module,
                &format!("{runbook}#{name}"),
            )
            .await
            .map_err(|e| anyhow!(e))?,
        );
    }
    let call_for: compose::CallFor = Arc::new(move |n: &str| calls.get(n).cloned());
    let options = Options {
        budget: Some(
            json!({ "calls": "unlimited", "events": "unlimited", "wallMs": "unlimited", "subscriptions": 8, "inFlight": 12 }),
        ),
        provides: Some(provides),
        ..Default::default()
    };
    let started = atlibs_host::iso_now();
    let name = format!("st-{runbook}-{}", atlibs_host::now_ms() / 1000);
    let input: serde_json::Map<String, Value> =
        json!({ "did": acc.did, "handle": acc.handle, "name": name })
            .as_object()
            .unwrap()
            .clone();
    let comp = Arc::new(Composition {
        manifest: comp.manifest.clone(),
        modules: comp.modules.clone(),
    });
    let ran = compose::run_compose(
        comp,
        Wasm::new(5000),
        Some(store.ledger("managed_records")),
        "test",
        "run",
        input,
        call_for,
        HashMap::new(),
        None,
        options,
    )
    .await;

    let mut cleanup = swc.cleanup().await;
    cleanup.extend(kube.cleanup().await);
    let result = match &ran {
        Ok(v) => v.get("result").cloned().unwrap_or_else(|| v.clone()),
        Err(e) => json!({ "pass": false, "error": e }),
    };
    let pass = result["pass"] == true && cleanup.is_empty();
    let mut readings = result["readings"].clone();
    if !readings.is_object() {
        readings = json!({});
    }
    readings["requester"] = json!({ "spec": target.requester, "version": bin.version });
    let doc = json!({
        "runbook": runbook, "test": "composition", "target": target.name, "startedAt": started, "endedAt": atlibs_host::iso_now(),
        "pass": pass, "error": result.get("error").cloned().or_else(|| ran.as_ref().ok().and_then(|v| v.get("error").cloned())), "phase": result["phase"],
        "cleanupErrors": cleanup, "readings": readings, "scratch": dir,
    });
    let out = std::env::var("SWC_READINGS")
        .map(PathBuf::from)
        .unwrap_or_else(|_| tests.join("readings"));
    std::fs::create_dir_all(&out)?;
    let file = out.join(format!(
        "{runbook}-composition-{}.json",
        started.replace([':', '.'], "")
    ));
    std::fs::write(&file, serde_json::to_string_pretty(&doc)?)?;
    println!("{}", serde_json::to_string_pretty(&doc)?);
    eprintln!("readings: {}", file.display());
    Ok(pass)
}

#[tokio::main]
async fn main() -> std::process::ExitCode {
    atlibs_host::tls();
    match main_().await {
        Ok(true) => std::process::ExitCode::SUCCESS,
        Ok(false) => std::process::ExitCode::FAILURE,
        Err(e) => {
            eprintln!("swc-test-host: {e:#}");
            std::process::ExitCode::from(2)
        }
    }
}
