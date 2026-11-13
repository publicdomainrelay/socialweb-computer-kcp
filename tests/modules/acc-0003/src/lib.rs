// docs/acceptance/0003-cli-webrtc-ssh.md (and 0001 section 9, ACC34): `run request --hold` with ssh over WebRTC,
// fresh and from a restarted invocation kcp (<= 10 s since rc165's fix), no plaintext signaling or password in any
// log, Released, every record of the run gone (the release's vm.delete too), the winner's host empty. Discovery; with
// --per-host and every bidder's kcp, each host in turn by pausing the others (restored after).
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, kube, out, sw};

fn bidder_patch(id: &str, cluster: &str, suspend: bool) -> abi::Call {
    let mut q = sw("MarketBidder", "app");
    q["patch"] = json!({ "spec": { "suspend": suspend } });
    kube(id, "patch", cluster, q)
}

fn run_0003(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let base = args["name"].as_str().ok_or("name")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let per_host = args["perHost"] == true && !bidders.is_empty() && bidders.iter().all(|b| b["cluster"].is_string());
    let rounds = if per_host { bidders.len() } else { 1 };
    let i = s.st["i"].as_u64().unwrap_or(0) as usize;
    let name = format!("{base}-{i}");
    let mut h = Handle::new();
    // pause every bidder but round i's (per host), or nothing
    let arrange = |h: Handle, i: Option<usize>| bidders.iter().enumerate().fold(h, |h, (j, b)| h.call(bidder_patch(&format!("pause-{j}"), b["cluster"].as_str().unwrap_or_default(), i.is_some_and(|i| i != j))));
    let start = |s: &mut S, h: Handle| {
        s.set("held");
        s.st["f"] = json!({});
        let a = json!(["--hold", "--tunnel", "ssh:22:webrtc", "--transport", "webrtc", "--exec", "uname -a; id"]);
        h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a })))
    };
    match run.event {
        Event::Start => {
            h = h.timer("deadline", (25 * rounds as u64) * MIN);
            if per_host {
                s.set("arrange");
                s.st["fan"] = json!(bidders.len());
                h = arrange(h, Some(0));
            } else {
                h = start(&mut s, h);
            }
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done (phase {}, round {i})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            for (j, b) in bidders.iter().enumerate() {
                h = h.call(kube(&format!("phase-{j}"), "get", b["cluster"].as_str().unwrap_or_default(), sw("MarketBidder", "app")));
            }
            s.st["fan"] = json!(bidders.len());
            s.st["ok"] = json!(true);
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Failed" && !s.has("held") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                "exited" if !s.has("held") => return Ok(s.fail("run request exited before the VM was Held")),
                "contract" if data["phase"] == "Held" && !s.has("held") => {
                    s.flag("held");
                    s.mark("held");
                }
                "ssh returned" if !s.has("ssh") => {
                    if data["transport"] != "webrtc" || data["exit"] != 0 {
                        return Ok(s.fail(format!("the first ssh: {data}")));
                    }
                    s.flag("ssh");
                    s.st["fresh"] = data["seconds"].clone();
                }
                "exited" => s.flag("exited"),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.has("ssh") && s.has("exited") {
                s.set("restarted");
                s.mark("ssh2");
                h = h.cancel("req").call(host("ssh2", "request.ssh", json!({ "name": name, "transport": "webrtc", "exec": "uname -a; id" })));
            }
        }
        Event::Result { id, result, .. } => {
            if id.starts_with("pause-") || id.starts_with("phase-") {
                let o = match out(&s, &id, result.output()) {
                    Ok(o) => o,
                    Err(f) => return Ok(f),
                };
                if id.starts_with("phase-") {
                    let j: usize = id[6..].parse().unwrap_or(0);
                    let want = if s.is("resume") || j == i { "Ready" } else { "Suspended" };
                    if o["status"]["phase"] != want {
                        s.st["ok"] = json!(false);
                    }
                }
                let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                s.st["fan"] = json!(left);
                if left == 0 {
                    if id.starts_with("pause-") {
                        s.mark("arrange");
                        return Ok(s.keep(h.timer("poll", 5_000)));
                    }
                    if s.st["ok"] != true {
                        if s.since("arrange") > 300.0 {
                            return Ok(s.fail("the bidders did not reach their phases in 5 min"));
                        }
                        return Ok(s.keep(h.timer("poll", 5_000)));
                    }
                    if s.is("resume") {
                        return Ok(s.pass());
                    }
                    h = start(&mut s, h);
                }
                return Ok(s.keep(h));
            }
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "ssh2" => {
                    let wall = s.since("ssh2");
                    let txt = o["output"].as_str().unwrap_or_default();
                    if o["ok"] != true || !txt.contains("Linux") || !txt.contains("uid=") {
                        return Ok(s.fail(format!("restarted ssh: {txt}")));
                    }
                    // the runbook's number is the CLI's own `ssh returned` seconds (the wall time adds the kcp's restart
                    // from its state dir and its stop)
                    let t = txt
                        .lines()
                        .filter_map(|l| serde_json::from_str::<Value>(l).ok())
                        .find(|l| l["msg"] == "ssh returned")
                        .and_then(|l| l["seconds"].as_f64())
                        .map_or(wall, |x| (x * 10.0).round() / 10.0);
                    let max = args["restartSshMaxS"].as_f64().unwrap_or(10.0);
                    if t > max {
                        return Ok(s.fail(format!("the restarted invocation's ssh took {t} s (want <= {max} s)")));
                    }
                    s.st["restarted"] = json!(t);
                    s.st["restarted_wall"] = json!(wall);
                    h = h.call(host("summary", "request.summary", json!({ "name": name })));
                }
                "summary" => {
                    let w = o["contract"]["winnerDid"].clone();
                    let Some(b) = bidders.iter().find(|b| b["did"] == w) else { return Ok(s.fail(format!("winner {w} is not a target bidder"))) };
                    if per_host && bidders[i]["did"] != w {
                        return Ok(s.fail(format!("{} paused all but itself, the winner is {w}", bidders[i]["name"])));
                    }
                    s.st["winner"] = json!({ "did": w, "name": b["name"], "cluster": b["cluster"] });
                    s.st["receipt"] = o["contract"]["receiptUri"].clone();
                    h = h.call(host("release", "request.release", json!({ "name": name })));
                }
                "release" => {
                    s.st["release_s"] = o["seconds"].clone();
                    h = h.call(host("logs", "request.checkLogs", json!({ "name": name })));
                }
                "logs" => {
                    if o["ok"] != true {
                        return Ok(s.fail(format!("logs: {}", o["problems"])));
                    }
                    h = h.call(host("records", "request.records", json!({ "name": name })));
                }
                "records" => {
                    s.st["records_checked"] = json!(o["uris"].as_array().map_or(0, Vec::len));
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left after the release: {} {}", o["left"], o["errors"])));
                    }
                    match s.st["winner"]["cluster"].as_str() {
                        Some(c) => h = h.call(kube("vms", "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }))),
                        None => { let h = next(&mut s, h, i, rounds, &bidders, per_host); return Ok(s.keep(h)); },
                    }
                }
                "vms" => {
                    if o["items"].as_array().into_iter().flatten().any(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]) {
                        return Ok(s.fail("the winner's host still runs this run's VM"));
                    }
                    { let h = next(&mut s, h, i, rounds, &bidders, per_host); return Ok(s.keep(h)); };
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

/// Record the round, then the next one (re-arranged), or resume every bidder / pass.
fn next(s: &mut S, mut h: Handle, i: usize, rounds: usize, bidders: &[Value], per_host: bool) -> Handle {
    let r = json!({ "winner": s.st["winner"]["name"], "fresh_s": s.st["fresh"], "restarted_s": s.st["restarted"], "restarted_wall_s": s.st["restarted_wall"], "release_s": s.st["release_s"], "records_checked": s.st["records_checked"] });
    let key = if per_host { bidders[i]["name"].as_str().unwrap_or("?").to_string() } else { "discovery".into() };
    s.st["readings"][key] = r;
    s.st["i"] = json!(i + 1);
    if i + 1 < rounds {
        s.set("arrange");
        s.st["fan"] = json!(bidders.len());
        for (j, b) in bidders.iter().enumerate() {
            h = h.call(bidder_patch(&format!("pause-{j}"), b["cluster"].as_str().unwrap_or_default(), j != i + 1));
        }
        return h;
    }
    if per_host {
        s.set("resume");
        s.st["fan"] = json!(bidders.len());
        for (j, b) in bidders.iter().enumerate() {
            h = h.call(bidder_patch(&format!("pause-{j}"), b["cluster"].as_str().unwrap_or_default(), false));
        }
        return h;
    }
    let t = s.t();
    s.reading("seconds", t);
    abi::Handle::new().result(json!({ "pass": true, "readings": s.st["readings"] })).done()
}

entries! { run => run_0003 }
