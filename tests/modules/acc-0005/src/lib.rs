// docs/acceptance/0005-guest-requester-association.md. run (A): an outer VM with a guest requester R2 (`run request r2`),
// R2 Held on its nested VM, the renter's contract-scoped grant + endorsement, R2's associationRequest, the walk; the
// outer-only release cascades (r2Released), records gone, R2 tombstoned. b (B): R2 with a co-located account mesh1
// (peer records, R2's grant, agent-of R2, the walk), then the out-of-band loss: the outer ComputeVM deleted on its
// bidder's kcp (only this run's receipt), the renter tombstones both DIDs; without that kcp, a release.
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{ASSOC, ENDORSE, MIN, S, get_record, host, kube, out};

fn walk(id: &str, agent: &str) -> Call {
    Call::new(id, "com.fedfork.exec.identlink.walk").input(json!({ "agent": agent }))
}

fn plc(id: &str, did: &str) -> Call {
    Call::new(id, "com.fedfork.exec.identlink.plc").input(json!({ "did": did }))
}

fn run_a(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state.clone(), args["now"].as_str().unwrap_or_default());
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let renter = args["did"].as_str().ok_or("did")?.to_string();
    let mut h = Handle::new();
    match &run.event {
        Event::Start => {
            s.set("held");
            let a = json!(["--hold", "--guest-tools", "socialweb-computer", "--associate", "guest", "--agent"]);
            h = h.call(host("req", "request.start", json!({ "name": name, "args": a })).subscribe()).timer("deadline", 60 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 60 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = h.call(host("held2", "request.ssh", json!({ "name": name, "exec": "journalctl -u socialweb-r2.service -o cat --no-pager | grep -c '\"phase\":\"Held\"' || true" })));
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => s.flag("held"),
                "exited" => s.flag("exited"),
                "requester_association_written" => s.st["r2"] = data["subject"].clone(),
                "requester_guest_requester_released" => s.flag("r2released"),
                "requester_association_deleted" => s.flag("assocDeleted"),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.has("exited") {
                s.set("r2");
                h = h.call(host("r2", "request.r2", json!({ "name": name, "r2Name": "r2-st", "agent": true })));
            }
            if s.is("assoc") && s.st["r2"].is_string() {
                s.set("r2held");
                s.mark("r2");
                h = h.timer("poll", 10_000);
            }
            if s.is("released") && s.has("r2released") && s.has("assocDeleted") {
                s.set("records");
                h = h.call(host("records", "request.records", json!({ "name": name })));
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            let r2 = s.st["r2"].as_str().unwrap_or_default().to_string();
            match id.as_str() {
                "r2" => {
                    s.set("assoc");
                    if s.st["r2"].is_string() {
                        s.set("r2held");
                        s.mark("r2");
                        h = h.timer("poll", 10_000);
                    }
                }
                "held2" => {
                    let n: u64 = o["output"].as_str().unwrap_or_default().lines().filter_map(|l| l.trim().parse().ok()).next().unwrap_or(0);
                    if n > 0 {
                        let t = s.t();
                        s.reading("r2_held_s", t);
                        s.set("chain");
                        h = h
                            .call(get_record("grant", &format!("at://{renter}/{ASSOC}/{r2}")))
                            .call(get_record("endorse", &format!("at://{renter}/{ENDORSE}/{r2}")))
                            .call(get_record("areq", &format!("at://{r2}/com.publicdomainrelay.temp.market.associationRequest/{renter}")))
                            .call(plc("plc", &r2))
                            .call(walk("walk", &r2));
                        s.st["fan"] = json!(5);
                    } else if s.since("r2") > 600.0 {
                        return Ok(s.fail("R2 not Held in 10 min"));
                    } else {
                        h = h.timer("poll", 10_000);
                    }
                }
                "grant" | "endorse" | "areq" | "plc" | "walk" => {
                    s.st["got"][id.as_str()] = o;
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        let g = s.st["got"].clone();
                        let mut p = vec![];
                        if g["grant"]["value"]["agent"] != r2.as_str() || !g["grant"]["value"]["expiresAt"].is_string() {
                            p.push(format!("renter grant {}", g["grant"]));
                        }
                        if g["endorse"]["value"]["agent"] != r2.as_str() {
                            p.push(format!("renter endorsement {}", g["endorse"]));
                        }
                        if g["areq"]["value"]["principal"] != renter.as_str() || g["areq"]["value"]["contract"] != g["grant"]["value"]["contract"] {
                            p.push(format!("associationRequest {}", g["areq"]));
                        }
                        if !g["plc"]["alsoKnownAs"].as_array().is_some_and(|a| a.contains(&json!(format!("https://ident.link/agent-of/{renter}")))) {
                            p.push(format!("R2 alsoKnownAs {}", g["plc"]["alsoKnownAs"]));
                        }
                        if g["walk"]["roots"] != json!([renter]) {
                            p.push(format!("walk {}", g["walk"]));
                        }
                        if !p.is_empty() {
                            return Ok(s.fail(p.join("; ")));
                        }
                        s.set("released");
                        h = h.call(host("release", "request.release", json!({ "name": name })));
                    }
                }
                "release" => {
                    s.reading("release_s", o["seconds"].clone());
                    if s.has("r2released") && s.has("assocDeleted") {
                        s.set("records");
                        h = h.call(host("records", "request.records", json!({ "name": name })));
                    }
                }
                "records" => {
                    let mut uris = o["uris"].as_array().cloned().unwrap_or_default();
                    uris.push(json!(format!("at://{renter}/{ENDORSE}/{r2}")));
                    uris.push(json!(format!("at://{renter}/{ASSOC}/{r2}")));
                    s.reading("records_checked", uris.len());
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": uris })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left: {} {}", o["left"], o["errors"])));
                    }
                    h = h.call(host("tomb", "assert.tombstoned", json!({ "dids": [r2] })));
                }
                "tomb" => {
                    if o["alive"].as_array().is_none_or(|x| !x.is_empty()) {
                        return Ok(s.fail(format!("not tombstoned: {}", o["alive"])));
                    }
                    return Ok(s.pass());
                }
                _ => {}
            }
        }
        Event::Closed { id, error } if id == "req" => return Ok(s.fail(format!("req closed: {}", error.clone().unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}

fn run_b(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let renter = args["did"].as_str().ok_or("did")?.to_string();
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            let a = json!(["--hold", "--guest-tools", "socialweb-computer", "--associate", "guest"]);
            h = h.call(host("req", "request.start", json!({ "name": name, "args": a })).subscribe()).timer("deadline", 60 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 60 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            let r2 = s.st["r2"].as_str().unwrap_or_default().to_string();
            h = h.call(Call::new("list", "com.atproto.repo.listRecords").params(json!({ "repo": r2, "collection": ENDORSE })).service(r2.clone()));
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => s.flag("held"),
                "exited" => s.flag("exited"),
                "requester_association_written" => s.st["r2"] = data["subject"].clone(),
                "requester_tombstoned" => s.flag("renterTombstoned"),
                "requester_contract_records_deleted" => s.flag("recordsDeleted"),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.has("exited") {
                s.set("start-r2");
                let cmd = "printf 'R2_NAME=r2\\nR2_HOLD=1\\nR2_COLOCATED=mesh1\\n' > /run/socialweb-r2/r2-start.env && chown root:socialweb-r2 /run/socialweb-r2/r2-start.env && chmod 0640 /run/socialweb-r2/r2-start.env && systemctl start --no-block socialweb-r2.service";
                h = h.call(host("r2", "request.ssh", json!({ "name": name, "exec": cmd })));
            }
            if s.is("assoc") && s.st["r2"].is_string() {
                s.set("mesh1");
                s.mark("r2");
                h = h.timer("poll", 10_000);
            }
            if s.is("lost") && s.has("renterTombstoned") && s.has("recordsDeleted") {
                s.set("tomb");
                h = h.call(host("tomb", "assert.tombstoned", json!({ "dids": [s.st["r2"], s.st["mesh1"]] })));
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            let r2 = s.st["r2"].as_str().unwrap_or_default().to_string();
            match id.as_str() {
                "r2" => {
                    s.set("assoc");
                    if s.st["r2"].is_string() {
                        s.set("mesh1");
                        s.mark("r2");
                        h = h.timer("poll", 10_000);
                    }
                }
                "list" => match o["records"].as_array().and_then(|r| r.first()).and_then(|r| r["value"]["agent"].as_str()) {
                    Some(m) => {
                        s.st["mesh1"] = json!(m);
                        s.reading("mesh1", m);
                        h = h.call(plc("plc-r2", &r2));
                    }
                    None if s.since("r2") > 300.0 => return Ok(s.fail("no endorsement of mesh1 in R2's repo in 5 min")),
                    None => h = h.timer("poll", 10_000),
                },
                "plc-r2" => {
                    let r2h = o["alsoKnownAs"].as_array().into_iter().flatten().find_map(|a| a.as_str()?.strip_prefix("at://").map(String::from)).unwrap_or_default();
                    let m = s.st["mesh1"].as_str().unwrap_or_default().to_string();
                    s.st["fan"] = json!(4);
                    h = h
                        .call(get_record("peer", &format!("at://{m}/computer.socialweb.peer/mesh1.{r2h}")))
                        .call(get_record("g2", &format!("at://{r2}/{ASSOC}/{m}")))
                        .call(plc("plc-m", &m))
                        .call(walk("walk", &m));
                }
                "peer" | "g2" | "plc-m" | "walk" => {
                    s.st["got"][id.as_str()] = o;
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        let g = s.st["got"].clone();
                        let mut p = vec![];
                        if !g["peer"]["value"]["iroh"].is_string() || g["peer"]["value"].get("lease").is_some() {
                            p.push(format!("mesh1 peer {}", g["peer"]));
                        }
                        if !g["plc-m"]["alsoKnownAs"].as_array().is_some_and(|a| a.contains(&json!(format!("https://ident.link/agent-of/{r2}")))) {
                            p.push(format!("mesh1 alsoKnownAs {}", g["plc-m"]["alsoKnownAs"]));
                        }
                        if g["walk"]["roots"] != json!([renter]) || !g["walk"]["ancestors"].as_array().is_some_and(|a| a.iter().any(|x| x["did"] == r2.as_str() && x["depth"] == 1)) {
                            p.push(format!("walk {}", g["walk"]));
                        }
                        if !p.is_empty() {
                            return Ok(s.fail(p.join("; ")));
                        }
                        h = h.call(host("summary", "request.summary", json!({ "name": name })));
                    }
                }
                "summary" => {
                    let w = o["contract"]["winnerDid"].clone();
                    let receipt = o["contract"]["receiptUri"].clone();
                    s.st["receipt"] = receipt.clone();
                    match bidders.iter().find(|b| b["did"] == w).and_then(|b| b["cluster"].as_str()) {
                        Some(c) => {
                            s.st["cluster"] = json!(c);
                            s.set("lost");
                            s.reading("out_of_band", true);
                            h = h.call(kube("vms", "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })));
                        }
                        None => {
                            s.reading("out_of_band", "skipped: no kcp for the winner");
                            s.set("lost");
                            h = h.call(host("release", "request.release", json!({ "name": name })));
                        }
                    }
                }
                "vms" => {
                    let vm = o["items"].as_array().into_iter().flatten().find(|v| v["spec"]["contract"]["receipt"]["uri"] == s.st["receipt"]).and_then(|v| v["metadata"]["name"].as_str()).map(String::from);
                    let Some(vm) = vm else { return Ok(s.fail("the outer ComputeVM of this run's receipt is not on its bidder")) };
                    let c = s.st["cluster"].as_str().unwrap_or_default().to_string();
                    h = h.call(kube("delvm", "delete", &c, steps::sw("ComputeVM", &vm)));
                }
                "release" => {
                    s.set("tomb");
                    h = h.call(host("tomb", "assert.tombstoned", json!({ "dids": [s.st["r2"], s.st["mesh1"]] })));
                }
                "tomb" => {
                    if o["alive"].as_array().is_none_or(|x| !x.is_empty()) {
                        return Ok(s.fail(format!("not tombstoned: {}", o["alive"])));
                    }
                    h = h.call(host("records", "request.records", json!({ "name": name })));
                }
                "records" => h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] }))),
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left: {}", o["left"])));
                    }
                    return Ok(s.pass());
                }
                _ => {}
            }
        }
        Event::Closed { id, error } if id == "req" => return Ok(s.fail(format!("req closed: {}", error.unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_a, b => run_b }
