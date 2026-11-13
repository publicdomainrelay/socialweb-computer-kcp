// docs/acceptance/0008-host-observer.md: a VM's timeline on its host (Running there while Held), the release, the
// vm.terminated pinned by one getRecord at its computed key (deleted_on_request), the release's records gone, the
// orphan check (nothing of the receipt on the host). Without the winner's kcp the host readings are skipped.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, out, termination_rkey};

fn run_0008(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let cluster = s.st["cluster"].as_str().map(String::from);
    let vms = |id: &str, c: &str| kube(id, "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }));
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            h = h.call(host("req", "request.start", json!({ "name": name, "args": ["--hold", "--exec", "id"] })).subscribe()).timer("deadline", 30 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 30 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = h.call(get_record("term", &format!("at://{}/{VM_TERMINATED}/{}", s.st["winner"].as_str().unwrap_or_default(), termination_rkey(s.st["receipt"].as_str().unwrap_or_default()))));
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => s.flag("held"),
                "exited" => s.flag("exited"),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.has("exited") {
                s.set("summary");
                h = h.call(host("summary", "request.summary", json!({ "name": name })));
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
                "summary" => {
                    let w = o["contract"]["winnerDid"].clone();
                    s.st["winner"] = w.clone();
                    s.st["receipt"] = o["contract"]["receiptUri"].clone();
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
                    match cluster {
                        Some(c) => h = h.call(vms("empty", &c)),
                        None => return Ok(s.pass()),
                    }
                }
                "empty" => {
                    if o["items"].as_array().into_iter().flatten().any(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]) {
                        return Ok(s.fail("the host still runs a VM of the receipt"));
                    }
                    return Ok(s.pass());
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0008 }
