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

/// Log lines of the bidder's core that mean the restarted core started or stopped THIS VM's unit: a VM unit that is
/// Active is adopted silently (hostprocess supervisor markAdopted), so any of these after the restart is a restart
/// of the VM. [text, text...]: a line holds all of them.
fn vm_lines(vm: &str) -> Value {
    json!([
        ["\"msg\":\"placed\"", vm],
        ["\"msg\":\"stopped\"", vm],
        ["work dir and API socket removed", vm],
        ["\"msg\":\"shutdown\"", vm],
        ["the process wait saw the VMM exit", vm],
        ["bidder_vm_terminated", vm],
        ["a released bid is marked terminated", vm],
    ])
}

/// The positive controls: the restarted core really started (a dev network's core logs `core holds the instance`
/// with a new generation; an installed bidder's logs `adopted` for each of its infrastructure units, role kine-a,
/// front, kcp-a). Index 0 and 1 of the counts.
fn control_lines() -> Value {
    json!(["core holds the instance", ["\"msg\":\"adopted\"", "\"role\":"]])
}

fn counts(o: &Value, texts: &Value) -> Vec<u64> {
    texts
        .as_array()
        .into_iter()
        .flatten()
        .map(|t| {
            let k = match t {
                Value::Array(a) => a.iter().filter_map(Value::as_str).collect::<Vec<_>>().join(" & "),
                t => t.as_str().unwrap_or_default().to_string(),
            };
            o["counts"][k].as_u64().unwrap_or(0)
        })
        .collect()
}

fn fcvm(id: &str, cluster: &str, name: &str) -> abi::Call {
    kube(id, "get", cluster, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "FirecrackerVM", "name": name }))
}

/// The VM's HostProcess `vm-<name>`: status.runtime is the unit as the supervisor last read it (MainPID, InvocationID,
/// ActiveEnterTimestamp).
fn hproc(id: &str, cluster: &str, vm: &str) -> abi::Call {
    kube(id, "get", cluster, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "HostProcess", "plural": "hostprocesses", "name": format!("vm-{vm}") }))
}

fn cvms(id: &str, cluster: &str) -> abi::Call {
    kube(id, "list", cluster, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }))
}

/// restart (staging): the winner's core restarted in place (`dev restart <network> <bidder>/core`, the upgrade path, not a
/// drain) under this run's Held VM. The VM's own unit is identified by its FirecrackerVM runtime (pid, invocationID,
/// startTicks, unit) before and after and must be unchanged; its ComputeVM is read every 2 s from before the restart
/// to 30 s after it and is Running every time (never Lost or Releasing); the guest's boot id is unchanged and ssh
/// answers; the new core's log has none of vm_lines() for it (it adopts an Active unit silently); no vm.terminated; the
/// contract never leaves Held. The restart itself must show (control_lines: a new core generation, or the infra
/// units adopted). Then the release leaves no record.
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
    let logs = |id: &str, s: &S| {
        let mut texts = control_lines().as_array().cloned().unwrap_or_default();
        texts.extend(vm_lines(&vm).as_array().cloned().unwrap_or_default());
        host(id, "bidder.logCount", json!({ "name": s.st["bidder"], "texts": texts }))
    };
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
        // the watch: the ComputeVM every 2 s, from before the restart to 30 s after it returns
        Event::Timer { id } if id == "watch" => {
            if s.is("restarting") || s.is("watching") {
                h = h.call(cvms("w", &cluster)).timer("watch", 2_000);
            }
        }
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
                h = h.call(boot("boot0")).call(cvms("vms", c));
            }
        }
        Event::Result { id, result, .. } => {
            let r = result.output();
            if id == "term" {
                if let Ok(o) = r {
                    return Ok(s.fail(format!("vm.terminated after the restart: {}", o["value"])));
                }
                s.flag("noTerm");
            } else if id == "w" && !(s.is("restarting") || s.is("watching")) {
                // a list that came back after the watch ended (the release has begun): not read
            } else if id == "w" {
                // an unreadable list during the restart is a reading, not a verdict; a VM that is not Running is
                let Ok(o) = r else {
                    s.st["unreadable"] = json!(s.st["unreadable"].as_u64().unwrap_or(0) + 1);
                    return Ok(s.keep(h));
                };
                let mine = o["items"].as_array().into_iter().flatten().find(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]);
                s.st["watched"] = json!(s.st["watched"].as_u64().unwrap_or(0) + 1);
                match mine {
                    Some(v) if v["status"]["phase"] == "Running" && v["metadata"]["deletionTimestamp"].is_null() && !v["status"]["lostReason"].is_string() => {}
                    other => {
                        return Ok(s.fail(format!("the ComputeVM left Running during the restart: {}", other.map(|v| json!({ "phase": v["status"]["phase"], "lostReason": v["status"]["lostReason"], "deleting": v["metadata"]["deletionTimestamp"] })).unwrap_or(json!("gone")))));
                    }
                }
                if s.is("watching") && s.since("restarted") > 30.0 {
                    s.set("verify");
                    s.reading("watched", s.st["watched"].clone());
                    s.reading("watchUnreadable", s.st["unreadable"].clone());
                    h = h.call(fcvm("fc1", &cluster, &vm)).call(hproc("hp1", &cluster, &vm)).call(boot("boot1")).call(logs("log1", &s)).call(host(
                        "adopt",
                        "bidder.logLines",
                        json!({ "name": s.st["bidder"], "texts": [["hostprocess_unit_adopted", format!("vm-{vm}")]], "keys": ["hostprocess", "unit", "pid", "invocation", "activeSince", "state"] }),
                    ));
                    let rcpt = s.st["receipt"].as_str().unwrap_or_default().to_string();
                    let wd = rcpt.trim_start_matches("at://").split('/').next().unwrap_or_default().to_string();
                    h = h.call(get_record("term", &format!("at://{wd}/{VM_TERMINATED}/{}", termination_rkey(&rcpt))));
                }
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
                        let n = v["metadata"]["name"].as_str().unwrap_or_default();
                        h = h.call(fcvm("fc0", &cluster, n)).call(hproc("hp0", &cluster, n));
                    }
                    "hp0" | "hp1" => {
                        let rt = &o["status"]["runtime"];
                        if rt["pid"].as_i64().unwrap_or(0) == 0 || rt["invocationID"].as_str().unwrap_or_default().is_empty() {
                            return Ok(s.fail(format!("the HostProcess vm-{vm} has no runtime pid and invocationID ({id}): {}", o["status"])));
                        }
                        s.st[id.as_str()] = json!({ "pid": rt["pid"], "invocationID": rt["invocationID"], "unit": rt["unit"], "startTicks": rt["startTicks"], "activeSince": rt["activeSince"], "phase": o["status"]["phase"] });
                    }
                    "fc0" | "fc1" => {
                        let rt = &o["status"]["runtime"];
                        if rt["pid"].as_i64().unwrap_or(0) == 0 || rt["invocationID"].as_str().unwrap_or_default().is_empty() {
                            return Ok(s.fail(format!("the FirecrackerVM has no runtime pid and invocationID ({id}): {}", o["status"])));
                        }
                        let k = if id == "fc0" { "rt0" } else { "rt1" };
                        s.st[k] = json!({ "pid": rt["pid"], "invocationID": rt["invocationID"], "unit": rt["unit"], "startTicks": rt["startTicks"], "startTime": rt["startTime"], "phase": o["status"]["phase"], "lostReason": o["status"]["lostReason"] });
                    }
                    "restart" => {
                        s.reading("restart_s", o["seconds"].clone());
                        s.set("watching");
                        s.mark("restarted");
                    }
                    "adopt" => s.st["adopt"] = o["lines"].clone(),
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
                if id == "log0" || id == "log1" {
                    let mut texts = control_lines().as_array().cloned().unwrap_or_default();
                    texts.extend(vm_lines(&vm).as_array().cloned().unwrap_or_default());
                    s.st[if id == "log0" { "c0" } else { "c1" }] = json!(counts(&o, &json!(texts)));
                }
            }
            // before: the guest's boot id, the VM's runtime and the log counts, then the restart with the watch
            if s.is("before") && s.st["boot0"].is_string() && s.st["rt0"].is_object() && s.st["hp0"].is_object() && !s.has("logAsked") {
                s.flag("logAsked");
                h = h.call(logs("log0", &s));
            } else if s.is("before") && s.st["c0"].is_array() {
                s.reading("before", json!({ "firecrackervm": s.st["rt0"], "hostprocess": s.st["hp0"] }));
                s.set("restarting");
                s.mark("restarted");
                h = h.call(host("restart", "bidder.restart", json!({ "name": s.st["bidder"] }))).call(cvms("w", &cluster)).timer("watch", 2_000);
            }
            // verify: the three answers in, then the verdict, then the release
            if s.is("verify") && ["rt1", "hp1", "boot1", "c1", "adopt"].iter().all(|k| !s.st[*k].is_null()) && s.has("noTerm") {
                let (a, b) = (s.st["rt0"].clone(), s.st["rt1"].clone());
                let (c0, c1): (Vec<u64>, Vec<u64>) = (
                    s.st["c0"].as_array().into_iter().flatten().map(|v| v.as_u64().unwrap_or(0)).collect(),
                    s.st["c1"].as_array().into_iter().flatten().map(|v| v.as_u64().unwrap_or(0)).collect(),
                );
                let mut p = vec![];
                for k in ["pid", "invocationID", "unit", "startTicks", "startTime"] {
                    if a[k] != b[k] {
                        p.push(format!("the VM's VMM changed: {k} {} -> {}", a[k], b[k]));
                    }
                }
                // the unit as the supervisor reads it: MainPID, InvocationID, ActiveEnterTimestamp, before and after
                let (u, v) = (s.st["hp0"].clone(), s.st["hp1"].clone());
                for k in ["pid", "invocationID", "unit", "startTicks", "activeSince"] {
                    if u[k] != v[k] {
                        p.push(format!("the VM's unit changed: {k} {} -> {}", u[k], v[k]));
                    }
                }
                if b["phase"] != "Running" || b["lostReason"].is_string() {
                    p.push(format!("the VM is {} ({}) after the restart", b["phase"], b["lostReason"]));
                }
                if s.st["boot0"] != s.st["boot1"] {
                    p.push(format!("the guest rebooted: {} -> {}", s.st["boot0"], s.st["boot1"]));
                }
                // the restart really happened: a new core generation, or the infra units adopted (>= 3 role lines)
                let newgen = c1.first().copied().unwrap_or(0).saturating_sub(c0.first().copied().unwrap_or(0));
                let infra = c1.get(1).copied().unwrap_or(0).saturating_sub(c0.get(1).copied().unwrap_or(0));
                s.reading("controls", json!({ "newCoreGenerations": newgen, "infraAdopted": infra }));
                if newgen == 0 && infra < 3 {
                    p.push("the bidder's log shows no restart (no new `core holds the instance`, fewer than 3 adopted infra units)".to_string());
                }
                // the new core's supervisor adopted exactly this VM's unit (hostprocess_unit_adopted: pid, invocation and
                // activeSince of the unit as the supervisor read it), the one the FirecrackerVM named before
                let ad = s.st["adopt"].as_array().cloned().unwrap_or_default();
                s.reading("adopted", json!(ad));
                match ad.as_slice() {
                    [l] => {
                        if l["pid"].as_i64() != u["pid"].as_i64() || l["invocation"] != u["invocationID"] {
                            p.push(format!("the adopted unit is not the VM's: pid {} invocation {} (before: {} {})", l["pid"], l["invocation"], u["pid"], u["invocationID"]));
                        }
                        // activeSince: the unit's ActiveEnterTimestamp, as the supervisor read it before (to the second)
                        if l["activeSince"].as_str().zip(u["activeSince"].as_str()).is_none_or(|(x, y)| (steps::secs(x) - steps::secs(y)).abs() > 1.0) {
                            p.push(format!("activeSince {} is not the unit's before the restart {}", l["activeSince"], u["activeSince"]));
                        }
                        if !l["state"].as_str().unwrap_or_default().eq_ignore_ascii_case("active") {
                            p.push(format!("the adopted unit's state: {}", l["state"]));
                        }
                    }
                    _ => p.push(format!("{} hostprocess_unit_adopted lines for vm-{vm}, one expected", ad.len())),
                }
                // the VM's own lines: none
                let names = vm_lines(&vm);
                let mut touched = vec![];
                for (i, t) in names.as_array().into_iter().flatten().enumerate() {
                    let n = c1.get(i + 2).copied().unwrap_or(0).saturating_sub(c0.get(i + 2).copied().unwrap_or(0));
                    if n > 0 {
                        touched.push(format!("{t} x{n}"));
                    }
                }
                s.reading("vmLinesAfterRestart", json!(touched));
                if !touched.is_empty() {
                    p.push(format!("the restarted core touched the VM's unit: {}", touched.join(", ")));
                }
                s.reading("after", b);
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
