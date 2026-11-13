// docs/acceptance/0013-mixed-prod.md. a: a long-lived `run requester`, mixed-prod.yaml with its bidder pins taken
// out (discovery): vm1 ssh over iroh and webrtc, sbx1's stdout, agent1 (bundle pre-written, association, owner mesh
// node, an answer), a Ctrl-C restart keeps everything Held, the delete releases and deletes every record, the guest
// DIDs tombstoned, hosts empty. c: `run requester --with-gateway`, cluster c1, k8s-cluster.yaml on a market node, scale
// to zero, the infra mirror records gone. b (root on a staging VM): `install requester`, Ready, a unit restart keeps
// the DID, uninstall.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{ASSOC, ENDORSE, MIN, S, host, kube, out, ready_pods, sw};

const CC: [&str; 3] = ["vm1", "sbx1", "agent1"];

fn chan(handle: &str) -> String {
    format!("#{}", handle.replace(['.', '-'], "_"))
}

fn mixed_prod(text: &str, key: &str) -> String {
    text.lines().filter(|l| !l.contains("bidderDids:") && !l.contains("REPLACE-BIDDER-DID")).collect::<Vec<_>>().join("\n").replace("ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop", key.trim())
}

fn a_handler(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let did = args["did"].as_str().ok_or("did")?.to_string();
    let handle = args["handle"].as_str().ok_or("handle")?.to_string();
    let rq = "requester:mp";
    let cc = |c: &str| format!("einfra-mixedprod-{c}");
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("requester");
            h = h.call(host("req", "requester.start", json!({ "name": "mp" }))).timer("deadline", 75 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 75 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = match s.phase().as_str() {
                "deployed" => h.call(kube("ei", "get", rq, sw("EphemeralInfra", "mixedprod"))),
                "gone" => h.call(kube("eigone", "get", rq, sw("EphemeralInfra", "mixedprod"))),
                _ => h,
            };
        }
        Event::Message { id, data, .. } if id == "ev" => {
            match data["msg"].as_str().unwrap_or_default() {
                "requester_agent_mesh_key_written" => s.flag("key"),
                "requester_association_written" => s.st["r2"] = data["subject"].clone(),
                "requester_agent_identity" => {
                    let peer = data["peer"].as_str().unwrap_or_default();
                    let (identity, agent) = peer.split_once('@').unwrap_or_default();
                    s.st["peer"] = json!(peer);
                    s.st["identity"] = json!(identity);
                    s.st["agent"] = json!(agent);
                }
                "requester_contract_records_deleted" => s.flag("recordsDeleted"),
                _ => {}
            }
            if s.is("agent") && s.st["r2"].is_string() && s.st["agent"].is_string() {
                let t = s.since("agentStart");
                s.reading("association_after_agent_start_s", t);
                s.set("owner");
                h = h.call(host("envfile", "account.envFile", json!({})));
            }
            if s.is("released") && s.has("recordsDeleted") {
                s.set("records");
                h = h.call(host("records", "requester.records", json!({ "name": "mp" })));
            }
        }
        Event::Message { id, data, .. } if id == "mesh" => {
            let from = data["identity"] == s.st["identity"];
            let text = data["text"].as_str().unwrap_or_default();
            let nick = s.st["identity"].as_str().unwrap_or_default().split('.').next().unwrap_or_default().to_string();
            let prompt = |id: &str| host(id, "atirc.meshPub", json!({ "cluster": rq, "node": "st-owner", "text": format!("{nick}: uname -m") }));
            if s.is("online") && from && text.contains("online") {
                let t = s.since("agentStart");
                s.reading("online_after_agent_start_s", t);
                s.set("answer");
                s.mark("prompt");
                h = h.call(prompt("prompt"));
            } else if (s.is("answer") || s.is("answer2")) && from && (text.contains("x86_64") || text.contains("aarch64")) {
                let t = s.since("prompt");
                if s.is("answer") {
                    s.reading("answer_s", t);
                    s.set("restart");
                    h = h.call(host("restart", "requester.restart", json!({ "name": "mp" })));
                } else {
                    s.reading("answer_after_restart_s", t);
                    s.set("delete");
                    s.mark("delete");
                    h = h.cancel("mesh").call(kube("del", "delete", rq, sw("EphemeralInfra", "mixedprod")));
                }
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) if id == "eigone" => {
                    let _ = f;
                    Value::Null
                }
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "req" => {
                    if o["did"] != did.as_str() {
                        return Ok(s.fail(format!("the requester's DID {} is not the account's", o["did"])));
                    }
                    s.reading("identity_mode", o["identityMode"].clone());
                    s.st["key"] = o["sshKey"].clone();
                    h = h.call(host("ev", "requester.events", json!({ "name": "mp" })).subscribe()).call(kube("mr", "get", rq, sw("MarketRequester", "app")));
                }
                "mr" => {
                    if o["status"]["phase"] != "Ready" || o["status"]["did"] != did.as_str() {
                        return Ok(s.fail(format!("MarketRequester {}", o["status"])));
                    }
                    // a bidder operator vouches the account (else no bids): one getRecord per operator
                    s.st["fan"] = json!(bidders.len());
                    s.st["vouched"] = json!([]);
                    for (j, b) in bidders.iter().enumerate() {
                        h = h.call(steps::get_record(&format!("vouch-{j}"), &format!("at://{}/sh.tangled.graph.vouch/{did}", b["operator"].as_str().unwrap_or_default())));
                    }
                }
                v if v.starts_with("vouch-") => {}
                "bundle" => h = h.call(host("ex", "example", json!({ "name": "mixed-prod.yaml" }))),
                "ex" => {
                    let key = s.st["key"].as_str().unwrap_or_default().to_string();
                    let spec = mixed_prod(o["text"].as_str().unwrap_or_default(), &key);
                    if let Err(e) = steps::keys_are(&spec, &key) {
                        return Ok(s.fail(format!("mixed-prod.yaml before the apply: {e}")));
                    }
                    s.set("deployed");
                    s.mark("apply");
                    h = h.call(kube("apply", "apply", rq, json!({ "yaml": spec })));
                }
                "apply" => return Ok(s.keep(h.timer("poll", 5_000))),
                "ei" => {
                    if o["status"]["phase"] == "Deployed" {
                        let t = s.since("apply");
                        s.reading("deployed_s", t);
                        s.set("winners");
                        for c in CC {
                            h = h.call(kube(&format!("cc-{c}"), "get", rq, sw("ComputeContract", &cc(c))));
                        }
                        s.st["fan"] = json!(CC.len());
                    } else if s.since("apply") > 600.0 {
                        return Ok(s.fail(format!("mixedprod not Deployed in 10 min: {}", o["status"])));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                c if c.starts_with("cc-") => {
                    let name = &c[3..];
                    let w = o["status"]["contract"]["winner"]["did"].clone();
                    if !bidders.iter().any(|b| b["did"] == w) {
                        return Ok(s.fail(format!("{name} won by {w}, not a target bidder")));
                    }
                    s.st["readings"]["winners"][name] = w;
                    if name == "sbx1" {
                        let sb = &o["status"]["contract"]["sandbox"];
                        if sb["exitCode"] != 0 || !sb["stdout"].as_str().is_some_and(|x| x.contains("mixedprod-deno-ok sum=10 on linux/x86_64")) {
                            return Ok(s.fail(format!("sbx1 sandbox {sb}")));
                        }
                    }
                    if s.is("held") && o["status"]["phase"] != "Held" {
                        return Ok(s.fail(format!("{name} is {} after the restart", o["status"]["phase"])));
                    }
                }
                "ssh-iroh" | "ssh-again" => {
                    if !o["output"].as_str().is_some_and(|x| x.contains("mixedprod-vm-ok")) {
                        return Ok(s.fail(format!("{id}: {}", o["output"])));
                    }
                    if id == "ssh-iroh" {
                        h = h.call(host("ssh-webrtc", "requester.ssh", json!({ "name": "mp", "contract": cc("vm1"), "transport": "webrtc", "exec": "uname -m" })));
                    } else {
                        s.set("answer2");
                        s.mark("prompt");
                        let nick = s.st["identity"].as_str().unwrap_or_default().split('.').next().unwrap_or_default().to_string();
                        h = h.call(host("prompt2", "atirc.meshPub", json!({ "cluster": rq, "node": "st-owner", "text": format!("{nick}: uname -m") })));
                    }
                }
                "ssh-webrtc" => {
                    if !o["output"].as_str().is_some_and(|x| x.contains("x86_64")) {
                        return Ok(s.fail(format!("vm1 over webrtc: {}", o["output"])));
                    }
                    s.set("agent");
                    s.mark("agentStart");
                    h = h.call(host("as", "example", json!({ "name": "agent-start.yaml" })));
                }
                "as" => {
                    let y = o["text"].as_str().unwrap_or_default().replace("quickstart-agent-agent1-start", "mixedprod-agent1-start").replace("einfra-quickstart-agent-agent1", "einfra-mixedprod-agent1");
                    h = h.call(kube("agentstart", "apply", rq, json!({ "yaml": y })));
                }
                "envfile" => {
                    let node = json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "AtircNode", "metadata": { "name": "st-owner" },
                        "spec": { "handle": handle, "nick": "st-owner", "channels": [chan(&handle)], "peers": [s.st["peer"]], "ircPort": 0, "passwordEnvFile": o["path"] } });
                    h = h.call(kube("node", "apply", rq, json!({ "object": node })));
                }
                "node" => {
                    s.set("online");
                    h = h.call(host("mesh", "atirc.meshSub", json!({ "cluster": rq, "node": "st-owner" })).subscribe());
                }
                "restart" => {
                    if o["did"] != did.as_str() {
                        return Ok(s.fail("the DID changed across the restart"));
                    }
                    s.reading("restart_ready_s", o["seconds"].clone());
                    s.set("held");
                    s.st["fan"] = json!(CC.len());
                    for c in CC {
                        h = h.call(kube(&format!("cc-{c}"), "get", rq, sw("ComputeContract", &cc(c))));
                    }
                }
                "del" => {
                    s.set("gone");
                    h = h.timer("poll", 5_000);
                }
                "eigone" => {
                    if o.is_null() {
                        let t = s.since("delete");
                        s.reading("delete_s", t);
                        s.set("released");
                        if s.has("recordsDeleted") {
                            s.set("records");
                            h = h.call(host("records", "requester.records", json!({ "name": "mp" })));
                        }
                    } else if s.since("delete") > 300.0 {
                        return Ok(s.fail("mixedprod still there 5 min after the delete"));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "records" => {
                    let mut uris = o["uris"].as_array().cloned().unwrap_or_default();
                    for c in [ENDORSE, ASSOC] {
                        uris.push(json!(format!("at://{did}/{c}/{}", s.st["r2"].as_str().unwrap_or_default())));
                    }
                    s.reading("records_checked", uris.len());
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": uris })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records after the delete: {} {}", o["left"], o["errors"])));
                    }
                    h = h.call(host("tomb", "assert.tombstoned", json!({ "dids": [s.st["r2"], s.st["agent"]] })));
                }
                "tomb" => {
                    if o["alive"].as_array().is_none_or(|x| !x.is_empty()) {
                        return Ok(s.fail(format!("not tombstoned: {}", o["alive"])));
                    }
                    return Ok(s.pass());
                }
                _ => {}
            }
            // fans: vouches, then the bundle; winners, then vm1 ssh; Held checks after the restart, then ssh again
            if id.starts_with("vouch-") || id.starts_with("cc-") {
                if id.starts_with("vouch-") && result.output().is_ok() {
                    s.st["vouched"].as_array_mut().unwrap().push(json!(id));
                }
                let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                s.st["fan"] = json!(left);
                if left == 0 {
                    h = match s.phase().as_str() {
                        "requester" if s.st["vouched"].as_array().is_none_or(|v| v.is_empty()) => return Ok(s.fail("no bidder operator vouches the account")),
                        "requester" => {
                            s.set("bundle");
                            h.call(host("bundle", "requester.bundle", json!({ "name": "mp", "contract": cc("agent1") })))
                        }
                        "winners" => h.call(host("ssh-iroh", "requester.ssh", json!({ "name": "mp", "contract": cc("vm1"), "exec": "cat /root/mixedprod" }))),
                        "held" => h.call(host("ssh-again", "requester.ssh", json!({ "name": "mp", "contract": cc("vm1"), "exec": "cat /root/mixedprod" }))),
                        _ => h,
                    };
                }
            }
        }
        Event::Closed { id, error } if id == "ev" || (id == "mesh" && !s.is("delete") && !s.is("gone")) => {
            return Ok(s.fail(format!("{id} closed: {}", error.unwrap_or_default())));
        }
        _ => {}
    }
    Ok(s.keep(h))
}

fn c_handler(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let did = args["did"].as_str().ok_or("did")?.to_string();
    let t = "tenant:c1";
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            h = h.call(host("req", "requester.start", json!({ "name": "mk", "withGateway": true }))).timer("deadline", 45 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 45 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = match s.phase().as_str() {
                "pods" => h.call(kube("pods", "list", t, json!({ "apiVersion": "v1", "kind": "Pod", "namespace": "default", "labelSelector": "app=quickstart-k8s" }))),
                "zero" => h.call(kube("nodes", "list", t, json!({ "apiVersion": "v1", "kind": "Node" }))),
                "mirror" => h.call(host("records", "requester.records", json!({ "name": "mk" }))),
                _ => h,
            };
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "req" => {
                    if o["gateway"].is_null() {
                        return Ok(s.fail("the ready file names no gateway"));
                    }
                    h = h.call(host("cl", "requester.cluster", json!({ "name": "mk", "cluster": "c1" })));
                }
                "cl" => h = h.call(host("ex", "example", json!({ "name": "k8s-cluster.yaml" }))),
                "ex" => {
                    s.mark("apply");
                    s.set("pods");
                    h = h.call(kube("apply", "apply", t, json!({ "yaml": o["text"] })));
                }
                "apply" => h = h.timer("poll", 5_000),
                "pods" => {
                    let pods = ready_pods(&o);
                    if let Some((pod, node)) = pods.first() {
                        let tt = s.since("apply");
                        s.reading("pod_ready_s", tt);
                        s.reading("node", node.clone());
                        h = h.call(kube("log", "raw", t, json!({ "path": format!("/api/v1/namespaces/default/pods/{pod}/log") })));
                    } else if s.since("apply") > 900.0 {
                        return Ok(s.fail("no Ready quickstart-k8s pod in 15 min"));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "log" => {
                    if !o["text"].as_str().is_some_and(|x| x.contains("quickstart-k8s-ok")) {
                        return Ok(s.fail(format!("pod log: {}", o["text"])));
                    }
                    s.set("zero");
                    s.mark("delete");
                    h = h.call(kube("del", "delete", t, json!({ "apiVersion": "apps/v1", "kind": "Deployment", "namespace": "default", "name": "quickstart-k8s" })));
                }
                "del" => h = h.timer("poll", 5_000),
                "nodes" => {
                    if o["items"].as_array().is_some_and(|i| i.is_empty()) {
                        let tt = s.since("delete");
                        s.reading("nodes_zero_s", tt);
                        s.set("mirror");
                        s.mark("zero");
                        h = h.timer("poll", 10_000);
                    } else if s.since("delete") > 600.0 {
                        return Ok(s.fail("nodes left 10 min after the delete"));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "records" => {
                    let infra: Vec<Value> = o["uris"].as_array().into_iter().flatten().filter(|u| u.as_str().is_some_and(|u| u.contains("/computer.socialweb.infra/"))).cloned().collect();
                    s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                    s.st["infra"] = json!(infra);
                }
                "gone" => {
                    if o["left"].as_array().is_some_and(Vec::is_empty) {
                        return Ok(s.pass());
                    }
                    if s.since("zero") > 180.0 {
                        return Ok(s.fail(format!("records of {did} left after nodes 0: {}", o["left"])));
                    }
                    h = h.timer("poll", 10_000);
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

/// b, staging only: `install requester` as root on a staging VM (the app password on stdin), Ready, a unit restart
/// keeps the DID, `uninstall --purge`.
fn b_handler(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if args["target"] == "prod" {
        return Ok(s.skip("needs staging (root on a staging VM)"));
    }
    let did = args["did"].as_str().ok_or("did")?.to_string();
    let st = "/var/lib/socialweb-computer/swc-requester";
    let ident = format!("--atproto-identity-mode pdsAccount --atproto-handle {} --atproto-pds-url {}", args["handle"].as_str().unwrap_or_default(), args["pds"].as_str().unwrap_or("https://bsky.social"));
    let root = |id: &str, cmd: String, pw: bool| host(id, "root.exec", json!({ "cmd": cmd, "stdinPassword": pw }));
    let mut h = Handle::new();
    match run.event {
        Event::Start => h = h.call(host("put", "root.putBinary", json!({}))).timer("deadline", 30 * MIN),
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 30 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => h = h.call(root("ready", format!("cat {st}/requesters/mp/ready.json 2>/dev/null || true"), false)),
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            if o.get("exit").is_some_and(|e| e != 0) && id != "ready" {
                return Ok(s.fail(format!("{id}: exit {} {}", o["exit"], o["stderr"])));
            }
            let bin = "/root/swc-st-socialweb-computer";
            match id.as_str() {
                "put" => h = h.call(root("dry", format!("{bin} install requester --name mp --dry-run -- {ident}"), false)),
                "dry" => h = h.call(root("install", format!("{bin} install requester --name mp -- {ident}"), false)),
                "install" => h = h.call(root("pw", format!("runuser -u swc-requester -- {st}/bin/current/socialweb-computer run requester password mp --state-dir {st}"), true)),
                "pw" => {
                    s.set("ready1");
                    s.mark("ready");
                    h = h.timer("poll", 5_000);
                }
                "ready" => {
                    let r: Value = serde_json::from_str(o["stdout"].as_str().unwrap_or_default()).unwrap_or_default();
                    if r["did"].is_string() {
                        if r["did"] != did.as_str() {
                            return Ok(s.fail(format!("ready file DID {}", r["did"])));
                        }
                        if s.is("ready1") {
                            s.set("ready2");
                            s.mark("ready");
                            h = h.call(root("restart", "systemctl --user -M swc-requester@ restart socialweb-computer-requester@mp.service".into(), false));
                        } else {
                            h = h.call(root("uninstall", format!("{st}/bin/current/socialweb-computer uninstall requester --account swc-requester --purge"), false));
                        }
                    } else if s.since("ready") > 600.0 {
                        return Ok(s.fail("no ready file in 10 min"));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "restart" => h = h.timer("poll", 5_000),
                "uninstall" => return Ok(s.pass()),
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => a_handler, c => c_handler, b => b_handler }
