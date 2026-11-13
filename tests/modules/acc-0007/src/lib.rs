// docs/acceptance/0007-bidder-termination.md: fault injection on the winner's kcp, scoped to this run's own receipt:
// delete its ComputeVM; it goes within 2 min, its vm.terminated is pinned `deleted_on_request`, the requester deletes
// its contract records (the release's vm.delete too), the host runs nothing of the receipt. Needs the winner's kcp.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, out, sw, termination_rkey};

fn run_0007(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
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

entries! { run => run_0007 }
