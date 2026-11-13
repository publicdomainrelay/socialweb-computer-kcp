//! swc-test-host: runs a test composition (tests/compositions/<runbook>) against SWC_TARGET as an account, with
//! atlibs-kube, atlibs-ssh, atlibs-iroh, identlink and the socialweb handler; the root instance `test` runs `run`.
//!
//!   swc-test-host <composition dir> [--account a] [--on-fail freeze|cleanup] [--freeze-at <step|phase>]...
//!                 [--freeze-ttl <seconds>] [--dry-run] [--instance i --entry e --input '<json>']
//!   swc-test-host attach <run dir>     stdio MCP to a live run's control socket
//!   swc-test-host cleanup <run dir>    from resources.json (the host may be gone)
//!   swc-test-host replay <run dir> [--upto <step>]
//!
//! A failure (pass false, an error) freezes the run when --on-fail freeze (the default outside CI): no more events,
//! every resource kept, the dump in the run dir (state.json, events.ndjson, resources.json, failure.json), the
//! control socket served (MCP), until `continue`/`cleanup` or the TTL (default 2 h), then cleanup. SIGUSR1 freezes a
//! live run. The readings land in tests/readings as the Rust tests' do.

mod swc;

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result, anyhow};
use atlibs_host::compose::{self, Composition, ModuleLoader};
use atlibs_host::freeze::{Break, Freezer, Tool};
use atlibs_host::host::Fetch;
use atlibs_host::run::{Call, Module, Options, Request};
use atlibs_host::store::Store;
use atlibs_host::wasm::{Bound, Wasm};
use atlibs_host::{BoxFut, Signal};
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

fn wasm_path(modules: &Path, key: &str) -> PathBuf {
    modules
        .join("target/wasm32-unknown-unknown/release")
        .join(format!("{}.wasm", key.replace('-', "_")))
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
            let wasm = wasm_path(&modules, &key);
            let bytes = std::fs::read(&wasm)
                .map_err(|e| format!("{}: {e} (cargo wasm in tests/modules)", wasm.display()))?;
            let mut m = Module::from_manifest(bytes, &manifest)?;
            m.id = Some(dir.display().to_string());
            // a local module has no published session record: module.json `scope` stands in for its client scope
            m.scope = manifest["scope"].as_str().map(|s| (key.clone(), s.to_string()));
            m.key = Some(key);
            Ok(m)
        })
    })
}

/// One line of what a tick saw and did: the event (its id; a request's message and phase), the calls and timers it
/// made (ids and NSIDs), and its end. Never a call's params or a result's output.
fn progress(event: &Value, out: &Value) {
    let at = atlibs_host::iso_now();
    let data = &event["data"];
    let said = [
        data["msg"].as_str(),
        data["phase"].as_str(),
        data["contractPhase"].as_str(),
    ]
    .into_iter()
    .flatten()
    .filter(|s| !s.is_empty())
    .collect::<Vec<_>>()
    .join(" ");
    let woke = format!(
        "{} {}",
        event["type"].as_str().unwrap_or("?"),
        event["id"].as_str().unwrap_or_default()
    );
    let calls: Vec<String> = (out["calls"].as_array().into_iter().flatten())
        .map(|c| {
            format!(
                "{}={}",
                c["id"].as_str().unwrap_or("?"),
                c["nsid"]
                    .as_str()
                    .unwrap_or("?")
                    .trim_start_matches("computer.socialweb.test.")
            )
        })
        .collect();
    let timers: Vec<&str> = (out["timers"].as_array().into_iter().flatten())
        .filter_map(|t| t["id"].as_str())
        .collect();
    let mut line = format!("{at} {}", woke.trim());
    if !said.is_empty() {
        line += &format!(" ({said})");
    }
    if !calls.is_empty() {
        line += &format!(" -> {}", calls.join(" "));
    }
    if !timers.is_empty() {
        line += &format!(" timers {}", timers.join(" "));
    }
    if out["done"] == true {
        line += &format!(" done pass={}", out["result"]["pass"]);
    }
    println!("{line}");
}

fn flag(args: &[String], f: &str) -> Option<String> {
    args.iter()
        .position(|a| a == f)
        .and_then(|i| args.get(i + 1))
        .cloned()
}

fn flags(args: &[String], f: &str) -> Vec<String> {
    args.windows(2)
        .filter(|w| w[0] == f)
        .map(|w| w[1].clone())
        .collect()
}

fn tool(
    name: &str,
    description: &str,
    schema: Value,
    f: impl Fn(Value) -> BoxFut<'static, Result<Value, String>> + Send + Sync + 'static,
) -> Tool {
    Tool {
        name: name.into(),
        description: description.into(),
        schema,
        call: Arc::new(f),
    }
}

async fn run_dir_cmd(cmd: &str, args: &[String]) -> Result<bool> {
    let dir = PathBuf::from(args.first().context("a run dir")?);
    match cmd {
        "attach" => {
            atlibs_host::freeze::attach(&dir.join("control.sock"))
                .await
                .map_err(|e| anyhow!(e))?;
            Ok(true)
        }
        "cleanup" => {
            let meta: Value =
                serde_json::from_slice(&std::fs::read(dir.join("run.json")).context("run.json")?)?;
            let target = Target::load()?;
            let bin =
                Binary::resolve(meta["requester"].as_str().unwrap_or(&target.requester)).await?;
            let errors = swc::cleanup_from_disk(
                &dir,
                &target,
                meta["account"].as_str().unwrap_or("alice"),
                &bin,
            )
            .await;
            println!("{}", json!({ "cleanupErrors": errors }));
            Ok(errors.is_empty())
        }
        "replay" => {
            // every tick again from ticks.ndjson.zst, the whole composition in seq order; --to <seq> stops there and
            // prints every run's state; --modules <dir>: the wasm of another build (tests/modules by default)
            let meta: Value =
                serde_json::from_slice(&std::fs::read(dir.join("run.json")).context("run.json")?)?;
            // the composition where the run had it, else this checkout's of the same name (a CI run's artifacts
            // name the CI VM's checkout)
            let recorded = PathBuf::from(meta["composition"].as_str().context("run.json composition")?);
            let comp = match recorded.join("composition.json").exists() {
                true => recorded.join("composition.json"),
                false => testkit::repo()
                    .join("tests/compositions")
                    .join(recorded.file_name().context("run.json composition")?)
                    .join("composition.json"),
            };
            let manifest: Value = serde_json::from_slice(
                &std::fs::read(&comp).with_context(|| comp.display().to_string())?,
            )?;
            let modules = flag(args, "--modules")
                .map(PathBuf::from)
                .unwrap_or_else(|| testkit::repo().join("tests/modules"));
            let wasm = Wasm::new(5000);
            let mut runners: HashMap<String, Arc<dyn atlibs_host::run::Runner>> = HashMap::new();
            for inst in manifest["instances"].as_array().into_iter().flatten() {
                let (name, key) = (
                    inst["name"].as_str().unwrap_or_default(),
                    inst["module"].as_str().unwrap_or_default(),
                );
                let bytes = std::fs::read(wasm_path(&modules, key))
                    .with_context(|| format!("{key}: the wasm (cargo wasm)"))?;
                runners.insert(
                    name.to_string(),
                    Arc::new(Bound {
                        wasm: wasm.clone(),
                        bytes: Arc::new(bytes),
                        sha256: None,
                    }),
                );
            }
            let records = atlibs_host::ticks::read(&dir).map_err(|e| anyhow!(e))?;
            let to = flag(args, "--to").and_then(|s| s.parse().ok());
            let mut r =
                atlibs_host::ticks::replay(&records, to, |i| runners.get(i).cloned(), 1 << 20)
                    .await
                    .map_err(|e| anyhow!(e))?;
            if let Some(seq) = to {
                r["statesAt"] = Value::Object(atlibs_host::ticks::states_at(&records, seq));
            }
            println!("{}", serde_json::to_string_pretty(&r)?);
            Ok(r["firstDiverge"].is_null())
        }
        _ => unreachable!(),
    }
}

async fn main_() -> Result<bool> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if let Some(cmd) = args
        .first()
        .filter(|c| ["attach", "cleanup", "replay"].contains(&c.as_str()))
    {
        return run_dir_cmd(cmd, &args[1..]).await;
    }
    let comp_dir = PathBuf::from(
        args.first()
            .context("usage: swc-test-host <composition dir> | attach|cleanup|replay <run dir>")?,
    );
    let account = flag(&args, "--account").unwrap_or_else(|| "alice".into());
    let dry = args.iter().any(|a| a == "--dry-run");
    let freeze_on_fail =
        flag(&args, "--on-fail").map_or(std::env::var("CI").is_err(), |v| v == "freeze");
    let ttl = Duration::from_secs(
        flag(&args, "--freeze-ttl")
            .and_then(|s| s.parse().ok())
            .unwrap_or(7200),
    );
    let breaks: Vec<Break> = flags(&args, "--freeze-at")
        .iter()
        .map(|s| Break::parse(s))
        .collect();
    let tests = testkit::repo().join("tests");
    let manifest: Value = serde_json::from_slice(
        &std::fs::read(comp_dir.join("composition.json")).context("composition.json")?,
    )?;
    let runbook = comp_dir.file_name().unwrap().to_string_lossy().to_string();

    let target = Target::load()?;
    let acc = target.account(&account)?;
    let bin = Binary::resolve(&target.requester).await?;
    let comp = compose::load(&manifest, &loader(tests.join("modules")))
        .await
        .map_err(|e| anyhow!(e))?;
    // two dirs per run: the dump (SWC_RUNS: ticks, state, failure, resources, run.json; no secret, a CI artifact) and
    // the work dir (requester state, kubeconfigs, keys, the store: secrets, never uploaded)
    let run_name = format!("{runbook}-composition-{}", atlibs_host::now_ms());
    let dump = testkit::runs_dir().join(&run_name);
    let dir = testkit::home_tmp("work").join(&run_name);
    for d in [&dump, &dir] {
        std::fs::create_dir_all(d)?;
        std::fs::set_permissions(d, std::os::unix::fs::PermissionsExt::from_mode(0o700))?;
    }

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
        dump.clone(),
    );
    let transports = vec![
        kube.transport(),
        ssh.transport(),
        atlibs_iroh::transport(testkit::home_tmp("iroh_ed25519")),
        swc.transport(),
    ];
    let provides = Arc::new(atlibs_host::transport::provides(&transports));
    compose::check(&comp, &provides).map_err(|e| anyhow!(e))?;
    // a required hook outside com.fedfork.exec.* needs an edge at run time (host transports serve only that prefix)
    for (name, m) in &comp.modules {
        for hk in &m.hooks {
            let routed = comp.manifest["edges"]
                .as_array()
                .into_iter()
                .flatten()
                .any(|e| e["from"] == name.as_str() && e["nsid"] == hk.nsid.as_str());
            if hk.required && !hk.nsid.starts_with("com.fedfork.exec.") && !routed {
                anyhow::bail!(
                    "instance {name}: required hook {} has no edge (a host call is a call, not a hook)",
                    hk.nsid
                );
            }
        }
    }
    // a dry run touches no account, and the guest requester has no creds: they are read only to run as one
    if !dry && !acc.guest_requester {
        acc.password()?;
    }
    if dry {
        println!(
            "dry run {runbook}: composition ok ({} instances), requester {}",
            comp.modules.len(),
            bin.version
        );
        return Ok(true);
    }

    let instance = flag(&args, "--instance").unwrap_or_else(|| "test".into());
    let entry = flag(&args, "--entry").unwrap_or_else(|| "run".into());
    let signed_in = flag(&args, "--instance").is_none() && !acc.guest_requester;
    let root_key = manifest["instances"]
        .as_array()
        .into_iter()
        .flatten()
        .find(|i| i["name"] == instance.as_str())
        .and_then(|i| i["module"].as_str())
        .unwrap_or_default()
        .to_string();
    std::fs::write(
        dump.join("run.json"),
        serde_json::to_string_pretty(
            &json!({ "composition": comp_dir.canonicalize()?, "account": account, "requester": target.requester, "rootModule": root_key, "instance": instance, "entry": entry, "work": dir }),
        )?,
    )?;
    let _lock = if signed_in {
        Some(testkit::lock_account(&acc.handle).await?)
    } else {
        None
    };
    let store = Store::open(&dir.join("store"), "swc-test-host").map_err(|e| anyhow!(e))?;
    let (session, fetch): (_, Arc<dyn Fetch>) = if signed_in {
        let at = testkit::Atproto::new(&target);
        (Some(at.session(acc).await?), Arc::new(at.http.clone()))
    } else {
        (None, Arc::new(reqwest::Client::new()))
    };
    let mut calls = HashMap::new();
    for (name, module) in &comp.modules {
        calls.insert(
            name.clone(),
            atlibs_host::registry::module_call(
                &store,
                fetch.clone(),
                session.clone(),
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
    let wasm = Wasm::new(5000);

    // freeze: the gate, the recorder, SIGUSR1, the control socket (MCP) with this host's tools
    // every tick of every instance (ticks.ndjson.zst), a full state each 50 ticks of a run
    let ticks = atlibs_host::ticks::Ticks::new(&dump, 50);
    let freezer = Freezer::new(&dump, breaks, Some(ticks.clone()));
    let signal = Signal::new();
    {
        let f = freezer.clone();
        tokio::spawn(async move {
            if let Ok(mut usr1) =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::user_defined1())
            {
                while usr1.recv().await.is_some() {
                    f.freeze("SIGUSR1");
                }
            }
        });
    }
    let (cleaned, clean_now) = (Arc::new(tokio::sync::Notify::new()), signal.clone());
    let host_tools = {
        let (s, k, c1, cf, comp2, w2, cl, sig) = (
            swc.clone(),
            kube.clone(),
            cleaned.clone(),
            call_for.clone(),
            comp.clone(),
            wasm.clone(),
            clean_now.clone(),
            signal.clone(),
        );
        let fz = freezer.clone();
        let pv2 = provides.clone();
        let kget = k.clone();
        let klist = k.clone();
        let klogs = k.clone();
        let s2 = swc.clone();
        let s3 = swc.clone();
        let s4 = swc.clone();
        let obj = json!({ "type": "object", "properties": { "cluster": { "type": "string" }, "apiVersion": { "type": "string" }, "kind": { "type": "string" }, "name": { "type": "string" }, "namespace": { "type": "string" } }, "required": ["cluster", "apiVersion", "kind"] });
        vec![
            tool("resources", "What the run started and created: requesters, invocations (receipt, winner, ssh), kube objects, clusters, records, keys.", json!({ "type": "object", "properties": {} }), move |_| {
                let s = s.clone();
                Box::pin(async move { Ok(s.resources().await) })
            }),
            tool("kube_get", "Get one object of any kcp or cluster of the run.", obj.clone(), move |a| {
                let k = kget.clone();
                Box::pin(async move { k.call(atlibs_kube::GET.into(), Request { input: Some(a), ..Default::default() }).await.map(Option::unwrap_or_default) })
            }),
            tool("kube_list", "List a kind on a kcp or cluster of the run.", obj.clone(), move |a| {
                let k = klist.clone();
                Box::pin(async move { k.call(atlibs_kube::LIST.into(), Request { input: Some(a), ..Default::default() }).await.map(Option::unwrap_or_default) })
            }),
            tool("kube_logs", "A pod's log on a tenant cluster: {cluster, namespace, pod}.", json!({ "type": "object", "properties": { "cluster": {}, "namespace": {}, "pod": {} }, "required": ["cluster", "pod"] }), move |a| {
                let k = klogs.clone();
                Box::pin(async move {
                    let path = format!("/api/v1/namespaces/{}/pods/{}/log", a["namespace"].as_str().unwrap_or("default"), a["pod"].as_str().unwrap_or_default());
                    k.call(atlibs_kube::RAW.into(), Request { input: Some(json!({ "cluster": a["cluster"], "path": path })), ..Default::default() }).await.map(Option::unwrap_or_default)
                })
            }),
            tool("ssh_exec", "Run a command in a run's VM: {name (the run request invocation), exec, transport?}.", json!({ "type": "object", "properties": { "name": {}, "exec": {}, "transport": {} }, "required": ["name", "exec"] }), move |a| {
                let s = s2.clone();
                Box::pin(async move { s.call("computer.socialweb.test.request.ssh".into(), Request { input: Some(a), ..Default::default() }).await.map(Option::unwrap_or_default) })
            }),
            tool("requester_ssh", "Run a command in a Held contract's VM of a run requester: {name, contract, exec, transport?}.", json!({ "type": "object", "properties": { "name": {}, "contract": {}, "exec": {}, "transport": {} }, "required": ["name", "contract", "exec"] }), move |a| {
                let s = s3.clone();
                Box::pin(async move { s.call("computer.socialweb.test.requester.ssh".into(), Request { input: Some(a), ..Default::default() }).await.map(Option::unwrap_or_default) })
            }),
            tool("record_get", "Read a record {uri} (public, the PLC the target names).", json!({ "type": "object", "properties": { "uri": { "type": "string" } }, "required": ["uri"] }), move |a| {
                let s = s4.clone();
                Box::pin(async move {
                    let at = testkit::Atproto::new(&s.target);
                    at.record(a["uri"].as_str().unwrap_or_default()).await.map(|v| json!({ "value": v })).map_err(|e| e.to_string())
                })
            }),
            tool("run_assert", "Run an entry of the composition's assert instance: {entry, input}.", json!({ "type": "object", "properties": { "entry": {}, "input": {} }, "required": ["entry"] }), move |a| {
                let (cf, comp, w, pv) = (cf.clone(), comp2.clone(), w2.clone(), pv2.clone());
                Box::pin(async move {
                    let input = a["input"].as_object().cloned().unwrap_or_default();
                    compose::run_compose(comp, w, None, "assert", a["entry"].as_str().unwrap_or_default(), input, cf, HashMap::new(), None, Options { provides: Some(pv), ..Default::default() }).await
                })
            }),
            tool("cleanup", "End the run now: abort it if it runs, release and delete everything, stop serving.", json!({ "type": "object", "properties": {} }), move |_| {
                let (c1, cl, fz) = (c1.clone(), cl.clone(), fz.clone());
                Box::pin(async move {
                    cl.abort("cleanup from the control socket");
                    fz.thaw();
                    c1.notify_waiters();
                    Ok(json!({ "cleaning": true }))
                })
            }),
        ]
        .into_iter()
        .chain(std::iter::once(tool("thaw", "Same as continue.", json!({ "type": "object", "properties": {} }), {
            let f = freezer.clone();
            move |_| {
                f.thaw();
                Box::pin(async { Ok(json!({ "thawed": true })) })
            }
        })))
        .chain(std::iter::once(tool("abort", "Abort the run (cleanup follows).", json!({ "type": "object", "properties": {} }), move |_| {
            sig.abort("aborted from the control socket");
            Box::pin(async { Ok(json!({ "aborted": true })) })
        })))
        .collect::<Vec<_>>()
    };
    let server = tokio::spawn(freezer.clone().serve(dump.join("control.sock"), host_tools));
    // resources.json every few seconds (kube writes do not pass the socialweb handler); a breakpoint freeze past
    // its TTL aborts the run
    let ticker = {
        let (s, f, sig) = (swc.clone(), freezer.clone(), signal.clone());
        tokio::spawn(async move {
            let mut frozen_for = Duration::ZERO;
            loop {
                tokio::time::sleep(Duration::from_secs(5)).await;
                s.write_resources().await;
                frozen_for = if f.frozen().is_some() {
                    frozen_for + Duration::from_secs(5)
                } else {
                    Duration::ZERO
                };
                if frozen_for >= ttl {
                    sig.abort("the freeze TTL ran out");
                    f.thaw();
                }
            }
        })
    };
    eprintln!(
        "run dir: {} (attach: swc-test-host attach {})",
        dump.display(),
        dump.display()
    );

    let options = Options {
        signal: Some(signal.clone()),
        inputs: freezer.inputs(),
        budget: Some(
            json!({ "calls": "unlimited", "events": "unlimited", "wallMs": "unlimited", "subscriptions": 8, "inFlight": 12 }),
        ),
        provides: Some(provides),
        gate: Some(freezer.gate()),
        tick_for: Some(ticks.tick_for()),
        on_event: {
            // and a progress line per tick on stdout, as the run goes (a CI step's log): names only, never params
            let frozen = freezer.on_event();
            Some(Arc::new(move |event: &Value, out: &Value| {
                frozen(event, out);
                progress(event, out);
            }))
        },
        http: {
            // com.fedfork.exec.http.fetch: the target's own origins only
            let mut allow = serde_json::Map::new();
            for o in [
                target.atirc.clone(),
                Some(target.gateway.url.clone()),
                Some(target.plc.clone()),
                Some(format!("https://ssh.{}", target.gateway.ssh)),
            ]
            .into_iter()
            .flatten()
            {
                allow.insert(o.trim_end_matches('/').to_string(), json!({}));
            }
            Some(Arc::new(
                atlibs_host::run::Http::from_json(fetch.clone(), &Value::Object(allow))
                    .map_err(|e| anyhow!(e))?,
            ))
        },
        ..Default::default()
    };
    let started = atlibs_host::iso_now();
    let name = format!("st-{runbook}-{}", atlibs_host::now_ms() / 1000);
    // what a test module knows of the target: no credential, no path
    let bidders: Vec<Value> = target
        .bidders
        .iter()
        .map(|b| json!({ "name": b.name, "did": b.did, "operator": b.operator, "cluster": b.kcp.as_ref().map(|_| format!("bidder:{}", b.name)) }))
        .collect();
    let mut input: serde_json::Map<String, Value> = json!({
        "did": acc.did, "handle": acc.handle, "name": name, "target": target.name, "bidders": bidders,
        "gateway": { "url": target.gateway.url, "aud": target.gateway.aud, "ssh": target.gateway.ssh, "requesterDid": target.gateway.requester_did, "cluster": target.gateway.kcp.as_ref().map(|_| "gateway") },
        "atirc": target.atirc, "plc": target.plc, "pds": target.pds, "requester": target.requester, "restartSshMaxS": target.restart_ssh_max_s, "perHost": args.iter().any(|a| a == "--per-host"), "guestRequester": acc.guest_requester,
    })
    .as_object()
    .unwrap()
    .clone();
    if let Some(i) = flag(&args, "--input") {
        // the root run's --input adds to what it knows of the target (`{"suite": false}`); another entry's is all of it
        let given: serde_json::Map<String, Value> = serde_json::from_str(&i)?;
        match signed_in || acc.guest_requester {
            true if flag(&args, "--instance").is_none() => input.extend(given),
            _ => input = given,
        }
    }
    let ran = compose::run_compose(
        comp,
        wasm,
        Some(store.ledger("managed_records")),
        &instance,
        &entry,
        input,
        call_for,
        HashMap::new(),
        None,
        options,
    )
    .await;
    let result = match &ran {
        Ok(v) if signed_in => v.get("result").cloned().unwrap_or_else(|| v.clone()),
        Ok(v) => {
            json!({ "pass": v.get("error").is_none_or(Value::is_null), "error": v.get("error"), "result": v })
        }
        Err(e) => json!({ "pass": false, "error": e }),
    };
    let failed = result["pass"] != true;
    ticks.flush();
    swc.write_resources().await;
    if failed && freeze_on_fail && signal.reason().is_none() {
        freezer.set_failure(
            json!({ "error": result.get("error"), "phase": result.get("phase"), "result": result }),
        );
        freezer.freeze("failure");
        eprintln!(
            "FROZEN: {} (attach: swc-test-host attach {}; thaws in {:?})",
            result["error"],
            dump.display(),
            ttl
        );
        tokio::select! {
            _ = freezer.wait_thawed(ttl) => {}
            _ = cleaned.notified() => {}
        }
    }
    // the run's kube objects first (a tenant cluster's pods, so its nodes scale down), then invocations and requesters
    let mut cleanup = kube.cleanup().await;
    cleanup.extend(swc.cleanup().await);
    swc.write_resources().await;
    ticker.abort();
    server.abort();
    let _ = std::fs::remove_file(dump.join("control.sock"));
    let pass = !failed && cleanup.is_empty();
    let mut readings = result["readings"].clone();
    if !readings.is_object() {
        readings = json!({});
    }
    readings["requester"] = json!({ "spec": target.requester, "version": bin.version });
    let doc = json!({
        "runbook": runbook, "test": "composition", "target": target.name, "startedAt": started, "endedAt": atlibs_host::iso_now(),
        "pass": pass, "error": result.get("error"), "phase": result.get("phase"), "cleanupErrors": cleanup, "readings": readings, "dump": dump, "work": dir,
    });
    if signed_in {
        let out = std::env::var("SWC_READINGS")
            .map(PathBuf::from)
            .unwrap_or_else(|_| tests.join("readings"));
        std::fs::create_dir_all(&out)?;
        let file = out.join(format!(
            "{runbook}-composition-{}.json",
            started.replace([':', '.'], "")
        ));
        std::fs::write(&file, serde_json::to_string_pretty(&doc)?)?;
        eprintln!("readings: {}", file.display());
    }
    println!("{}", serde_json::to_string_pretty(&doc)?);
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
