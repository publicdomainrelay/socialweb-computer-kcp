// docs/acceptance/0006-rollout-compat.md. run: on each bidder kcp the host reaches, the MarketIdentity is Ready and
// signed in with no pending action, the Bidder runs bidder-tangled-vouch, the DID is the target's, and its host (bidder.health,
// no sudo) has 0 ERROR in the bidder's first 10 min, 0 automatic restarts, bidder_guest_image_ready since its start, no VM
// process without a Running ComputeVM. compat: the
// unchanged CLI (SWC_COMPAT_REQUESTER) rents and releases against the new servers: Held, `id` ran, Released, every
// record of the run gone (the release's vm.delete too: no release leftovers since rc165), none created in the repo during
// the run left (the release process's own records), the CLI's version a reading.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, kube, out, sw};

fn bidders(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let bs: Vec<Value> = args["bidders"].as_array().into_iter().flatten().filter(|b| b["cluster"].is_string()).cloned().collect();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if bs.is_empty() {
                return Ok(s.skip("needs bidder kcps in the target"));
            }
            s.st["fan"] = json!(bs.len() * 5);
            for (j, b) in bs.iter().enumerate() {
                let c = b["cluster"].as_str().unwrap_or_default();
                h = h
                    .call(kube(&format!("mi-{j}"), "get", c, sw("MarketIdentity", "bidder")))
                    .call(kube(&format!("bd-{j}"), "get", c, sw("Bidder", "bidder")))
                    .call(kube(&format!("mb-{j}"), "get", c, sw("MarketBidder", "app")))
                    .call(kube(&format!("vm-{j}"), "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })))
                    .call(host(&format!("hl-{j}"), "bidder.health", json!({ "name": b["name"] })));
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            let j: usize = id[3..].parse().unwrap_or(0);
            let name = bs[j]["name"].as_str().unwrap_or_default().to_string();
            match &id[..2] {
                "mi" => {
                    let signed = o["status"]["conditions"].as_array().into_iter().flatten().find(|c| c["type"] == "SignedIn").map(|c| c["status"].clone());
                    if o["status"]["phase"] != "Ready" || !o["status"]["pendingAction"].is_null() || signed.as_ref().is_some_and(|s| s != "True") {
                        return Ok(s.fail(format!("{name} MarketIdentity {}", o["status"])));
                    }
                }
                "bd" => {
                    let p = &o["spec"]["policy"];
                    if !(p.is_null() || p == "bidder-tangled-vouch") {
                        return Ok(s.fail(format!("{name} policy {p}")));
                    }
                    s.st["readings"][&name]["policy"] = p.clone();
                }
                "mb" => {
                    if o["status"]["did"] != bs[j]["did"] {
                        return Ok(s.fail(format!("{name} DID {} is not the target's", o["status"]["did"])));
                    }
                    s.st["readings"][&name]["phase"] = o["status"]["phase"].clone();
                }
                "vm" => {
                    let running = o["items"].as_array().into_iter().flatten().filter(|v| v["status"]["phase"] == "Running").count();
                    s.st["readings"][&name]["runningVMs"] = json!(running);
                }
                // the runbook's startup and restart checks, read from the host (no sudo): 0 ERROR in the bidder's first
                // 10 min, 0 automatic restarts since its last stop; later ERROR lines are anomalies, kept as readings
                _ => {
                    if o["error"].is_string() {
                        return Ok(s.fail(format!("{name} health: {}", o["error"])));
                    }
                    for k in ["started", "errors", "startupErrors", "restarts", "vmProcesses", "guestImageReadyAt", "guestImageUnready"] {
                        s.st["readings"][&name][k] = o[k].clone();
                    }
                    if o["errors"].as_u64().unwrap_or(0) > 0 {
                        s.st["readings"][&name]["errorSample"] = o["errorSample"].clone();
                    }
                    if o["startupErrors"] != 0 || o["restarts"] != 0 {
                        return Ok(s.fail(format!("{name}: {} startup ERROR lines, {} automatic restarts since {}", o["startupErrors"], o["restarts"], o["started"])));
                    }
                    // the bidder verified its pinned guest image since it started (until then it offers nothing)
                    if o["guestImageReadyAt"].as_str().unwrap_or_default().is_empty() {
                        return Ok(s.fail(format!("{name}: no bidder_guest_image_ready since it started {} ({} bidder_guest_image_unready)", o["started"], o["guestImageUnready"])));
                    }
                }
            }
            let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
            s.st["fan"] = json!(left);
            if left == 0 {
                // the host is empty but for live guests: no VM process without a Running ComputeVM
                for b in &bs {
                    let r = &s.st["readings"][b["name"].as_str().unwrap_or_default()];
                    if r["vmProcesses"].as_u64().unwrap_or(0) > r["runningVMs"].as_u64().unwrap_or(0) {
                        return Ok(s.fail(format!("{}: {} VM processes, {} Running ComputeVMs (orphans)", b["name"], r["vmProcesses"], r["runningVMs"])));
                    }
                }
                return Ok(s.pass());
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

fn compat_handler(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            s.st["since"] = args["now"].clone();
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "compat": true, "args": ["--hold", "--exec", "id"] }))).timer("deadline", 20 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 20 min (phase {})", s.phase()))),
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Failed" && !s.has("held") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                "exited" if !s.has("held") => return Ok(s.fail("run request exited before the VM was Held")),
                "contract" if data["phase"] == "Held" && !s.has("held") => {
                    s.flag("held");
                    let t = s.t();
                    s.reading("held_s", t);
                }
                "ssh returned" => {
                    if data["exit"] != 0 {
                        return Ok(s.fail(format!("id over ssh: {data}")));
                    }
                    s.flag("ssh");
                }
                "exited" => s.flag("exited"),
                "requester_binary" => s.reading("cli", data["version"].clone()),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.has("ssh") && s.has("exited") {
                s.set("release");
                h = h.cancel("req").call(host("release", "request.release", json!({ "name": name })));
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "release" => {
                    s.reading("release_s", o["seconds"].clone());
                    h = h.call(host("logs", "request.checkLogs", json!({ "name": name })));
                }
                "logs" => {
                    if o["ok"] != true {
                        return Ok(s.fail(format!("logs: {}", o["problems"])));
                    }
                    h = h.call(host("records", "request.records", json!({ "name": name })));
                }
                "records" => {
                    s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left after the release: {} {}", o["left"], o["errors"])));
                    }
                    // the logs name only the run's records: the release process's own (market.event, vm.delete) show in a
                    // scan of the repo since the run started
                    h = h.call(host("since", "assert.noneSince", json!({ "did": args["did"], "since": s.st["since"] })));
                }
                "since" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) || o["errors"].as_array().is_some_and(|e| !e.is_empty()) {
                        return Ok(s.fail(format!("records created during the run left: {} {}", o["left"], o["errors"])));
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

entries! { run => bidders, compat => compat_handler }
