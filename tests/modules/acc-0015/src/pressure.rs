// docs/acceptance/0015-vm-memory.md, entry `pressure`: the bidder's memory events fire exactly once per crossing and re-arm.
// This run's own VM holds ~55% of its RAM in the guest; the test host lowers THIS VM unit's MemoryHigh (a runtime property of
// the unit in the network bidder's user manager: fault injection on the test's own VM only) to half its usage while the guest
// touches its memory, so memory.events `high` rises again and again for WINDOW_S. The bidder polls memory.events every 15 s and
// publishes one compute.events.vm.memoryPressure {kind high} per crossing, at most one per kind per MemoryTypeGap (10 min), and
// only after MemoryRearmQuiet (5 min without a rise, usage under 90% of the limit) re-arms it. So:
//   crossing 1 (t0): exactly one record, though `high` rose many times
//   crossing 2 (record 1 + 6.5 min, re-armed but inside the gap): still exactly one record
//   crossing 3 (record 1 + 10.75 min, past the gap): exactly two
// and never a `max` record or a vm.oomKilled; the VM stays Held and Running, the guest's boot id never changes; then the release.
// Dev / staging network whose bidder runs VM units under systemd-user (cgroup limits); SKIPs on prod and on a proc bidder.
use abi::{Call, Event, Handle, Run};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, out, secs, termination_rkey};

const PRESSURE: &str = "com.publicdomainrelay.temp.compute.events.vm.memoryPressure";
const OOM_KILLED: &str = "com.publicdomainrelay.temp.compute.events.vm.oomKilled";

/// internal/market/bidder/memory.go: MemoryRearmQuiet 5 min, MemoryTypeGap 10 min, MemoryPoll 15 s.
const REARM_S: f64 = 300.0;
const GAP_S: f64 = 600.0;
const WINDOW_S: f64 = 60.0;

const HOLD_PY: &str = r#"import os, sys, time
n = int(sys.argv[1])
hold = [bytearray(os.urandom(64 << 20)) for _ in range(n // 64)]
open('/var/tmp/memhold.ready', 'w').write(str(len(hold) * 64))
while True:
    if os.path.exists('/var/tmp/memhold.go'):
        for c in hold:
            c[::4096]
    else:
        time.sleep(0.2)
"#;

fn ssh(id: &str, name: &str, exec: &str) -> Call {
    host(id, "request.ssh", json!({ "name": name, "exec": exec }))
}

fn vm_memory(id: &str, s: &S, set_high: Option<Value>) -> Call {
    let mut i = json!({ "receipt": s.st["receipt"], "cluster": s.st["cluster"], "vm": s.st["vm"] });
    if let Some(v) = set_high {
        i["setHigh"] = v;
    }
    host(id, "vm.memory", i)
}

fn list(id: &str, repo: &str, collection: &str) -> Call {
    Call::new(id, "com.atproto.repo.listRecords").params(json!({ "repo": repo, "collection": collection, "limit": 100 })).service(repo)
}

/// This receipt's records of `nsid` of `kinds` (rkey ends -payload-<key_hash("<nsid>:<kind>-<n>:<receipt>")>, n 1..10).
fn mine(o: &Value, nsid: &str, kinds: &[&str], receipt: &str) -> Vec<Value> {
    let tails: Vec<String> = kinds.iter().flat_map(|k| (1..=10).map(move |n| format!("{k}-{n}"))).map(|t| format!("-payload-{}", steps::key_hash(&format!("{nsid}:{t}:{receipt}")))).collect();
    o["records"].as_array().into_iter().flatten().filter(|r| r["uri"].as_str().is_some_and(|u| tails.iter().any(|t| u.ends_with(t.as_str())))).cloned().collect()
}

/// The crossing n starts: MemoryHigh to half the VM's usage (256 MiB at least), the guest touches its memory.
fn cross(s: &mut S, h: Handle, name: &str, n: u64) -> Handle {
    let cur = s.st["m"]["current"].as_u64().unwrap_or(0);
    let lowered = (cur / 2).max(256 << 20);
    s.st["x"] = json!({ "n": n, "lowered": lowered, "cur": cur, "highBefore": s.st["m"]["events"]["high"], "startS": s.now, "until": s.now + WINDOW_S });
    s.set("cross");
    s.mark("cross");
    h.call(vm_memory("mx", s, Some(json!(lowered)))).call(ssh("go", name, "touch /var/tmp/memhold.go; echo ok")).timer("tick", 5_000)
}

pub fn run(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let receipt = s.st["receipt"].as_str().unwrap_or_default().to_string();
    let winner = receipt.trim_start_matches("at://").split('/').next().unwrap_or_default().to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if args["target"] == "prod" {
                return Ok(s.skip("staging only: it lowers its own VM's MemoryHigh through the network bidder's user manager (fault injection; prod bidders are shared)"));
            }
            // a bidder publishes its offering once its pinned guest image verified: a network that just came up is minutes
            // ahead of it, so the rent waits for the first bidder's offering (10 s retries, 10 min at most)
            s.set("offering");
            h = h.timer("deadline", 40 * MIN);
            h = match bidders.first() {
                Some(b) => h.call(get_record("off", &format!("at://{}/com.publicdomainrelay.temp.market.offering/self", b["did"].as_str().unwrap_or_default()))),
                None => h.timer("offered", 1),
            };
        }
        Event::Timer { id } if id == "offretry" => {
            let b = bidders.first().cloned().unwrap_or_default();
            h = h.call(get_record("off", &format!("at://{}/com.publicdomainrelay.temp.market.offering/self", b["did"].as_str().unwrap_or_default())));
        }
        Event::Timer { id } if id == "offered" && s.is("offering") => {
            s.set("held");
            let a = json!(["--hold", "--cpus", "2", "--mem", "2G", "--expires-in", "1h"]);
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a })));
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 40 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "ready" => h = h.call(ssh("ready", &name, "cat /var/tmp/memhold.ready 2>/dev/null; echo; echo BOOT $(cat /proc/sys/kernel/random/boot_id)")),
        // the crossing's watch: memory.events each 5 s; at its end the limit is restored and the guest stops touching
        Event::Timer { id } if id == "tick" && s.is("cross") => {
            h = h.call(vm_memory("mt", &s, None)).timer("tick", 5_000);
        }
        Event::Timer { id } if id == "lst" && (s.is("after") || s.is("verify")) => {
            h = h.call(list("ls", &winner, PRESSURE)).timer("lst", 15_000);
        }
        Event::Timer { id } if id == "next" && s.is("idle") => {
            let n = s.st["x"]["n"].as_u64().unwrap_or(0) + 1;
            h = cross(&mut s, h, &name, n);
        }
        Event::Message { data, .. } => {
            if s.st["receipt"].is_null() && data["receipt"].is_string() {
                s.st["receipt"] = data["receipt"].clone();
            }
            match (data["msg"].as_str().unwrap_or_default(), data["phase"].as_str().unwrap_or_default()) {
                ("contract", "Held") => s.flag("held"),
                ("contract", p) if s.has("held") && !s.has("releasing") && !p.is_empty() => {
                    return Ok(s.fail(format!("the contract left Held: {p} {}", data["message"])));
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
                s.set("find");
                h = h.call(kube("vms", "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })));
            }
        }
        Event::Result { id, result, .. } => {
            let r = result.output();
            if id == "off" {
                if r.as_ref().is_err_and(|e| e.contains("RecordNotFound")) {
                    let n = s.st["offTries"].as_u64().unwrap_or(0) + 1;
                    s.st["offTries"] = json!(n);
                    if n > 60 {
                        return Ok(s.fail("the bidder's offering is not published after 10 min"));
                    }
                    return Ok(s.keep(h.timer("offretry", 10_000)));
                }
                if let Err(e) = r {
                    return Ok(s.fail(format!("off: {e}")));
                }
                s.set("held");
                let a = json!(["--hold", "--cpus", "2", "--mem", "2G", "--expires-in", "1h"]);
                return Ok(s.keep(h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a })))));
            }
            // the VM's memory cgroup is the dev network's: a bidder without VM cgroups (proc backend) skips the entry
            if (id == "m0" || id == "mx") && r.as_ref().is_err_and(|e| e.contains("no VM unit") || e.contains("no readable memory cgroup")) {
                let why = format!("the winner's VM has no memory cgroup under a user manager of this account: {}", r.err().unwrap_or_default());
                s.flag("releasing");
                s.set("release");
                s.reading("skipped", why);
                return Ok(s.keep(h.call(host("release", "request.release", json!({ "name": name })))));
            }
            // no vm.terminated for the receipt is the pass: its absence reads RecordNotFound
            let o = if id == "term" && r.as_ref().is_err_and(|e| e.contains("RecordNotFound")) {
                json!({ "none": true })
            } else {
                match out(&s, &id, r) {
                    Ok(o) => o,
                    Err(f) => return Ok(f),
                }
            };
            match id.as_str() {
                "vms" => {
                    let c: Vec<&Value> = o["items"].as_array().into_iter().flatten().filter(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]).collect();
                    let [v] = c.as_slice() else {
                        return Ok(s.fail(format!("{} ComputeVMs of this run's receipt on the winner", c.len())));
                    };
                    s.st["vm"] = v["metadata"]["name"].clone();
                    s.set("deploy");
                    let start = format!(
                        "cat > /var/tmp/memhold.py <<'PY_EOF'\n{HOLD_PY}PY_EOF\ntotal=$(awk '/^MemTotal:/{{print $2}}' /proc/meminfo); rm -f /var/tmp/memhold.go /var/tmp/memhold.ready\nsetsid nohup python3 /var/tmp/memhold.py $((total * 55 / 100 / 1024)) > /var/tmp/memhold.log 2>&1 < /dev/null & echo started"
                    );
                    h = h.call(ssh("deploy", &name, &start));
                }
                "deploy" => {
                    s.mark("deploy");
                    h = h.timer("ready", 5_000);
                }
                "ready" => {
                    let text = o["output"].as_str().unwrap_or_default();
                    let held: Option<u64> = text.lines().next().and_then(|l| l.trim().parse().ok());
                    let boot = text.lines().find_map(|l| l.strip_prefix("BOOT ")).map(|b| b.trim().to_string());
                    match (held, boot) {
                        (Some(m), Some(b)) => {
                            s.st["boot0"] = json!(b);
                            s.reading("guest_held_mib", m);
                            s.set("base");
                            h = h.call(vm_memory("m0", &s, None));
                        }
                        _ if s.since("deploy") > 180.0 => return Ok(s.fail(format!("the guest did not hold its memory in 3 min: {text}"))),
                        _ => h = h.timer("ready", 5_000),
                    }
                }
                "m0" => {
                    s.st["m"] = o.clone();
                    s.st["high0"] = o["high"].clone();
                    s.reading("limits", json!({ "unit": o["unit"], "high": o["high"], "max": o["max"], "swapMax": o["swapMax"], "currentBeforeMiB": o["current"].as_u64().unwrap_or(0) >> 20, "events": o["events"] }));
                    h = cross(&mut s, h, &name, 1);
                }
                "mx" => {
                    s.st["x"]["setHigh"] = o["high"].clone();
                }
                "mt" => {
                    s.st["m"] = o.clone();
                    if s.now >= s.st["x"]["until"].as_f64().unwrap_or(0.0) && s.is("cross") {
                        let restore = match s.st["high0"].as_str() {
                            Some("max") | None => json!("max"),
                            Some(v) => v.parse::<u64>().map(Value::from).unwrap_or(json!("max")),
                        };
                        s.set("restore");
                        h = h.call(vm_memory("mr", &s, Some(restore))).call(ssh("stop", &name, "rm -f /var/tmp/memhold.go; echo ok"));
                    }
                }
                "mr" => {
                    let n = s.st["x"]["n"].as_u64().unwrap_or(0);
                    let rose = o["events"]["high"].as_u64().unwrap_or(0).saturating_sub(s.st["x"]["highBefore"].as_u64().unwrap_or(0));
                    let x = json!({ "n": n, "lowered": s.st["x"]["lowered"], "currentMiB": s.st["x"]["cur"].as_u64().unwrap_or(0) >> 20, "highEventsRose": rose, "restoredHigh": o["high"], "oomKill": o["events"]["oom_kill"], "max": o["events"]["max"] });
                    s.st["xs"].as_array_mut().map(|a| a.push(x.clone())).unwrap_or_else(|| s.st["xs"] = json!([x.clone()]));
                    s.reading("crossings", s.st["xs"].clone());
                    if rose < 3 {
                        return Ok(s.fail(format!("crossing {n}: memory.events high rose only {rose} times in {WINDOW_S} s (limit lowered to {} B)", s.st["x"]["lowered"])));
                    }
                    s.st["m"] = o.clone();
                    s.st["x"]["endS"] = json!(s.now);
                    s.set("after");
                    s.mark("after");
                    h = h.timer("lst", 1_000);
                }
                "ls" => {
                    let n = s.st["x"]["n"].as_u64().unwrap_or(0);
                    let recs = mine(&o, PRESSURE, &["high"], &receipt);
                    let maxes = mine(&o, PRESSURE, &["max"], &receipt);
                    if !maxes.is_empty() {
                        return Ok(s.fail(format!("a kind max record: {}", json!(maxes))));
                    }
                    let want_total = if n == 3 { 2 } else { 1 };
                    s.st["recs"] = json!(recs.iter().map(|r| json!({ "uri": r["uri"], "kind": r["value"]["kind"], "threshold": r["value"]["threshold"], "observed": r["value"]["observed"], "observedAt": r["value"]["observedAt"] })).collect::<Vec<_>>());
                    if recs.len() > want_total {
                        return Ok(s.fail(format!("crossing {n}: {} memoryPressure records, exactly {want_total} expected: {}", recs.len(), s.st["recs"])));
                    }
                    // the crossing's records are in once the expected count appeared; then 45 s more must add none
                    if recs.len() == want_total && !s.has(&format!("seen{n}")) {
                        s.flag(&format!("seen{n}"));
                        s.mark(&format!("seen{n}"));
                        if n == 1 {
                            s.st["t1"] = json!(secs(recs[0]["value"]["observedAt"].as_str().unwrap_or_default()));
                        }
                    }
                    let settled = s.has(&format!("seen{n}")) && s.since(&format!("seen{n}")) >= 45.0;
                    // crossing 2 must add none: 75 s after its window
                    let quiet2 = n == 2 && recs.len() == 1 && s.since("after") >= 75.0;
                    if s.is("after") && (settled || quiet2) {
                        let ok1 = recs[0]["value"]["kind"] == "high" && recs[0]["value"]["observed"].as_i64().unwrap_or(0) > 0;
                        if !ok1 {
                            return Ok(s.fail(format!("the first record is not a high record with an observed size: {}", recs[0]["value"])));
                        }
                        let t1 = s.st["t1"].as_f64().unwrap_or(s.now);
                        match n {
                            1 => {
                                s.reading("record1", s.st["recs"][0].clone());
                                s.set("idle");
                                h = h.timer("next", ((t1 + REARM_S + 90.0 - s.now).max(1.0) * 1000.0) as u64);
                            }
                            2 => {
                                s.reading("afterGap2", json!({ "records": recs.len() }));
                                s.set("idle");
                                h = h.timer("next", ((t1 + GAP_S + 45.0 - s.now).max(1.0) * 1000.0) as u64);
                            }
                            _ => {
                                s.reading("records", s.st["recs"].clone());
                                s.set("verify");
                                s.st["need"] = json!(["vf", "boot1", "oom", "term"]);
                                h = h
                                    .call(kube("vf", "list", &s.st["cluster"].as_str().unwrap_or_default().to_string(), json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })))
                                    .call(ssh("boot1", &name, "echo BOOT $(cat /proc/sys/kernel/random/boot_id)"))
                                    .call(list("oom", &winner, OOM_KILLED))
                                    .call(get_record("term", &format!("at://{winner}/{VM_TERMINATED}/{}", termination_rkey(&receipt))));
                            }
                        }
                    } else if s.is("after") && s.since("after") > 240.0 && recs.len() < want_total {
                        return Ok(s.fail(format!("crossing {n}: {} memoryPressure records 4 min after the window, {want_total} expected: {} (the bidder polls VM cgroups only when its core runs under a systemd user manager, user@<uid>.service: a dev network started from a login session has its core in a session scope, and none are published; start `dev up` inside `systemd-run --user --scope`)", recs.len(), s.st["recs"])));
                    }
                }
                "vf" | "boot1" | "oom" | "term" => {
                    s.st["got"][id.as_str()] = if id == "term" { json!({ "terminated": o["none"] != true, "value": o["value"] }) } else { o.clone() };
                    let all = ["vf", "boot1", "oom", "term"].iter().all(|k| !s.st["got"][*k].is_null());
                    if all {
                        let g = s.st["got"].clone();
                        let mut p = vec![];
                        let v = g["vf"]["items"].as_array().into_iter().flatten().find(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]);
                        if v.is_none_or(|v| v["status"]["phase"] != "Running" || v["status"]["lostReason"].is_string()) {
                            p.push(format!("the ComputeVM is not Running: {}", v.map(|v| v["status"].to_string()).unwrap_or("gone".into())));
                        }
                        let b1 = g["boot1"]["output"].as_str().unwrap_or_default().lines().find_map(|l| l.strip_prefix("BOOT ")).map(|b| b.trim().to_string());
                        if b1.as_deref() != s.st["boot0"].as_str() {
                            p.push(format!("the guest rebooted: {} -> {:?}", s.st["boot0"], b1));
                        }
                        if g["term"]["terminated"] == true {
                            p.push(format!("vm.terminated: {}", g["term"]["value"]));
                        }
                        let ooms = mine(&g["oom"], OOM_KILLED, &["oom"], &receipt);
                        s.reading("oomKilledRecords", ooms.len());
                        if !ooms.is_empty() {
                            p.push(format!("vm.oomKilled records: {}", json!(ooms)));
                        }
                        if !p.is_empty() {
                            return Ok(s.fail(p.join("; ")));
                        }
                        s.flag("releasing");
                        s.set("release");
                        h = h.call(host("release", "request.release", json!({ "name": name })));
                    }
                }
                "release" => {
                    s.reading("release_s", o["seconds"].clone());
                    if s.has("skip") || s.st["readings"]["skipped"].is_string() {
                        return Ok(s.skip(s.st["readings"]["skipped"].as_str().unwrap_or_default()));
                    }
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
        Event::Closed { id, error } if id == "req" && !s.has("releasing") => return Ok(s.fail(format!("req closed: {}", error.unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}
