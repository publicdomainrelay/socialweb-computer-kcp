// docs/acceptance/0006-rollout-compat.md. run: on each bidder kcp the host reaches, the MarketIdentity is Ready and
// signed in with no pending action, the Bidder runs bidder-tangled-vouch, the DID is the target's. compat: the
// unchanged CLI (SWC_COMPAT_REQUESTER) rents and releases against the new servers: Held, `id` ran, Released, every
// record of the run gone (the release's vm.delete too: no release leftovers since rc165).
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, kube, out, sw};

fn bidders(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let bs: Vec<Value> = args["bidders"].as_array().into_iter().flatten().filter(|b| b["cluster"].is_string()).cloned().collect();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if bs.is_empty() {
                return Ok(s.skip("needs bidder kcps in the target"));
            }
            s.st["fan"] = json!(bs.len() * 3);
            for (j, b) in bs.iter().enumerate() {
                let c = b["cluster"].as_str().unwrap_or_default();
                h = h.call(kube(&format!("mi-{j}"), "get", c, sw("MarketIdentity", "bidder"))).call(kube(&format!("bd-{j}"), "get", c, sw("Bidder", "bidder"))).call(kube(&format!("mb-{j}"), "get", c, sw("MarketBidder", "app")));
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
                _ => {
                    if o["status"]["did"] != bs[j]["did"] {
                        return Ok(s.fail(format!("{name} DID {} is not the target's", o["status"]["did"])));
                    }
                    s.st["readings"][&name]["phase"] = o["status"]["phase"].clone();
                }
            }
            let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
            s.st["fan"] = json!(left);
            if left == 0 {
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
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "compat": true, "args": ["--hold", "--exec", "id"] }))).timer("deadline", 20 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 20 min (phase {})", s.phase()))),
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
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
