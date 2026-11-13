// The runbooks' assertions, as contracts a test module calls (composition edges):
// records_gone {uris}: each read 3 times, {left, errors} (RecordNotFound is gone).
// tombstoned {dids}: PLC log/last and the document (identlink.plc), {alive}.
// agent_chain {renter, r2, agent, identity}: the ident.link pair + grant chain of an agent VM (0004), {ok, problems, walk}.
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

fn gone(e: &str) -> bool {
    e.contains("RecordNotFound") || e.contains("Could not locate record")
}

fn records_gone_handler(run: Run<Value>) -> Result<Handle, String> {
    let uris: Vec<String> = serde_json::from_value(run.args["uris"].clone())
        .map_err(|_| "uris is a list of at:// URIs")?;
    let mut st = if run.state.is_null() {
        json!({ "pending": 0, "left": [], "errors": [] })
    } else {
        run.state
    };
    Ok(match run.event {
        Event::Start => {
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
                Handle::new()
                    .result(json!({ "left": st["left"], "errors": st["errors"] }))
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
    let mut st = if run.state.is_null() {
        json!({ "pending": dids.len(), "alive": [] })
    } else {
        run.state
    };
    Ok(match run.event {
        Event::Start => dids
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
                Handle::new().result(json!({ "alive": st["alive"] })).done()
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
    let mut st = if run.state.is_null() {
        json!({ "got": {} })
    } else {
        run.state
    };
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
        Event::Start => {
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
                    && v("grant2")["contract"] == v("grant")["contract"]
                    && v("grant2")["expiresAt"] == v("grant")["expiresAt"],
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
            Handle::new()
                .result(json!({ "ok": problems.is_empty(), "problems": problems, "walk": w }))
                .done()
        }
        _ => Handle::new().state(st),
    })
}

entries! { records_gone => records_gone_handler, tombstoned => tombstoned_handler, agent_chain => agent_chain_handler }
