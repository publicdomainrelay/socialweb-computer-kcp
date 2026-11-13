// docs/acceptance/0012-infra-quickstart.md on the target's market: every examples/infra file passes a server dry run in
// a `run requester`, vm.yaml Deployed and ssh reads /root/quickstart, deno-sandbox.yaml's stdout, a Ctrl-C restart
// keeps the VM Held and ssh works again, the deletes release both and delete every record. k8s-cluster.yaml and the
// agent join are 0013's.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, kube, out, sw};

const EXAMPLES: [&str; 6] = ["vm.yaml", "deno-sandbox.yaml", "agent-vm.yaml", "mixed-prod.yaml", "mixed.yaml", "agent-start.yaml"];
const KEY: &str = "ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop";

fn run_0012(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let did = args["did"].as_str().ok_or("did")?.to_string();
    let rq = "requester:qs";
    let key = s.st["key"].as_str().unwrap_or_default().to_string();
    let ssh = |id: &str| host(id, "requester.ssh", json!({ "name": "qs", "contract": "infra-quickstart-vm-vm1", "exec": "cat /root/quickstart" }));
    let mut h = Handle::new();
    match run.event {
        Event::Start => h = h.call(host("req", "requester.start", json!({ "name": "qs" }))).timer("deadline", 40 * MIN),
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 40 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = match s.phase().as_str() {
                "vm" => h.call(kube("infra", "get", rq, sw("Infra", "quickstart-vm"))),
                "sbx" => h.call(kube("sbx", "get", rq, sw("ComputeContract", "einfra-quickstart-deno-sbx1"))),
                "gone" => h.call(kube("g1", "get", rq, sw("Infra", "quickstart-vm"))).call(kube("g2", "get", rq, sw("EphemeralInfra", "quickstart-deno"))),
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
                    s.st["key"] = o["sshKey"].clone();
                    s.st["fan"] = json!(EXAMPLES.len());
                    for (j, f) in EXAMPLES.iter().enumerate() {
                        h = h.call(host(&format!("ex-{j}"), "example", json!({ "name": f })));
                    }
                }
                e if e.starts_with("ex-") => {
                    let j: usize = e[3..].parse().unwrap_or(0);
                    let text = o["text"].as_str().unwrap_or_default().replace(KEY, key.trim());
                    if text.contains("sshAuthorizedKeys")
                        && let Err(e) = steps::keys_are(&text, &key)
                    {
                        return Ok(s.fail(format!("{} before the apply: {e}", EXAMPLES[j])));
                    }
                    s.st["ex"][EXAMPLES[j]] = json!(text);
                    h = h.call(kube(&format!("dry-{j}"), "apply", rq, json!({ "yaml": text, "dryRun": true })));
                }
                d if d.starts_with("dry-") => {
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        s.set("vm");
                        s.mark("apply");
                        h = h.call(kube("vmy", "apply", rq, json!({ "yaml": s.st["ex"]["vm.yaml"] }))).call(kube("deno", "apply", rq, json!({ "yaml": s.st["ex"]["deno-sandbox.yaml"] }))).timer("poll", 5_000);
                    }
                }
                "infra" => {
                    if o["status"]["phase"] == "Deployed" {
                        let t = s.since("apply");
                        s.reading("vm_deployed_s", t);
                        h = h.call(ssh("ssh1"));
                    } else if s.since("apply") > 600.0 {
                        return Ok(s.fail(format!("quickstart-vm not Deployed in 10 min: {}", o["status"])));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "ssh1" | "ssh2" => {
                    if !o["output"].as_str().is_some_and(|x| x.contains("quickstart-vm-ok")) {
                        return Ok(s.fail(format!("{id}: {}", o["output"])));
                    }
                    if id == "ssh1" {
                        s.set("sbx");
                        h = h.timer("poll", 1_000);
                    } else {
                        s.set("delete");
                        h = h.call(kube("d1", "delete", rq, sw("Infra", "quickstart-vm"))).call(kube("d2", "delete", rq, sw("EphemeralInfra", "quickstart-deno")));
                        s.st["fan"] = json!(2);
                    }
                }
                "sbx" => {
                    let sb = &o["status"]["contract"]["sandbox"];
                    if sb["stdout"].is_string() {
                        if sb["exitCode"] != 0 || !sb["stdout"].as_str().is_some_and(|x| x.contains("quickstart-deno-ok sum=10")) {
                            return Ok(s.fail(format!("sandbox {sb}")));
                        }
                        h = h.call(host("restart", "requester.restart", json!({ "name": "qs" })));
                    } else if s.since("apply") > 600.0 {
                        return Ok(s.fail("no sandbox stdout in 10 min"));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "restart" => {
                    if o["did"] != did.as_str() {
                        return Ok(s.fail("the DID changed across the restart"));
                    }
                    s.reading("restart_ready_s", o["seconds"].clone());
                    h = h.call(kube("held", "get", rq, sw("ComputeContract", "infra-quickstart-vm-vm1")));
                }
                "held" => {
                    if o["status"]["phase"] != "Held" {
                        return Ok(s.fail(format!("vm1 is {} after the restart", o["status"]["phase"])));
                    }
                    h = h.call(ssh("ssh2"));
                }
                "d1" | "d2" => {
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        s.set("gone");
                        s.mark("delete");
                        h = h.timer("poll", 5_000);
                    }
                }
                "g1" | "g2" => {
                    s.st["gone"][id.as_str()] = json!(o.is_null());
                    if s.st["gone"]["g1"] == true && s.st["gone"]["g2"] == true {
                        h = h.call(host("records", "requester.records", json!({ "name": "qs" })));
                    } else if s.st["gone"]["g1"].is_boolean() && s.st["gone"]["g2"].is_boolean() {
                        if s.since("delete") > 300.0 {
                            return Ok(s.fail("the infras are still there 5 min after the deletes"));
                        }
                        s.st["gone"] = json!({});
                        h = h.timer("poll", 5_000);
                    }
                }
                "records" => {
                    s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                    s.mark("records");
                    s.st["uris"] = o["uris"].clone();
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                }
                "gone" => {
                    if o["left"].as_array().is_some_and(Vec::is_empty) {
                        return Ok(s.pass());
                    }
                    return Ok(s.fail(format!("records of {did} left: {} {}", o["left"], o["errors"])));
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0012 }
