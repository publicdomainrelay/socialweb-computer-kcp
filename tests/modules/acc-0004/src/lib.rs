// docs/acceptance/0004-vm-atirc-agent.md as a state machine: an agent VM rented by `run request --hold --agent`
// (discovery), its ident.link chain, the owner's mesh node reaches it and it answers, then a fast release that deletes
// the renter's records and tombstones both DIDs. Input {did (the renter), handle, name}. Result {pass, error?, phase,
// readings}. The host cleans up whatever this run started (requesters, invocations, kube objects), however it ends.
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};

const T: &str = "computer.socialweb.test.";
const MIN: u64 = 60_000;

/// Seconds of an ISO time (UTC, `Z`), for elapsed readings.
fn secs(iso: &str) -> f64 {
    let n = |a: usize, b: usize| {
        iso.get(a..b)
            .and_then(|s| s.parse::<i64>().ok())
            .unwrap_or(0)
    };
    let (y, m, d) = (n(0, 4), n(5, 7), n(8, 10));
    let (y2, m2) = if m <= 2 { (y - 1, m + 9) } else { (y, m - 3) };
    let days = 365 * y2 + y2 / 4 - y2 / 100 + y2 / 400 + (153 * m2 + 2) / 5 + d;
    let frac = iso
        .get(19..)
        .and_then(|f| f.trim_end_matches('Z').parse::<f64>().ok())
        .unwrap_or(0.0);
    (days * 86400 + n(11, 13) * 3600 + n(14, 16) * 60 + n(17, 19)) as f64 + frac
}

fn fail(st: &Value, error: String) -> Handle {
    Handle::new().result(json!({ "pass": false, "error": error, "phase": st["phase"], "readings": st["readings"] })).done()
}

fn call(id: &str, what: &str, input: Value) -> Call {
    Call::new(id, format!("{T}{what}")).input(input)
}

fn run_0004(run: Run<Value>) -> Result<Handle, String> {
    let now = secs(run.args["now"].as_str().unwrap_or_default());
    let name = run.args["name"]
        .as_str()
        .ok_or("name is required")?
        .to_string();
    let renter = run.args["did"]
        .as_str()
        .ok_or("did is required")?
        .to_string();
    let handle = run.args["handle"]
        .as_str()
        .ok_or("handle is required")?
        .to_string();
    let mut st = if run.state.is_null() {
        json!({ "phase": "start", "t0": now, "readings": {}, "f": {} })
    } else {
        run.state
    };
    let t = |st: &Value| ((now - st["t0"].as_f64().unwrap_or(now)) * 10.0).round() / 10.0;
    let set = |st: &mut Value, phase: &str| st["phase"] = json!(phase);
    let phase = st["phase"].as_str().unwrap_or_default().to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            h = h
                .call(call("owner", "requester.start", json!({ "name": "owner" })))
                .timer("deadline", 45 * MIN);
        }
        Event::Timer { id } if id == "deadline" => {
            return Ok(fail(&st, format!("not done in 45 min (phase {phase})")));
        }
        Event::Result { id, result, .. } => {
            let out = match result.output() {
                Ok(o) => o.clone(),
                Err(e) => return Ok(fail(&st, format!("{id}: {e}"))),
            };
            match id.as_str() {
                "owner" => {
                    set(&mut st, "held");
                    let args = json!([
                        "--hold",
                        "--exec",
                        "true",
                        "--agent-config",
                        "@agentConfig",
                        "--agent=/etc/atirc/atirc.env"
                    ]);
                    h = h.call(
                        steps::host_sub("req", "request.start", json!({ "name": name, "args": args })),
                    );
                }
                "unit" => {
                    let o = out["output"].as_str().unwrap_or_default();
                    if !o.contains("inactive") || o.matches("root 600").count() != 3 {
                        return Ok(fail(&st, format!("before agentStart: {o}")));
                    }
                    st["t_start"] = json!(now);
                    set(&mut st, "agent");
                    h = h.call(call("agent", "request.agent", json!({ "name": name })));
                }
                "agent" => {}
                "chain" => {
                    if out["ok"] != true {
                        return Ok(fail(&st, format!("chain: {}", out["problems"])));
                    }
                    h = h.call(call("envfile", "account.envFile", json!({})));
                }
                "envfile" => {
                    let node = json!({
                        "apiVersion": "socialweb.computer/v1alpha1", "kind": "AtircNode", "metadata": { "name": "st-owner" },
                        "spec": { "handle": handle, "nick": "st-owner", "channels": [format!("#{}", handle.replace(['.', '-'], "_"))],
                                  "peers": [st["peer"]], "ircPort": 0, "passwordEnvFile": out["path"] }
                    });
                    h = h.call(
                        Call::new("node", "com.fedfork.exec.kube.apply")
                            .input(json!({ "cluster": "requester:owner", "object": node })),
                    );
                }
                "node" => {
                    set(&mut st, "online");
                    h = h.call(
                        steps::host_sub("mesh", "atirc.meshSub", json!({ "cluster": "requester:owner", "node": "st-owner" })),
                    );
                }
                "prompt" => {}
                "summary" => {
                    st["receipt"] = out["contract"]["receiptUri"].clone();
                    st["readings"]["winner"] = out["contract"]["winnerDid"].clone();
                    st["t_rel"] = json!(now);
                    h = h.call(call("release", "request.release", json!({ "name": name })));
                }
                "release" => {
                    let s = out["seconds"].as_f64().unwrap_or(f64::MAX);
                    st["readings"]["release_s"] = json!(s);
                    if s >= 60.0 {
                        return Ok(fail(
                            &st,
                            format!("release took {s} s (fast release: ~15 s)"),
                        ));
                    }
                    set(&mut st, "released");
                }
                "records" => {
                    let mut uris = out["uris"].as_array().cloned().unwrap_or_default();
                    for c in [
                        "link.ident.endorsement",
                        "com.publicdomainrelay.temp.market.association",
                    ] {
                        uris.push(json!(format!(
                            "at://{renter}/{c}/{}",
                            st["r2"].as_str().unwrap_or_default()
                        )));
                    }
                    st["readings"]["records_checked"] = json!(uris.len());
                    h = h.call(call("gone", "assert.recordsGone", json!({ "uris": uris })));
                }
                "gone" => {
                    if out["left"].as_array().is_none_or(|l| !l.is_empty())
                        || out["errors"].as_array().is_some_and(|e| !e.is_empty())
                    {
                        return Ok(fail(
                            &st,
                            format!(
                                "records after release: left {} errors {}",
                                out["left"], out["errors"]
                            ),
                        ));
                    }
                    h = h.call(call(
                        "tomb",
                        "assert.tombstoned",
                        json!({ "dids": [st["r2"], st["agent"]] }),
                    ));
                }
                "tomb" => {
                    if out["alive"].as_array().is_none_or(|a| !a.is_empty()) {
                        return Ok(fail(&st, format!("not tombstoned: {}", out["alive"])));
                    }
                    st["readings"]["seconds"] = json!(t(&st));
                    return Ok(Handle::new()
                        .result(json!({ "pass": true, "readings": st["readings"] }))
                        .done());
                }
                _ => {}
            }
        }
        Event::Message { id, data, .. } if id == "req" => {
            let msg = data["msg"].as_str().unwrap_or_default();
            match msg {
                "contract" if data["phase"] == "Held" && st["f"]["held"].is_null() => {
                    st["f"]["held"] = json!(true);
                    st["readings"]["held_s"] = json!(t(&st));
                }
                "requester_agent_mesh_key_written" => st["f"]["key"] = json!(true),
                "exited" => st["f"]["exited"] = json!(true),
                "requester_association_written" => st["r2"] = data["subject"].clone(),
                "requester_agent_identity" => {
                    let peer = data["peer"].as_str().unwrap_or_default();
                    let (identity, agent) = peer.split_once('@').unwrap_or_default();
                    st["peer"] = json!(peer);
                    st["identity"] = json!(identity);
                    st["agent"] = json!(agent);
                }
                "requester_guest_requester_released" => st["f"]["r2released"] = json!(true),
                "requester_guest_dids_tombstoned" => st["f"]["guestTomb"] = json!(true),
                "requester_guest_requester_release_timeout" => {
                    return Ok(fail(
                        &st,
                        "the release waited for the guest's timeout".into(),
                    ));
                }
                _ => {}
            }
            let f = &st["f"];
            if phase == "held" && f["held"] == true && f["key"] == true && f["exited"] == true {
                set(&mut st, "unit");
                let exec = "systemctl is-active atirc-agent.service; stat -c '%U %a' /etc/atirc/iroh_key.hex /etc/atirc/atirc.env /etc/atirc/config.toml";
                h = h.call(call(
                    "unit",
                    "request.ssh",
                    json!({ "name": name, "exec": exec }),
                ));
            }
            if phase == "agent" && st["r2"].is_string() && st["agent"].is_string() {
                st["readings"]["association_after_agent_start_s"] =
                    json!(now - st["t_start"].as_f64().unwrap_or(now));
                st["readings"]["r2"] = st["r2"].clone();
                st["readings"]["agent"] = st["agent"].clone();
                set(&mut st, "chain");
                h = h.call(call("chain", "assert.agentChain", json!({ "renter": renter, "r2": st["r2"], "agent": st["agent"], "identity": st["identity"] })));
            }
            if phase == "released" && st["f"]["r2released"] == true && st["f"]["guestTomb"] == true
            {
                set(&mut st, "records");
                h = h.call(call("records", "request.records", json!({ "name": name })));
            }
        }
        Event::Message { id, data, .. } if id == "mesh" => {
            let from_agent = data["identity"] == st["identity"];
            let text = data["text"].as_str().unwrap_or_default();
            if phase == "online" && from_agent && text.contains("online") {
                st["readings"]["online_after_agent_start_s"] =
                    json!(now - st["t_start"].as_f64().unwrap_or(now));
                st["t_prompt"] = json!(now);
                set(&mut st, "answer");
                let nick = st["identity"]
                    .as_str()
                    .unwrap_or_default()
                    .split('.')
                    .next()
                    .unwrap_or_default()
                    .to_string();
                h = h.call(call("prompt", "atirc.meshPub", json!({ "cluster": "requester:owner", "node": "st-owner", "text": format!("{nick}: uname -m") })));
            } else if phase == "answer"
                && from_agent
                && (text.contains("x86_64") || text.contains("aarch64"))
            {
                st["readings"]["answer_s"] = json!(now - st["t_prompt"].as_f64().unwrap_or(now));
                set(&mut st, "release");
                h = h.cancel("mesh").call(call(
                    "summary",
                    "request.summary",
                    json!({ "name": name }),
                ));
            }
        }
        Event::Closed { id, error } if id == "req" || (id == "mesh" && phase != "release") => {
            return Ok(fail(
                &st,
                format!("{id} closed: {}", error.unwrap_or_default()),
            ));
        }
        _ => {}
    }
    Ok(h.state(st))
}

entries! { run => run_0004 }
