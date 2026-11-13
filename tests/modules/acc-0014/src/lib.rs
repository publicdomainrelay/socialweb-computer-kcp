// docs/acceptance/0014-accept-fallback.md: rentals that outnumber the bidders' room, started together, on bidders this
// run sizes from their offerings (maxCpus, less the live guests on their kcp). A bidder whose committed capacity the
// accept would exceed answers 409 CapacityExceeded; the requester deletes that accept, denies the bidder and takes the
// next bid on the same RFP. run: 3 bidders, one VM each, 3 rentals (a refusal required; up to 3 rounds). concurrent:
// 2 bidders, two VMs each, 4 rentals. exhausted: 2 bidders, one VM each, 3 rentals: the third is refused by both, its
// bids run out, its new RFP (the refusers excluded) has no bidder left: Failed at once (requester_no_bidders). All through the kcp CLI requester (`run request`); the atlibs requester
// module's fallback is tests/compositions/knotless entry `fallback`. Both: every rental Held, never on a bidder that refused it, no bidder over its
// vCPUs, the refused accepts gone, then all released and every record gone. Staging only (it fills bidders).
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, get_record, host, kube, out};

const OFFERING: &str = "com.publicdomainrelay.temp.market.offering";
const LIVE: [&str; 5] = ["", "Pending", "Preparing", "Booting", "Running"];

struct Shape {
    bidders: usize,
    rentals: usize,
    fit: i64,
    need_refusal: bool,
    rounds: u64,
    /// rentals beyond the bidders' room: each refused by every bidder, then its new RFP refused at once
    fails: usize,
}

fn vms(id: &str, cluster: &str) -> abi::Call {
    kube(id, "list", cluster, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }))
}

fn live_vms(list: &Value) -> Vec<Value> {
    list["items"]
        .as_array()
        .into_iter()
        .flatten()
        .filter(|v| v["metadata"]["deletionTimestamp"].is_null() && LIVE.contains(&v["status"]["phase"].as_str().unwrap_or_default()))
        .cloned()
        .collect()
}

fn cpus_of(v: &Value) -> i64 {
    v["spec"]["vm"]["cpus"].as_i64().unwrap_or(0)
}

fn did_of(uri: &str) -> String {
    uri.trim_start_matches("at://").split('/').next().unwrap_or_default().to_string()
}

/// One more answer of the step's fan-out; true when it was the last.
fn landed(s: &mut S) -> bool {
    let left = s.st["wait"].as_u64().unwrap_or(1).saturating_sub(1);
    s.st["wait"] = json!(left);
    left == 0
}

fn chosen(s: &S) -> Vec<Value> {
    s.st["chosen"].as_array().cloned().unwrap_or_default()
}

fn rentals(s: &S) -> Vec<Value> {
    s.st["rentals"].as_array().cloned().unwrap_or_default()
}

/// The rentals and this round's fillers: what the release ends and the record check reads.
fn everyone(s: &S) -> Vec<Value> {
    let mut all = rentals(s);
    all.extend(s.st["fillers"].as_array().cloned().unwrap_or_default());
    all
}

/// A round: the fillers first (each pinned to its bidder, leaving it room for `fit` guests of `cpus`), then the rentals.
fn start_round(s: &mut S, mut h: Handle, name: &str, shape: &Shape) -> Handle {
    let plan = s.st["fill"].as_object().cloned().unwrap_or_default();
    if plan.is_empty() {
        s.st["fillers"] = json!([]);
        return launch(s, h, name, shape);
    }
    let round = s.st["round"].as_u64().unwrap_or(0) + 1;
    s.set("fillers");
    s.mark("fillers");
    let mut fs = vec![];
    for (j, (did, cpus)) in plan.iter().enumerate() {
        let n = format!("{name}-{round}-f{j}");
        let a = json!(["--hold", "--cpus", cpus.to_string(), "--mem", s.st["mem"], "--expires-in", "1h"]);
        h = h.call(steps::host_sub(&format!("fill:{j}"), "request.start", json!({ "name": n, "args": a, "bidderDids": [did] })));
        fs.push(json!({ "name": n, "bidder": did, "cpus": cpus }));
    }
    s.st["fillers"] = json!(fs);
    h.timer(&format!("fillers:{round}"), 15 * MIN)
}

fn launch(s: &mut S, mut h: Handle, name: &str, shape: &Shape) -> Handle {
    let round = s.st["round"].as_u64().unwrap_or(0) + 1;
    s.st["round"] = json!(round);
    s.set("held");
    s.mark("round");
    let dids: Vec<Value> = chosen(s).iter().map(|b| b["did"].clone()).collect();
    let mut rs = vec![];
    for i in 0..shape.rentals {
        let n = format!("{name}-{round}-{i}");
        let a = json!(["--hold", "--cpus", s.st["cpus"].to_string(), "--mem", s.st["mem"], "--expires-in", "1h"]);
        h = h.call(steps::host_sub(&format!("req:{i}"), "request.start", json!({ "name": n, "args": a, "bidderDids": dids })));
        rs.push(json!({ "name": n, "declined": [], "replaced": 0, "newRfp": 0 }));
    }
    s.st["rentals"] = json!(rs);
    h.timer(&format!("round:{round}"), 20 * MIN)
}

/// Every rental Held: where each landed, against what refused it and each bidder's room.
fn check(s: &mut S, mut h: Handle, shape: &Shape) -> Result<Handle, Handle> {
    let free = s.st["free"].clone();
    let mut per: serde_json::Map<String, Value> = Default::default();
    let mut p = vec![];
    let mut declined_uris = vec![];
    let mut failed = vec![];
    for r in rentals(s) {
        if r["failed"] == true {
            for d in r["declined"].as_array().into_iter().flatten() {
                declined_uris.push(d["accept"].clone());
            }
            let n = r["declined"].as_array().map_or(0, Vec::len);
            // the documented end: every bidder refused it, the bids ran out, its new RFP has no bidder left
            if n < chosen(s).len() || r["newRfp"].as_u64().unwrap_or(0) == 0 || !r["failMsg"].as_str().unwrap_or_default().contains("requester_no_bidders") {
                p.push(format!("{} failed, not after every bidder refused it and its new RFP found none: {} refusals, newRfp {}, {}", r["name"], n, r["newRfp"], r["failMsg"]));
            }
            failed.push(r["name"].clone());
            continue;
        }
        let w = did_of(r["receipt"].as_str().unwrap_or_default());
        if !chosen(s).iter().any(|b| b["did"] == w.as_str()) {
            p.push(format!("{} Held on {w}, not one of the run's bidders", r["name"]));
        }
        for d in r["declined"].as_array().into_iter().flatten() {
            if d["bidder"] == w.as_str() {
                p.push(format!("{} Held on {w}, which refused it", r["name"]));
            }
            if d["status"] != 409 || d["body"]["error"] != "CapacityExceeded" {
                p.push(format!("{} declined {} {}: not a 409 CapacityExceeded", r["name"], d["status"], d["body"]));
            }
            declined_uris.push(d["accept"].clone());
        }
        let c = per.get(&w).and_then(Value::as_i64).unwrap_or(0) + s.st["cpus"].as_i64().unwrap_or(0);
        per.insert(w, json!(c));
    }
    for (w, c) in &per {
        if c.as_i64().unwrap_or(0) > free[w].as_i64().unwrap_or(0) {
            p.push(format!("bidder {w} holds {c} vCPUs of this run, its room was {}", free[w]));
        }
    }
    if failed.len() != shape.fails {
        p.push(format!("{} rentals failed ({}), the bidders' room leaves {} without a VM", failed.len(), json!(failed), shape.fails));
    }
    if !p.is_empty() {
        return Err(s.fail(p.join("; ")));
    }
    let round = s.st["round"].clone();
    let rs = rentals(s);
    let refusals: usize = rs.iter().map(|r| r["declined"].as_array().map_or(0, Vec::len)).sum();
    let summary = json!({
        "round": round, "heldS": s.since("round"), "perBidder": per, "refusals": refusals,
        "failed": failed,
        "maxRefusalsOneRental": rs.iter().map(|r| r["declined"].as_array().map_or(0, Vec::len)).max().unwrap_or(0),
        "rentals": rs.iter().map(|r| json!({ "winner": did_of(r["receipt"].as_str().unwrap_or_default()), "failed": r["failed"],
            "refusedBy": r["declined"].as_array().into_iter().flatten().map(|d| d["bidder"].clone()).collect::<Vec<_>>(),
            "replaced": r["replaced"], "newRfp": r["newRfp"] })).collect::<Vec<_>>(),
    });
    // serde_json's index panics past an array's end: rounds grow by push
    let k = round.as_u64().unwrap_or(1) as usize - 1;
    if !s.st["readings"]["rounds"].is_array() {
        s.st["readings"]["rounds"] = json!([]);
    }
    let rounds = s.st["readings"]["rounds"].as_array_mut().unwrap();
    rounds.truncate(k);
    rounds.push(summary);
    s.set("verify");
    let mut wait = 0;
    if !declined_uris.is_empty() {
        h = h.call(host("gone:declined", "assert.recordsGone", json!({ "uris": declined_uris })));
        wait += 1;
    }
    for (j, b) in chosen(s).iter().enumerate() {
        if let Some(c) = b["cluster"].as_str() {
            h = h.call(vms(&format!("vms:{j}"), c));
            wait += 1;
        }
    }
    if wait == 0 {
        return Ok(release(s, h));
    }
    s.st["wait"] = json!(wait);
    Ok(h)
}

fn release(s: &mut S, mut h: Handle) -> Handle {
    s.set("release");
    s.mark("release");
    let rs = everyone(s);
    s.st["wait"] = json!(rs.len());
    for (i, r) in rs.iter().enumerate() {
        h = h.call(host(&format!("rel:{i}"), "request.release", json!({ "name": r["name"] })));
    }
    h
}

fn after_release(s: &mut S, h: Handle, name: &str, shape: &Shape) -> Handle {
    let rounds: Vec<Value> = s.st["readings"]["rounds"].as_array().cloned().unwrap_or_default();
    let refusals: u64 = rounds.iter().map(|r| r["refusals"].as_u64().unwrap_or(0)).sum();
    s.reading("refusals", refusals);
    if shape.need_refusal && refusals == 0 {
        if s.st["round"].as_u64().unwrap_or(0) < shape.rounds {
            return start_round(s, h, name, shape);
        }
        return s.fail(format!("no accept refused in {} rounds: every rental's first pick had room (a race; run again)", shape.rounds));
    }
    s.pass()
}

fn drive(run: Run<Value>, shape: Shape) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if args["target"] == "prod" {
                return Ok(s.skip("staging only: it fills bidders to their committed capacity (prod bidders are shared)"));
            }
            let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
            if bidders.len() < shape.bidders {
                return Ok(s.skip(&format!("needs {} bidders; the target has {}", shape.bidders, bidders.len())));
            }
            let picked: Vec<Value> = bidders.into_iter().take(shape.bidders).collect();
            s.st["chosen"] = json!(picked);
            s.st["mem"] = json!(args["mem"].as_str().unwrap_or("1G"));
            s.st["wait"] = json!(shape.bidders);
            s.set("offerings");
            for (j, b) in picked.iter().enumerate() {
                h = h.call(get_record(&format!("off:{j}"), &format!("at://{}/{OFFERING}/self", b["did"].as_str().unwrap_or_default())));
            }
            h = h.timer("deadline", (25 * shape.rounds + 15) * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in time (phase {})", s.phase()))),
        Event::Timer { id } if id.starts_with("round:") => {
            if (s.is("held") || s.is("settle")) && id == format!("round:{}", s.st["round"]) {
                return Ok(s.fail(format!("not every rental Held in 20 min: {}", s.st["rentals"])));
            }
        }
        // each bidder's room: its offering's maxCpus less its live guests; sized so `fit` VMs fit the tightest
        Event::Timer { id } if id == "sized" => {
            let mut free = json!({});
            for b in chosen(&s) {
                let d = b["did"].as_str().unwrap_or_default();
                free[d] = json!(s.st["max"][d].as_i64().unwrap_or(0) - s.st["used"][d].as_i64().unwrap_or(0));
            }
            let tight = free.as_object().into_iter().flatten().filter_map(|(_, v)| v.as_i64()).min().unwrap_or(0);
            let mut cpus = if shape.fit == 1 { tight / 2 + 1 } else { tight / shape.fit };
            // big bidders (a dev network's: the whole host's CPUs) get a filler each, so the contending guests stay
            // small (input `cpus`, default 2 over 8 free): many large guests booting at once miss the 2 min on-network
            let small = args["cpus"].as_i64().unwrap_or(if tight > 8 { 2 } else { 0 });
            if small > 0 && small < cpus {
                let mut plan = json!({});
                for (d, f) in free.as_object().cloned().unwrap_or_default() {
                    let filler = f.as_i64().unwrap_or(0) - shape.fit * small;
                    if filler > 0 {
                        plan[&d] = json!(filler);
                    }
                    free[&d] = json!(shape.fit * small);
                }
                s.reading("fillers", plan.clone());
                s.st["fill"] = plan;
                cpus = small;
            }
            let room: i64 = free.as_object().into_iter().flatten().map(|(_, v)| v.as_i64().unwrap_or(0) / cpus.max(1)).sum();
            s.reading("free", free.clone());
            s.reading("cpus", cpus);
            s.reading("mem", s.st["mem"].clone());
            s.st["free"] = free;
            s.st["cpus"] = json!(cpus);
            if cpus < 1 || room < (shape.rentals - shape.fails) as i64 {
                return Ok(s.fail(format!("the bidders have room for {room} VMs of {cpus} vCPUs, the run needs {}: {}", shape.rentals, s.st["free"])));
            }
            h = start_round(&mut s, h, &name, &shape);
        }
        Event::Timer { id } if id.starts_with("fillers:") => {
            if s.is("fillers") && id == format!("fillers:{}", s.st["round"].as_u64().unwrap_or(0) + 1) {
                return Ok(s.fail(format!("not every filler Held in 15 min: {}", s.st["fillers"])));
            }
        }
        Event::Message { id, data, .. } if id.starts_with("fill:") => {
            let j: usize = id[5..].parse().unwrap_or(0);
            let f = &mut s.st["fillers"][j];
            if f["receipt"].is_null() && data["receipt"].is_string() {
                f["receipt"] = data["receipt"].clone();
            }
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => f["held"] = json!(true),
                "contract" if data["phase"] == "Failed" && f["held"] != true => {
                    return Ok(s.fail(format!("filler {} failed before Held: {}", s.st["fillers"][j]["name"], data["message"])));
                }
                "exited" if f["held"] != true => return Ok(s.fail(format!("filler {} exited before Held: {data}", s.st["fillers"][j]["name"]))),
                _ => {}
            }
            let fs = s.st["fillers"].as_array().cloned().unwrap_or_default();
            if s.is("fillers") && fs.iter().all(|f| f["held"] == true && f["receipt"].is_string()) {
                // each filler on its own bidder (it was pinned there)
                if let Some(f) = fs.iter().find(|f| did_of(f["receipt"].as_str().unwrap_or_default()) != f["bidder"].as_str().unwrap_or_default()) {
                    return Ok(s.fail(format!("filler {} Held off its bidder: {}", f["name"], f["receipt"])));
                }
                s.reading("fillersHeldS", s.since("fillers"));
                h = launch(&mut s, h, &name, &shape);
            }
        }
        Event::Timer { id } if id == "settle" => {
            h = match check(&mut s, h, &shape) {
                Ok(h) => h,
                Err(f) => return Ok(f),
            };
        }
        Event::Timer { id } if id == "poll" => {
            s.st["wait"] = json!(0);
            for (j, b) in chosen(&s).iter().enumerate() {
                if let Some(c) = b["cluster"].as_str() {
                    h = h.call(vms(&format!("post:{j}"), c));
                    s.st["wait"] = json!(s.st["wait"].as_u64().unwrap_or(0) + 1);
                }
            }
            s.st["left"] = json!([]);
        }
        Event::Message { id, data, .. } if id.starts_with("req:") => {
            let i: usize = id[4..].parse().unwrap_or(0);
            let r = &mut s.st["rentals"][i];
            // the receipt: from the requester's own lines (requester_*, the core log); a refused accept has none
            if r["receipt"].is_null() && data["receipt"].is_string() {
                r["receipt"] = data["receipt"].clone();
            }
            match data["msg"].as_str().unwrap_or_default() {
                "requester_submit_accept_declined" => {
                    let d = json!({ "bidder": data["bidder"], "accept": data["accept"], "status": data["status"], "body": data["body"] });
                    r["declined"].as_array_mut().map(|a| a.push(d));
                }
                "requester_accept_declined_replace" => r["replaced"] = json!(r["replaced"].as_u64().unwrap_or(0) + 1),
                "requester_bids_exhausted_new_rfp" => r["newRfp"] = json!(r["newRfp"].as_u64().unwrap_or(0) + 1),
                "contract" if data["phase"] == "Held" => {
                    r["held"] = json!(true);
                    if r["receipt"].is_null() {
                        r["receipt"] = data["receipt"].clone();
                    }
                }
                "contract" if data["phase"] == "Failed" && r["held"] != true && shape.fails > 0 => {
                    r["failed"] = json!(true);
                    r["failMsg"] = data["message"].clone();
                }
                "contract" if data["phase"] == "Failed" && r["held"] != true => {
                    return Ok(s.fail(format!("{} failed before Held: {}", s.st["rentals"][i]["name"], data["message"])));
                }
                "exited" if r["held"] != true && r["failed"] != true => return Ok(s.fail(format!("{} exited before Held: {}", s.st["rentals"][i]["name"], data))),
                _ => {}
            }
            // every rental settled: the request log (Held, Failed) and the core log (requester_*) are two files, so
            // their lines settle 5 s before the check
            if s.is("held") && rentals(&s).iter().all(|r| (r["held"] == true && r["receipt"].is_string()) || r["failed"] == true) {
                s.set("settle");
                h = h.timer("settle", 5_000);
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            let j: usize = id.split(':').nth(1).and_then(|x| x.parse().ok()).unwrap_or(0);
            let b = chosen(&s).get(j).cloned().unwrap_or_default();
            let did = b["did"].as_str().unwrap_or_default().to_string();
            match id.split(':').next().unwrap_or_default() {
                "off" => {
                    let Some(m) = o["value"]["maxCpus"].as_i64() else {
                        return Ok(s.fail(format!("bidder {did}'s offering has no maxCpus: {}", o["value"])));
                    };
                    s.st["max"][&did] = json!(m);
                    if landed(&mut s) {
                        s.set("pre");
                        let with: Vec<(usize, String)> = chosen(&s).iter().enumerate().filter_map(|(j, b)| b["cluster"].as_str().map(|c| (j, c.to_string()))).collect();
                        s.st["wait"] = json!(with.len());
                        for (j, c) in &with {
                            h = h.call(vms(&format!("pre:{j}"), c));
                        }
                        if with.is_empty() {
                            s.reading("liveGuestsBefore", "unknown: the target names no bidder kcp");
                            s.st["wait"] = json!(1);
                            return Ok(s.keep(h.timer("sized", 1)));
                        }
                    }
                }
                "pre" => {
                    let used: i64 = live_vms(&o).iter().map(cpus_of).sum();
                    s.st["used"][&did] = json!(used);
                    if landed(&mut s) {
                        return Ok(s.keep(h.timer("sized", 1)));
                    }
                }
                "vms" => {
                    let live: i64 = live_vms(&o).iter().map(cpus_of).sum();
                    let max = s.st["max"][&did].as_i64().unwrap_or(0);
                    let k = s.st["round"].as_u64().unwrap_or(1) as usize - 1;
                    s.st["readings"]["rounds"][k]["liveCpus"][&did] = json!(live);
                    if live > max {
                        return Ok(s.fail(format!("bidder {did} runs {live} guest vCPUs of its {max}: overcommitted")));
                    }
                    if landed(&mut s) {
                        h = release(&mut s, h);
                    }
                }
                "gone" if id == "gone:declined" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("refused accepts not deleted: {} {}", o["left"], o["errors"])));
                    }
                    if landed(&mut s) {
                        h = release(&mut s, h);
                    }
                }
                "rel" => {
                    if landed(&mut s) {
                        s.reading("releaseS", s.since("release"));
                        let rs = everyone(&s);
                        s.st["wait"] = json!(rs.len());
                        s.st["uris"] = json!([]);
                        for (i, r) in rs.iter().enumerate() {
                            h = h.call(host(&format!("rec:{i}"), "request.records", json!({ "name": r["name"] })));
                        }
                    }
                }
                "rec" => {
                    for u in o["uris"].as_array().into_iter().flatten() {
                        s.st["uris"].as_array_mut().map(|a| a.push(u.clone()));
                    }
                    if landed(&mut s) {
                        s.reading("recordsChecked", s.st["uris"].as_array().map_or(0, Vec::len));
                        h = h.call(host("gone:records", "assert.recordsGone", json!({ "uris": s.st["uris"] })));
                    }
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left after the release: {} {}", o["left"], o["errors"])));
                    }
                    if chosen(&s).iter().all(|b| b["cluster"].is_null()) {
                        let h = after_release(&mut s, h, &name, &shape);
                        return Ok(s.keep(h));
                    }
                    s.set("orphans");
                    s.mark("orphans");
                    h = h.timer("poll", 1);
                }
                "post" => {
                    let ours: Vec<String> = everyone(&s).iter().filter_map(|r| r["receipt"].as_str().map(String::from)).collect();
                    for v in o["items"].as_array().into_iter().flatten() {
                        if v["spec"]["contract"]["receipt"]["uri"].as_str().is_some_and(|u| ours.iter().any(|x| x == u)) {
                            s.st["left"].as_array_mut().map(|a| a.push(v["metadata"]["name"].clone()));
                        }
                    }
                    if landed(&mut s) {
                        if s.st["left"].as_array().is_some_and(|l| !l.is_empty()) {
                            if s.since("orphans") > 180.0 {
                                return Ok(s.fail(format!("ComputeVMs of this run left 3 min after the release: {}", s.st["left"])));
                            }
                            return Ok(s.keep(h.timer("poll", 5_000)));
                        }
                        let h = after_release(&mut s, h, &name, &shape);
                        return Ok(s.keep(h));
                    }
                }
                _ => {}
            }
        }
        Event::Closed { id, error } if id.starts_with("req:") && s.is("held") && s.st["rentals"][id[4..].parse::<usize>().unwrap_or(0)]["failed"] != true => {
            return Ok(s.fail(format!("{id} closed: {}", error.unwrap_or_default())));
        }
        _ => {}
    }
    Ok(s.keep(h))
}

fn fallback(r: Run<Value>) -> Result<Handle, String> {
    drive(r, Shape { bidders: 3, rentals: 3, fit: 1, need_refusal: true, rounds: 3, fails: 0 })
}

fn concurrent_0014(r: Run<Value>) -> Result<Handle, String> {
    drive(r, Shape { bidders: 2, rentals: 4, fit: 2, need_refusal: false, rounds: 1, fails: 0 })
}

fn exhausted_0014(r: Run<Value>) -> Result<Handle, String> {
    drive(r, Shape { bidders: 2, rentals: 3, fit: 1, need_refusal: true, rounds: 1, fails: 1 })
}

entries! { run => fallback, concurrent => concurrent_0014, exhausted => exhausted_0014 }
