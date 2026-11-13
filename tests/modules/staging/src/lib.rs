// tests/compositions/staging/README.md as a state machine, first slice: the orchestrator (requester --with-gateway),
// its tenant cluster `stg`, the overlay (mesh keys, the roster record, the orchestrator's own meshd), the VMs (gateway,
// bidder-1..3) on it, every name resolving. Input {did, handle, name (the run), pds}. Result {pass, error?, phase,
// readings}. The host releases the VMs, stops meshd and the requester, and the ledger deletes the roster, however it ends.
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};

const T: &str = "computer.socialweb.test.";
const ROSTER: &str = "computer.socialweb.mesh.roster";
const MIN: u64 = 60_000;
/// (peer, overlay address, cpus, mem)
const VMS: [(&str, &str, u32, &str); 4] = [
    ("gateway", "10.44.0.10", 2, "4G"),
    ("bidder-1", "10.44.0.11", 4, "8G"),
    ("bidder-2", "10.44.0.12", 4, "8G"),
    ("bidder-3", "10.44.0.13", 4, "8G"),
];
const ORCH: &str = "10.44.0.1";

fn fail(st: &Value, error: String) -> Handle {
    Handle::new().result(json!({ "pass": false, "error": error, "phase": st["phase"], "readings": st["readings"] })).done()
}

fn call(id: &str, what: &str, input: Value) -> Call {
    Call::new(id, format!("{T}{what}")).input(input)
}

fn roster(st: &Value, zone: &str) -> Value {
    let ep = &st["endpoints"];
    let mut peers = vec![
        json!({ "name": "orchestrator", "endpoint": ep["orchestrator"], "cidrs": [format!("{ORCH}/32")] }),
    ];
    for (n, ip, _, _) in VMS {
        peers.push(json!({ "name": n, "endpoint": ep[n], "cidrs": [format!("{ip}/32")] }));
    }
    json!({ "$type": ROSTER, "zone": zone, "peers": peers, "createdAt": st["createdAt"] })
}

fn user_data(peer: &str, ip: &str, url: &str, zone: &str) -> String {
    format!(
        "#cloud-config\nwrite_files:\n- path: /etc/socialweb-meshd/staging.secret\n  permissions: '0600'\n  content: \"@meshKey:{peer}\"\nruncmd:\n- [sh, -c, \"nohup socialweb-meshd run --secret-file /etc/socialweb-meshd/staging.secret --roster '{url}' --dns-zone {zone} --dns-listen {ip}:53 > /var/log/socialweb-meshd-staging.log 2>&1 &\"]\n- [sh, -c, \"printf 'nameserver {ip}\\\\n' > /etc/resolv.conf\"]\n"
    )
}

fn run_staging(run: Run<Value>) -> Result<Handle, String> {
    let s = |k: &str| {
        run.args[k]
            .as_str()
            .map(str::to_string)
            .ok_or(format!("{k} is required"))
    };
    let (did, name, pds) = (s("did")?, s("name")?, s("pds")?);
    let now = run.args["now"].as_str().unwrap_or_default().to_string();
    let zone = format!("{name}.staging.internal");
    let url = format!(
        "{}/xrpc/com.atproto.repo.getRecord?repo={did}&collection={ROSTER}&rkey={name}",
        pds.trim_end_matches('/')
    );
    let mut st = if run.state.is_null() {
        json!({ "phase": "start", "readings": { "zone": zone, "started": now } })
    } else {
        run.state
    };
    let phase = st["phase"].as_str().unwrap_or_default().to_string();
    let mut h = Handle::new();
    let at = |st: &mut Value, k: &str| st["readings"][k] = json!(now);
    match run.event {
        Event::Start => {
            h = h
                .call(call(
                    "orch",
                    "requester.start",
                    json!({ "name": "orch", "withGateway": true }),
                ))
                .timer("deadline", 60 * MIN);
        }
        Event::Timer { id } if id == "deadline" => {
            return Ok(fail(&st, format!("not up in 60 min (phase {phase})")));
        }
        Event::Timer { id } if id == "poll" => {
            h = h.call(Call::new("held", "com.fedfork.exec.kube.get").input(json!({ "cluster": "requester:orch", "apiVersion": "socialweb.computer/v1alpha1", "kind": "EphemeralInfra", "name": name })));
        }
        Event::Result { id, result, .. } => {
            let out = match result.output() {
                Ok(o) => o.clone(),
                Err(e) => return Ok(fail(&st, format!("{id}: {e}"))),
            };
            match id.as_str() {
                "orch" => {
                    st["phase"] = json!("cluster");
                    at(&mut st, "requester");
                    h = h.call(call(
                        "cluster",
                        "requester.cluster",
                        json!({ "name": "orch", "cluster": "stg" }),
                    ));
                }
                "cluster" => {
                    st["phase"] = json!("keys");
                    at(&mut st, "cluster");
                    let names: Vec<&str> = std::iter::once("orchestrator")
                        .chain(VMS.iter().map(|v| v.0))
                        .collect();
                    h = h.call(call("keys", "staging.meshKeys", json!({ "names": names })));
                }
                "keys" => {
                    st["phase"] = json!("roster");
                    st["endpoints"] = out["endpoints"].clone();
                    st["createdAt"] = json!(now);
                    let record = roster(&st, &zone);
                    h = h.call(Call::new("roster", "com.atproto.repo.putRecord").input(json!({ "repo": did, "collection": ROSTER, "rkey": name, "record": record })));
                }
                "roster" => {
                    st["phase"] = json!("mesh");
                    h = h.call(call("mesh", "staging.meshJoin", json!({ "peer": "orchestrator", "address": ORCH, "roster": url, "zone": zone })));
                }
                "mesh" => {
                    st["phase"] = json!("vms");
                    let contracts: Vec<Value> = VMS
                        .iter()
                        .map(|(n, ip, cpus, mem)| {
                            json!({ "name": n, "state": "Requested", "vmName": format!("{name}-{n}"), "cpus": cpus, "mem": mem,
                                    "userData": user_data(n, ip, &url, &zone) })
                        })
                        .collect();
                    let infra = json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "EphemeralInfra", "metadata": { "name": name },
                                        "spec": { "description": format!("staging {name}"), "ttlSeconds": 7200, "endWhen": "Deadline", "contracts": contracts } });
                    h = h.call(call(
                        "vms",
                        "requester.infra",
                        json!({ "name": "orch", "object": infra }),
                    ));
                }
                "vms" => {
                    st["phase"] = json!("held");
                    h = h.timer("poll", 10_000);
                }
                "held" => {
                    let cs = out["status"]["contracts"]
                        .as_array()
                        .cloned()
                        .unwrap_or_default();
                    if let Some(c) = cs.iter().find(|c| c["phase"] == "Failed") {
                        return Ok(fail(&st, format!("contract {} Failed", c["name"])));
                    }
                    if cs.len() == VMS.len() && cs.iter().all(|c| c["phase"] == "Held") {
                        st["phase"] = json!("dns");
                        at(&mut st, "held");
                        let names: Vec<String> = VMS.iter().map(|v| v.0.to_string()).collect();
                        h = h.call(call("dns", "staging.probe", json!({ "dns": format!("{ORCH}:53"), "zone": zone, "names": names, "timeoutS": 600 })));
                    } else {
                        h = h.timer("poll", 10_000);
                    }
                }
                "dns" => {
                    st["readings"]["resolved"] = out["resolved"].clone();
                    at(&mut st, "dns");
                    if out["failed"].as_array().is_some_and(|f| !f.is_empty()) {
                        return Ok(fail(&st, format!("unresolved: {}", out["failed"])));
                    }
                    st["phase"] = json!("up");
                    return Ok(h
                        .result(json!({ "pass": true, "phase": "up", "readings": st["readings"] }))
                        .done());
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(h.state(st))
}

entries! { run => run_staging }
