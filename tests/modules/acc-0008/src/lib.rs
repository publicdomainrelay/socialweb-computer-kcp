// docs/acceptance/0008-host-observer.md: a VM's timeline on its host (Running there while Held), the release, the
// vm.terminated pinned by one getRecord at its computed key (deleted_on_request), the release's records gone, the
// orphan check on every host the target reaches: CONFIRMED (identity Ready, 0 startup ERROR, 0 automatic restarts),
// nothing of the receipt, no VM process without a Running ComputeVM. Without the winner's kcp its timeline is skipped.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, out, termination_rkey};

fn run_0008(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let vms = |id: &str, c: &str| kube(id, "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }));
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": ["--hold"] }))).timer("deadline", 30 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 30 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "contract" => {
            let w = json!(s.st["receipt"].as_str().unwrap_or_default().trim_start_matches("at://").split('/').next().unwrap_or_default());
            s.st["winner"] = w.clone();
            s.reading("winner", w.clone());
            if !bidders.iter().any(|b| b["did"] == w) {
                return Ok(s.fail(format!("unknown bidder {w}: the winner is not in the target")));
            }
            match bidders.iter().find(|b| b["did"] == w).and_then(|b| b["cluster"].as_str()) {
                Some(c) => {
                    s.st["cluster"] = json!(c);
                    h = h.call(vms("running", c));
                }
                None => {
                    s.reading("host", "not read: no kcp for the winner");
                    s.mark("release");
                    h = h.call(host("release", "request.release", json!({ "name": name })));
                }
            }
        }
        Event::Timer { id } if id == "poll" => {
            h = h.call(get_record("term", &format!("at://{}/{VM_TERMINATED}/{}", s.st["winner"].as_str().unwrap_or_default(), termination_rkey(s.st["receipt"].as_str().unwrap_or_default()))));
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Failed" && !s.has("held") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                "exited" if !s.has("held") => return Ok(s.fail("run request exited before the VM was Held")),
                "contract" if data["phase"] == "Held" => s.flag("held"),
                _ => {}
            }
            // the --hold process stays (it settles the end): the receipt comes from its log, the winner is its author
            if s.st["receipt"].is_null() && data["receipt"].is_string() {
                s.st["receipt"] = data["receipt"].clone();
            }
            if s.is("held") && s.has("held") && s.st["receipt"].is_string() {
                s.set("summary");
                h = h.timer("contract", 1);
            }
        }
        Event::Result { id, result, .. } => {
            let r = result.output();
            if id == "term" && r.is_err() {
                if s.since("release") > 120.0 {
                    return Ok(s.fail("no vm.terminated 2 min after the release"));
                }
                return Ok(s.keep(h.timer("poll", 3_000)));
            }
            let o = match out(&s, &id, r) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "running" => {
                    let mine: Vec<Value> = o["items"].as_array().into_iter().flatten().filter(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]).map(|v| json!({ "name": v["metadata"]["name"], "phase": v["status"]["phase"] })).collect();
                    if !mine.iter().any(|v| v["phase"] == "Running") {
                        return Ok(s.fail(format!("no Running VM of the receipt on the winner: {mine:?}")));
                    }
                    s.reading("running", json!(mine));
                    s.mark("release");
                    h = h.call(host("release", "request.release", json!({ "name": name })));
                }
                "release" => {
                    s.reading("release_s", o["seconds"].clone());
                    h = h.timer("poll", 1_000);
                }
                "term" => {
                    if o["value"]["reason"] != "deleted_on_request" {
                        return Ok(s.fail(format!("vm.terminated {}", o["value"])));
                    }
                    s.reading("terminated", json!({ "reason": o["value"]["reason"], "request": o["value"]["request"]["uri"], "createdAt": o["value"]["createdAt"] }));
                    h = h.call(host("records", "request.records", json!({ "name": name })));
                }
                "records" => h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] }))),
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left: {} {}", o["left"], o["errors"])));
                    }
                    // every host the target reaches: CONFIRMED (identity Ready, no pending action, 0 startup ERROR,
                    // 0 automatic restarts) and empty of this receipt, no VM process without a Running ComputeVM
                    let reach: Vec<&Value> = bidders.iter().filter(|b| b["cluster"].is_string()).collect();
                    s.set("hosts");
                    s.st["fan"] = json!(reach.len() * 3);
                    if reach.is_empty() {
                        return Ok(s.pass());
                    }
                    for (j, b) in bidders.iter().enumerate().filter(|(_, b)| b["cluster"].is_string()) {
                        let c = b["cluster"].as_str().unwrap_or_default();
                        h = h.call(kube(&format!("mi-{j}"), "get", c, steps::sw("MarketIdentity", "bidder"))).call(vms(&format!("vm-{j}"), c)).call(host(&format!("hl-{j}"), "bidder.health", json!({ "name": b["name"] })));
                    }
                }
                x if s.is("hosts") && x.len() > 3 && ["mi-", "vm-", "hl-"].contains(&&x[..3]) => {
                    let j: usize = x[3..].parse().unwrap_or(0);
                    let name = bidders[j]["name"].as_str().unwrap_or_default().to_string();
                    match &x[..2] {
                        "mi" => {
                            if o["status"]["phase"] != "Ready" || !o["status"]["pendingAction"].is_null() {
                                return Ok(s.fail(format!("{name} MarketIdentity {}", o["status"])));
                            }
                        }
                        "vm" => {
                            let items = o["items"].as_array().cloned().unwrap_or_default();
                            if items.iter().any(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]) {
                                return Ok(s.fail(format!("{name} still runs a VM of the receipt")));
                            }
                            s.st["readings"]["hosts"][&name]["runningVMs"] = json!(items.iter().filter(|v| v["status"]["phase"] == "Running").count());
                        }
                        _ => {
                            if o["error"].is_string() || o["startupErrors"] != 0 || o["restarts"] != 0 {
                                return Ok(s.fail(format!("{name} not CONFIRMED: {o}")));
                            }
                            for k in ["started", "errors", "vmProcesses"] {
                                s.st["readings"]["hosts"][&name][k] = o[k].clone();
                            }
                        }
                    }
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        for (n, r) in s.st["readings"]["hosts"].as_object().cloned().unwrap_or_default() {
                            if r["vmProcesses"].as_u64().unwrap_or(0) > r["runningVMs"].as_u64().unwrap_or(0) {
                                return Ok(s.fail(format!("{n}: {} VM processes, {} Running ComputeVMs (orphans)", r["vmProcesses"], r["runningVMs"])));
                            }
                        }
                        return Ok(s.pass());
                    }
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0008 }
