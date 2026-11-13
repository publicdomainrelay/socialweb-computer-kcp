// docs/acceptance/0007-bidder-termination.md: fault injection on the winner's kcp, scoped to this run's own receipt:
// delete its ComputeVM; it goes within 2 min, its vm.terminated is pinned `deleted_on_request`, the requester deletes
// its contract records (the release's vm.delete too), the host runs nothing of the receipt. Needs the winner's kcp.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, out, sw, termination_rkey};

fn run_0007(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let cluster = s.st["cluster"].as_str().unwrap_or_default().to_string();
    let vms = |id: &str| kube(id, "list", &cluster, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }));
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
            let Some(c) = bidders.iter().find(|b| b["did"] == w).and_then(|b| b["cluster"].as_str()) else {
                return Ok(s.skip(&format!("the target names no kcp for the winner {w}")));
            };
            s.st["cluster"] = json!(c);
            s.set("find");
            h = h.call(kube("vms", "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })));
        }
        Event::Timer { id } if id == "poll" => {
            h = match s.phase().as_str() {
                "gone" | "empty" => h.call(vms("q")),
                "pinned" => h.call(get_record("term", &format!("at://{}/{VM_TERMINATED}/{}", s.st["winner"].as_str().unwrap_or_default(), termination_rkey(s.st["receipt"].as_str().unwrap_or_default())))),
                _ => h,
            };
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Failed" && !s.has("held") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                "exited" if !s.has("held") => return Ok(s.fail("run request exited before the VM was Held")),
                "contract" if data["phase"] == "Held" => s.flag("held"),
                "requester_contract_records_deleted" => s.flag("recordsDeleted"),
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
            if s.is("records") && s.has("recordsDeleted") {
                s.set("check");
                h = h.call(host("records", "request.records", json!({ "name": name })));
            }
        }
        Event::Result { id, result, .. } => {
            let r = result.output();
            if id == "term" && r.is_err() {
                if s.since("delete") > 120.0 {
                    return Ok(s.fail("no vm.terminated 2 min after the delete"));
                }
                return Ok(s.keep(h.timer("poll", 3_000)));
            }
            let o = match out(&s, &id, r) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "vms" => {
                    let mine: Vec<String> = o["items"].as_array().into_iter().flatten().filter(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]).filter_map(|v| v["metadata"]["name"].as_str().map(String::from)).collect();
                    if mine.len() != 1 {
                        return Ok(s.fail(format!("{} ComputeVMs of this run's receipt on the winner", mine.len())));
                    }
                    s.st["vm"] = json!(mine[0]);
                    s.mark("delete");
                    h = h.call(kube("del", "delete", &cluster, sw("ComputeVM", &mine[0])));
                }
                "del" => {
                    s.set("gone");
                    h = h.timer("poll", 2_000);
                }
                "q" => {
                    let there = o["items"].as_array().into_iter().flatten().any(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]);
                    if there && s.since("delete") > 120.0 {
                        return Ok(s.fail("the ComputeVM is still there 2 min after the delete"));
                    }
                    if there {
                        h = h.timer("poll", 2_000);
                    } else if s.is("gone") {
                        let t = s.since("delete");
                        s.reading("gone_s", t);
                        s.set("pinned");
                        h = h.timer("poll", 1_000);
                    } else {
                        return Ok(s.pass());
                    }
                }
                "term" => {
                    if o["value"]["reason"] != "deleted_on_request" {
                        return Ok(s.fail(format!("vm.terminated {}", o["value"])));
                    }
                    s.reading("terminated", json!({ "reason": o["value"]["reason"], "request": o["value"]["request"]["uri"] }));
                    s.set("records");
                    if s.has("recordsDeleted") {
                        s.set("check");
                        h = h.call(host("records", "request.records", json!({ "name": name })));
                    }
                }
                "records" => {
                    s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left: {} {}", o["left"], o["errors"])));
                    }
                    s.set("empty");
                    h = h.call(vms("q"));
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

/// The new core's HostProcess supervisor line for a VM unit it found running (bootstrap.go): msg adopted, its
/// HostProcess vm-<FirecrackerVM name>.
fn adopted(vm: &str) -> Value {
    json!(["\"msg\":\"adopted\"", format!("\"hostprocess\":\"vm-{vm}\"")])
}

fn adopted_key(vm: &str) -> String {
    format!("\"msg\":\"adopted\" & \"hostprocess\":\"vm-{vm}\"")
}

fn fcvm(id: &str, cluster: &str, name: &str) -> abi::Call {
    kube(id, "get", cluster, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "FirecrackerVM", "name": name }))
}

/// restart (staging): the winner's core restarted in place (`dev restart <network> <bidder>/core`, the upgrade path, not a
/// drain) under this run's Held VM: the new core adopts the VM's unit (its log), the VMM pid and invocation unchanged,
/// ssh still answers with the same boot id, the VM Running with no lostReason, no vm.terminated, the contract Held
/// throughout; then the release leaves no record.
fn run_restart(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let cluster = s.st["cluster"].as_str().unwrap_or_default().to_string();
    let vm = s.st["vm"].as_str().unwrap_or_default().to_string();
    let boot = |id: &str| host(id, "request.ssh", json!({ "name": name, "exec": "echo BOOT $(cat /proc/sys/kernel/random/boot_id)" }));
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if args["target"] == "prod" {
                return Ok(s.skip("staging only: it restarts a bidder's core (prod: the rollout's own upgrade)"));
            }
            s.set("held");
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": ["--hold"] }))).timer("deadline", 30 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 30 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => h = h.call(fcvm("fc1", &cluster, &vm)),
        Event::Timer { id } if id == "adopt" => h = h.call(host("log1", "bidder.logCount", json!({ "name": s.st["bidder"], "texts": [adopted(&vm)] }))),
        Event::Message { data, .. } => {
            if s.st["receipt"].is_null() && data["receipt"].is_string() {
                s.st["receipt"] = data["receipt"].clone();
            }
            match (data["msg"].as_str().unwrap_or_default(), data["phase"].as_str().unwrap_or_default()) {
                ("contract", "Held") => s.flag("held"),
                ("contract", p) if s.has("held") && !s.has("releasing") && !p.is_empty() => {
                    return Ok(s.fail(format!("the contract left Held around the restart: {p} {}", data["message"])));
                }
                ("contract", "Failed") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                ("exited", _) if !s.has("releasing") => return Ok(s.fail(format!("run request exited before the release: {data}"))),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.st["receipt"].is_string() {
                let w = s.st["receipt"].as_str().unwrap_or_default().trim_start_matches("at://").split('/').next().unwrap_or_default().to_string();
                s.reading("winner", w.clone());
                let Some(b) = bidders.iter().find(|b| b["did"] == w.as_str()).cloned() else {
                    return Ok(s.fail(format!("unknown bidder {w}: the winner is not in the target")));
                };
                let Some(c) = b["cluster"].as_str() else {
                    return Ok(s.skip(&format!("the target names no kcp for the winner {w}")));
                };
                s.st["cluster"] = json!(c);
                s.st["bidder"] = b["name"].clone();
                s.set("before");
                h = h.call(boot("boot0")).call(kube("vms", "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })));
            }
        }
        Event::Result { id, result, .. } => {
            let r = result.output();
            if id == "term" {
                if let Ok(o) = r {
                    return Ok(s.fail(format!("vm.terminated after the restart: {}", o["value"])));
                }
                s.flag("noTerm");
            } else {
                let o = match out(&s, &id, r) {
                    Ok(o) => o,
                    Err(f) => return Ok(f),
                };
                match id.as_str() {
                    "boot0" | "boot1" => {
                        let Some(b) = o["output"].as_str().unwrap_or_default().lines().find_map(|l| l.strip_prefix("BOOT ")) else {
                            return Ok(s.fail(format!("{id}: ssh into the guest: {}", o["output"])));
                        };
                        s.st[id.as_str()] = json!(b.trim());
                    }
                    "vms" => {
                        let mine: Vec<&Value> = o["items"].as_array().into_iter().flatten().filter(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]).collect();
                        let [v] = mine.as_slice() else {
                            return Ok(s.fail(format!("{} ComputeVMs of this run's receipt on the winner", mine.len())));
                        };
                        s.st["vm"] = v["metadata"]["name"].clone();
                        h = h.call(fcvm("fc0", &cluster, v["metadata"]["name"].as_str().unwrap_or_default()));
                    }
                    "fc0" | "fc1" => {
                        let rt = &o["status"]["runtime"];
                        let k = if id == "fc0" { "rt0" } else { "rt1" };
                        if rt["pid"].as_i64().unwrap_or(0) == 0 {
                            if id == "fc0" {
                                return Ok(s.fail(format!("the FirecrackerVM has no runtime pid: {}", o["status"])));
                            }
                            if s.since("restart") > 120.0 {
                                return Ok(s.fail(format!("no runtime pid 2 min after the restart: {}", o["status"])));
                            }
                            return Ok(s.keep(h.timer("poll", 5_000)));
                        }
                        s.st[k] = json!({ "pid": rt["pid"], "invocationID": rt["invocationID"], "unit": rt["unit"], "phase": o["status"]["phase"], "lostReason": o["status"]["lostReason"] });
                    }
                    "log0" => s.st["adopt0"] = o["counts"][adopted_key(&vm)].clone(),
                    // the new core adopts the VM's unit once its supervisor runs: wait for that, 3 min at most
                    "log1" => {
                        let n = o["counts"][adopted_key(&vm)].as_u64().unwrap_or(0);
                        if n <= s.st["adopt0"].as_u64().unwrap_or(0) {
                            if s.since("restart") > 180.0 {
                                return Ok(s.fail(format!("the restarted core did not adopt the VM's unit vm-{vm} in 3 min")));
                            }
                            return Ok(s.keep(h.timer("adopt", 5_000)));
                        }
                        s.st["adopt1"] = json!(n);
                        s.reading("adopted_s", s.since("restart"));
                        h = h.call(fcvm("fc1", &cluster, &vm));
                    }
                    "restart" => {
                        s.reading("restart_s", o["seconds"].clone());
                        s.set("after");
                        s.mark("restart");
                        h = h.call(host("log1", "bidder.logCount", json!({ "name": s.st["bidder"], "texts": [adopted(&vm)] })));
                    }
                    "release" => {
                        s.reading("release_s", o["seconds"].clone());
                        h = h.call(host("records", "request.records", json!({ "name": name })));
                    }
                    "records" => {
                        s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                        h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                    }
                    "gone" => {
                        if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                            return Ok(s.fail(format!("records left: {} {}", o["left"], o["errors"])));
                        }
                        return Ok(s.pass());
                    }
                    _ => {}
                }
            }
            // before: the boot id, the runtime and the adoption count, then the restart
            if s.is("before") && s.st["boot0"].is_string() && s.st["rt0"].is_object() && s.st["adopt0"].is_null() && !s.has("logAsked") {
                s.flag("logAsked");
                h = h.call(host("log0", "bidder.logCount", json!({ "name": s.st["bidder"], "texts": [adopted(&vm)] })));
            } else if s.is("before") && s.st["adopt0"].is_u64() {
                s.reading("before", s.st["rt0"].clone());
                s.set("restarting");
                h = h.call(host("restart", "bidder.restart", json!({ "name": s.st["bidder"] })));
            }
            // after: the same runtime, the same guest, its adoption logged, no vm.terminated, then the release
            if s.is("after") && s.st["rt1"].is_object() && !s.has("afterAsked") {
                s.flag("afterAsked");
                let rcpt = s.st["receipt"].as_str().unwrap_or_default().to_string();
                let w = rcpt.trim_start_matches("at://").split('/').next().unwrap_or_default().to_string();
                h = h
                    .call(boot("boot1"))
                    .call(get_record("term", &format!("at://{w}/{VM_TERMINATED}/{}", termination_rkey(&rcpt))));
            }
            if s.is("after") && s.st["boot1"].is_string() && s.st["adopt1"].is_u64() && s.has("noTerm") {
                let (a, b) = (&s.st["rt0"], &s.st["rt1"]);
                let mut p = vec![];
                if a["pid"] != b["pid"] || a["invocationID"] != b["invocationID"] {
                    p.push(format!("the VMM changed: {a} -> {b}"));
                }
                if b["phase"] != "Running" || b["lostReason"].is_string() {
                    p.push(format!("the VM is {} ({}) after the restart", b["phase"], b["lostReason"]));
                }
                if s.st["boot0"] != s.st["boot1"] {
                    p.push(format!("the guest rebooted: {} -> {}", s.st["boot0"], s.st["boot1"]));
                }
                s.reading("after", b.clone());
                if !p.is_empty() {
                    return Ok(s.fail(p.join("; ")));
                }
                s.set("release");
                s.flag("releasing");
                h = h.call(host("release", "request.release", json!({ "name": name })));
            }
        }
        Event::Closed { id, error } if id == "req" && !s.has("releasing") => return Ok(s.fail(format!("req closed: {}", error.unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0007, restart => run_restart }

