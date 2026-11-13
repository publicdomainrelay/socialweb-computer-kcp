// The runbooks' assertions, as contracts a test module calls (composition edges):
// records_gone {uris}: each read 3 times, {left, errors} (RecordNotFound is gone).
// tombstoned {dids}: PLC log/last and the document (identlink.plc), {alive}.
// agent_chain {renter, r2, agent, identity}: the ident.link pair + grant chain of an agent VM (0004), {ok, problems, walk}.
// Each asks again every 10 s until it holds or `within` seconds (default 180) pass: the writers (a PDS, PLC, R2's
// requester) land seconds after the event that told the test to look. `seconds` is how long it took.
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};

const GET: &str = "com.atproto.repo.getRecord";
const ASSOC: &str = "com.publicdomainrelay.temp.market.association";
const ENDORSE: &str = "link.ident.endorsement";
const PLC: &str = "com.fedfork.exec.identlink.plc";
const WALK: &str = "com.fedfork.exec.identlink.walk";

fn get(id: String, uri: &str) -> Option<Call> {
    let mut p = uri.strip_prefix("at://")?.splitn(3, '/');
    let (repo, collection, rkey) = (p.next()?, p.next()?, p.next()?);
    Some(
        Call::new(id, GET)
            .params(json!({ "repo": repo, "collection": collection, "rkey": rkey }))
            .service(repo),
    )
}

const AGAIN_MS: u64 = 10_000;

/// Not yet: ask again in 10 s (state reset to `fresh`, `t0` kept), or None when `within` has passed.
fn again((now, within): (f64, f64), st: &Value, fresh: Value) -> Option<Handle> {
    let t0 = st["t0"].as_f64().unwrap_or(now);
    if now - t0 >= within {
        return None;
    }
    let mut f = fresh;
    f["t0"] = json!(t0);
    Some(Handle::new().timer("again", AGAIN_MS).state(f))
}

/// Seconds since the first ask.
fn took((now, _): (f64, f64), st: &Value) -> f64 {
    now - st["t0"].as_f64().unwrap_or(now)
}

/// (now, within) of a run, and its state with `t0` set on the first ask.
fn clock(run: &Run<Value>, fresh: &Value) -> ((f64, f64), Value) {
    let now = steps::secs(run.args["now"].as_str().unwrap_or_default());
    let mut st = if run.state.is_null() { fresh.clone() } else { run.state.clone() };
    if st["t0"].is_null() {
        st["t0"] = json!(now);
    }
    ((now, run.args["within"].as_f64().unwrap_or(180.0)), st)
}

fn gone(e: &str) -> bool {
    e.contains("RecordNotFound") || e.contains("Could not locate record")
}

fn records_gone_handler(run: Run<Value>) -> Result<Handle, String> {
    let uris: Vec<String> = serde_json::from_value(run.args["uris"].clone())
        .map_err(|_| "uris is a list of at:// URIs")?;
    let fresh = json!({ "pending": 0, "left": [], "errors": [] });
    let (c, mut st) = clock(&run, &fresh);
    Ok(match run.event {
        Event::Start | Event::Timer { .. } => {
            let mut h = Handle::new();
            for (i, u) in uris.iter().enumerate() {
                for r in 0..3 {
                    h = h.call(
                        get(format!("{i}-{r}"), u)
                            .ok_or_else(|| format!("{u} is not an at:// record URI"))?,
                    );
                }
            }
            if uris.is_empty() {
                return Ok(Handle::new()
                    .result(json!({ "left": [], "errors": [] }))
                    .done());
            }
            st["pending"] = json!(uris.len() * 3);
            h.state(st)
        }
        Event::Result { id, result, .. } => {
            let i: usize = id
                .split('-')
                .next()
                .and_then(|n| n.parse().ok())
                .ok_or("bad id")?;
            match result.output() {
                Ok(_) if !st["left"].as_array().unwrap().contains(&json!(uris[i])) => {
                    st["left"].as_array_mut().unwrap().push(json!(uris[i]))
                }
                Ok(_) => {}
                Err(e) if gone(&e) => {}
                Err(e) => st["errors"]
                    .as_array_mut()
                    .unwrap()
                    .push(json!(format!("{}: {e}", uris[i]))),
            }
            st["pending"] = json!(st["pending"].as_u64().unwrap_or(1) - 1);
            if st["pending"] == 0 {
                let clean = st["left"].as_array().is_some_and(|l| l.is_empty())
                    && st["errors"].as_array().is_some_and(|e| e.is_empty());
                if !clean && let Some(h) = again(c, &st, fresh) {
                    return Ok(h);
                }
                Handle::new()
                    .result(json!({ "left": st["left"], "errors": st["errors"], "seconds": took(c, &st) }))
                    .done()
            } else {
                Handle::new().state(st)
            }
        }
        _ => Handle::new().state(st),
    })
}

fn tombstoned_handler(run: Run<Value>) -> Result<Handle, String> {
    let dids: Vec<String> =
        serde_json::from_value(run.args["dids"].clone()).map_err(|_| "dids is a list of DIDs")?;
    let fresh = json!({ "pending": dids.len(), "alive": [] });
    let (c, mut st) = clock(&run, &fresh);
    Ok(match run.event {
        Event::Start | Event::Timer { .. } => dids
            .iter()
            .enumerate()
            .fold(Handle::new(), |h, (i, d)| {
                h.call(Call::new(i.to_string(), PLC).input(json!({ "did": d })))
            })
            .state(st),
        Event::Result { id, result, .. } => {
            let i: usize = id.parse().map_err(|_| "bad id")?;
            let dead = result
                .output()
                .is_ok_and(|o| o["lastType"] == "plc_tombstone" && o["docStatus"] != 200);
            if !dead {
                st["alive"].as_array_mut().unwrap().push(json!(dids[i]));
            }
            st["pending"] = json!(st["pending"].as_u64().unwrap_or(1) - 1);
            if st["pending"] == 0 {
                if st["alive"].as_array().is_some_and(|a| !a.is_empty()) && let Some(h) = again(c, &st, fresh) {
                    return Ok(h);
                }
                Handle::new().result(json!({ "alive": st["alive"], "seconds": took(c, &st) })).done()
            } else {
                Handle::new().state(st)
            }
        }
        _ => Handle::new().state(st),
    })
}

fn agent_chain_handler(run: Run<Value>) -> Result<Handle, String> {
    let a = &run.args;
    let (renter, r2, agent, identity) = (
        a["renter"].as_str().ok_or("renter")?,
        a["r2"].as_str().ok_or("r2")?,
        a["agent"].as_str().ok_or("agent")?,
        a["identity"].as_str().ok_or("identity")?,
    );
    let fresh = json!({ "got": {} });
    let (c, mut st) = clock(&run, &fresh);
    let reads = [
        ("grant", format!("at://{renter}/{ASSOC}/{r2}")),
        ("endorse", format!("at://{renter}/{ENDORSE}/{r2}")),
        ("grant2", format!("at://{r2}/{ASSOC}/{agent}")),
        ("endorse2", format!("at://{r2}/{ENDORSE}/{agent}")),
        (
            "peer",
            format!("at://{agent}/computer.socialweb.peer/{identity}"),
        ),
    ];
    Ok(match run.event {
        Event::Start | Event::Timer { .. } => {
            let mut h = Handle::new();
            for (id, uri) in &reads {
                h = h.call(get(id.to_string(), uri).ok_or("bad URI")?);
            }
            h.call(Call::new("plc-r2", PLC).input(json!({ "did": r2 })))
                .call(Call::new("plc-agent", PLC).input(json!({ "did": agent })))
                .call(Call::new("walk", WALK).input(json!({ "agent": agent })))
                .state(st)
        }
        Event::Result { id, result, .. } => {
            st["got"][&id] = result
                .output()
                .cloned()
                .unwrap_or_else(|e| json!({ "error": e }));
            if st["got"].as_object().unwrap().len() < reads.len() + 3 {
                return Ok(Handle::new().state(st));
            }
            let g = &st["got"];
            let v = |k: &str| &g[k]["value"];
            let mut problems = vec![];
            let mut need = |ok: bool, what: String| {
                if !ok {
                    problems.push(what);
                }
            };
            need(
                v("grant")["agent"] == r2
                    && (v("grant")["contract"].is_object() || v("grant")["contract"].is_string()),
                format!("renter grant {}", g["grant"]),
            );
            need(
                v("grant")["expiresAt"].is_string(),
                "renter grant without expiresAt".into(),
            );
            need(
                v("endorse")["agent"] == r2,
                format!("renter endorsement {}", g["endorse"]),
            );
            need(
                v("grant2")["agent"] == agent
                    && v("grant2")["contract"]["uri"] == v("grant")["contract"]["uri"]
                    // the same instant (R2 writes .27Z where the renter writes .270Z)
                    && steps::secs(v("grant2")["expiresAt"].as_str().unwrap_or("x")) == steps::secs(v("grant")["expiresAt"].as_str().unwrap_or("y")),
                format!("R2 grant {}", g["grant2"]),
            );
            need(
                v("endorse2")["agent"] == agent,
                format!("R2 endorsement {}", g["endorse2"]),
            );
            need(
                v("peer")["iroh"].is_string() && v("peer").get("lease").is_none(),
                format!("peer record {}", g["peer"]),
            );
            let aka = |k: &str, want: String| {
                g[k]["alsoKnownAs"]
                    .as_array()
                    .is_some_and(|a| a.contains(&json!(want)))
            };
            need(
                aka("plc-r2", format!("https://ident.link/agent-of/{renter}")),
                format!("R2 alsoKnownAs {}", g["plc-r2"]["alsoKnownAs"]),
            );
            need(
                aka("plc-agent", format!("https://ident.link/agent-of/{r2}")),
                format!("agent alsoKnownAs {}", g["plc-agent"]["alsoKnownAs"]),
            );
            for k in ["plc-r2", "plc-agent"] {
                need(
                    g[k]["rotationKeys"]
                        .as_array()
                        .is_some_and(|r| r.len() == 2),
                    format!("{k} rotationKeys {}", g[k]["rotationKeys"]),
                );
            }
            let w = &g["walk"];
            need(
                w["roots"] == json!([renter])
                    && w["errors"].as_array().is_some_and(|e| e.is_empty()),
                format!("walk {w}"),
            );
            need(
                w["ancestors"]
                    .as_array()
                    .is_some_and(|a| a.iter().any(|x| x["did"] == r2 && x["depth"] == 1)),
                format!("walk ancestors {}", w["ancestors"]),
            );
            if !problems.is_empty() && let Some(h) = again(c, &st, fresh) {
                return Ok(h);
            }
            Handle::new()
                .result(json!({ "ok": problems.is_empty(), "problems": problems, "walk": w, "seconds": took(c, &st) }))
                .done()
        }
        _ => Handle::new().state(st),
    })
}

const MARKET: [&str; 2] = ["com.publicdomainrelay.temp.market.", "com.publicdomainrelay.temp.compute."];

/// A listRecords page's records created at or after `since` into `left`, those with no createdAt into `undated`.
fn tally(st: &mut Value, page: &Value, since: f64) {
    for r in page["records"].as_array().into_iter().flatten() {
        match r["value"]["createdAt"].as_str() {
            Some(t) if steps::secs(t) >= since => st["left"].as_array_mut().unwrap().push(r["uri"].clone()),
            Some(_) => {}
            None => st["undated"].as_array_mut().unwrap().push(r["uri"].clone()),
        }
    }
}

/// none_since {did, since, prefixes?}: no record of a collection under `prefixes` (default the market's and compute's)
/// in the repo of `did` was created at or after `since` (its createdAt): {left, undated, errors} (the hand runs' full
/// listRecords scan; it sees records whose URIs no log of the run names, e.g. a separate release process's).
fn none_since_handler(run: Run<Value>) -> Result<Handle, String> {
    let did = run.args["did"].as_str().ok_or("did is required")?.to_string();
    let since = steps::secs(run.args["since"].as_str().ok_or("since is required")?);
    let prefixes: Vec<String> = match run.args["prefixes"].as_array() {
        Some(p) => p.iter().filter_map(|x| x.as_str().map(String::from)).collect(),
        None => MARKET.iter().map(|p| p.to_string()).collect(),
    };
    let fresh = json!({ "pending": 0, "left": [], "undated": [], "errors": [] });
    let (c, mut st) = clock(&run, &fresh);
    Ok(match run.event {
        Event::Start | Event::Timer { .. } => Handle::new()
            .call(Call::new("d", "com.atproto.repo.describeRepo").params(json!({ "repo": did })).service(did.clone()))
            .state(st),
        Event::Result { id, result, .. } if id == "d" => {
            let o = result.output()?.clone();
            let cols: Vec<String> = o["collections"].as_array().into_iter().flatten().filter_map(|x| x.as_str()).filter(|x| prefixes.iter().any(|p| x.starts_with(p.as_str()))).map(String::from).collect();
            if cols.is_empty() {
                return Ok(Handle::new().result(json!({ "left": [], "undated": [], "errors": [], "seconds": took(c, &st) })).done());
            }
            let mut h = Handle::new();
            for (i, col) in cols.iter().enumerate() {
                h = h.call(Call::new(format!("l-{i}"), "com.atproto.repo.listRecords").params(json!({ "repo": did, "collection": col, "limit": 100 })).service(did.clone()));
            }
            st["pending"] = json!(cols.len());
            h.state(st)
        }
        Event::Result { id, result, .. } => {
            match result.output() {
                Ok(o) => {
                    // rkeys are not all TIDs (hash rkeys sort anywhere): every page, up to 50 per collection (the page stays pending)
                    let (i, page) = id.trim_start_matches("l-").split_once('-').map_or((id.trim_start_matches("l-").to_string(), 0), |(a, b)| (a.to_string(), b.parse().unwrap_or(0)));
                    if let Some(cursor) = o["cursor"].as_str().filter(|_| o["records"].as_array().is_some_and(|r| r.len() == 100) && page < 50) {
                        let col = o["records"][0]["uri"].as_str().unwrap_or_default().split('/').nth(3).unwrap_or_default().to_string();
                        let call = Call::new(format!("l-{i}-{}", page + 1), "com.atproto.repo.listRecords").params(json!({ "repo": did, "collection": col, "limit": 100, "cursor": cursor })).service(did.clone());
                        tally(&mut st, o, since);
                        return Ok(Handle::new().call(call).state(st));
                    }
                    tally(&mut st, o, since);
                }
                Err(e) => st["errors"].as_array_mut().unwrap().push(json!(e)),
            }
            st["pending"] = json!(st["pending"].as_u64().unwrap_or(1) - 1);
            if st["pending"] != 0 {
                return Ok(Handle::new().state(st));
            }
            let clean = st["left"].as_array().is_some_and(|l| l.is_empty()) && st["errors"].as_array().is_some_and(|e| e.is_empty());
            if !clean && let Some(h) = again(c, &st, fresh) {
                return Ok(h);
            }
            Handle::new().result(json!({ "left": st["left"], "undated": st["undated"], "errors": st["errors"], "seconds": took(c, &st) })).done()
        }
        _ => Handle::new().state(st),
    })
}

entries! { records_gone => records_gone_handler, tombstoned => tombstoned_handler, agent_chain => agent_chain_handler, none_since => none_since_handler }
