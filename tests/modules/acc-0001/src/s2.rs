// Section 2 (staging only: root on a staging VM): a temporary oauthQr bidder of its own account (the drop-in before
// the first start), signed in by QR as its operator (the run's account), the gateway PDS firehose as its event source
// when the host reaches the gateway kcp; root Ready, offering iroh + webrtc, no endpointUrl. Removed after: uninstall
// --purge, userdel, its records in the operator's repo.
use abi::{Call, Event, Handle, Run};
use serde_json::{Value, json};
use steps::{MIN, S, get_record, host, kube, out, sw};

const ACCT: &str = "swc-st-qr";
const BIN: &str = "/root/swc-st-socialweb-computer";

fn root(id: &str, cmd: String) -> Call {
    host(id, "root.exec", json!({ "cmd": cmd }))
}

pub fn s2(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if args["target"] == "prod" {
        return Ok(s.skip("needs staging (root on a staging VM)"));
    }
    let hdl = args["handle"].as_str().ok_or("handle")?.to_string();
    let did = args["did"].as_str().ok_or("did")?.to_string();
    let st = format!("/var/lib/socialweb-computer/{ACCT}");
    let c = "tenant:qr-bidder";
    let mut h = Handle::new();
    match run.event {
        Event::Start => h = h.call(host("put", "root.putBinary", json!({}))).timer("deadline", 30 * MIN),
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 30 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = match s.phase().as_str() {
                "kubeconfig" => h.call(root("kc", format!("K=$(ls -t {st}/handles/*/kcp/front.json 2>/dev/null | head -1 | sed s,front.json,admin.kubeconfig,); [ -n \"$K\" ] && cat \"$K\" || true"))),
                "qr" => h.call(kube("mi", "get", c, sw("MarketIdentity", "bidder"))),
                "ready" => h.call(kube("mb", "get", c, sw("MarketBidder", "app"))),
                "offering" => h.call(get_record("off", &format!("at://{did}/com.publicdomainrelay.temp.market.offering/self"))),
                _ => h,
            };
        }
        Event::Result { id, result, .. } => {
            if id == "off" && result.output().is_err() {
                return Ok(s.keep(h.timer("poll", 5_000)));
            }
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            if o.get("exit").is_some_and(|e| e != 0) {
                return Ok(s.fail(format!("{id}: exit {} {}", o["exit"], o["stderr"])));
            }
            match id.as_str() {
                "put" => {
                    let d = format!("{st}/.config/systemd/user/socialweb-computer-bidder@{hdl}.service.d");
                    h = h.call(root("install", format!(
                        "useradd --system --user-group --no-create-home --home-dir {st} --shell /usr/sbin/nologin --comment socialweb-computer {ACCT} && \
                         install -d -m 0700 -o {ACCT} -g {ACCT} {st} && install -d -m 0755 -o {ACCT} -g {ACCT} {st}/.config {st}/.config/systemd {st}/.config/systemd/user {d} && \
                         printf '[Service]\\nExecStart=\\nExecStart=%s/bin/current/socialweb-computer run bidder --handle %%i --fg -- --atproto-oauth-qr\\n' {st} | install -m 0644 -o {ACCT} -g {ACCT} /dev/stdin {d}/10-oauth-qr.conf && \
                         {BIN} install bidder --account {ACCT} --adopt-account --handle {hdl} --compute-kinds vm,deno --vm-slots 2"
                    )));
                }
                "install" => {
                    s.set("kubeconfig");
                    s.mark("phase");
                    h = h.timer("poll", 5_000);
                }
                "kc" => match o["stdout"].as_str().filter(|t| !t.trim().is_empty()) {
                    Some(k) => h = h.call(host("add", "kube.addCluster", json!({ "name": "qr-bidder", "kubeconfig": k, "viaRoot": true }))),
                    None if s.since("phase") > 300.0 => return Ok(s.fail("no admin kubeconfig in 5 min")),
                    None => h = h.timer("poll", 5_000),
                },
                "add" => {
                    s.set("qr");
                    s.mark("phase");
                    h = h.timer("poll", 5_000);
                }
                "mi" => match (o["status"]["phase"].as_str(), o["status"]["pendingAction"]["url"].as_str()) {
                    (Some("Ready"), _) => {
                        s.set("ready");
                        s.mark("phase");
                        h = h.timer("poll", 5_000);
                    }
                    (_, Some(url)) if !s.has("signed") => {
                        s.flag("signed");
                        h = h.call(host("signin", "oauth.signin", json!({ "url": url, "done": "You can close this window" })));
                    }
                    _ if s.since("phase") > 600.0 => return Ok(s.fail(format!("MarketIdentity not Ready in 10 min: {}", o["status"]))),
                    _ => h = h.timer("poll", 5_000),
                },
                "signin" => h = h.timer("poll", 5_000),
                "mb" => {
                    if o["status"]["phase"] == "Ready" {
                        match args["gateway"]["cluster"].as_str() {
                            Some(g) => h = h.call(kube("exp", "get", g, json!({ "apiVersion": "exposure.socialweb.computer/v1alpha1", "kind": "Exposure", "name": "svc-pds-pds-ingress" }))),
                            None => {
                                s.reading("event_source", "not applied: the host reaches no gateway kcp");
                                s.set("offering");
                                h = h.timer("poll", 5_000);
                            }
                        }
                    } else if s.since("phase") > 300.0 {
                        return Ok(s.fail(format!("the bidder root is not Ready: {}", o["status"])));
                    } else {
                        h = h.timer("poll", 5_000);
                    }
                }
                "exp" => {
                    let url = o["status"]["address"].clone();
                    let y = format!(
                        "apiVersion: events.socialweb.computer/v1alpha1\nkind: RelayEventSource\nmetadata: {{name: gateway-pds}}\nspec: {{url: {url}}}\n---\napiVersion: events.socialweb.computer/v1alpha1\nkind: EventSource\nmetadata: {{name: gateway-pds}}\nspec: {{backendRef: {{group: events.socialweb.computer, kind: RelayEventSource, name: gateway-pds}}}}\n"
                    );
                    h = h.call(kube("es", "apply", c, json!({ "yaml": y })));
                }
                "es" => {
                    let mut q = sw("MarketBidder", "app");
                    q["patch"] = json!({ "spec": { "suspend": false, "comms": { "eventSourceRefs": [{ "kind": "EventSource", "name": "gateway-pds" }] } } });
                    h = h.call(kube("refs", "patch", c, q));
                }
                "refs" => {
                    s.set("offering");
                    h = h.timer("poll", 5_000);
                }
                "off" => {
                    let kinds: Vec<String> = o["value"]["endpoints"].as_array().into_iter().flatten().filter_map(|e| e["kind"].as_str().map(String::from)).collect();
                    if o["value"].get("endpointUrl").is_some() || kinds.is_empty() || !kinds.iter().all(|k| k == "iroh" || k == "webrtc") {
                        return Ok(s.fail(format!("offering {}", o["value"])));
                    }
                    s.reading("offering_endpoints", json!(kinds));
                    h = h.call(root("remove", format!("{st}/bin/current/socialweb-computer uninstall bidder --account {ACCT} --purge; userdel {ACCT}")));
                }
                "remove" => {
                    s.st["fan"] = json!(3);
                    for (k, (col, rk)) in [("com.publicdomainrelay.temp.market.offering", "self"), ("com.publicdomainrelay.temp.market.heartbeat", "self"), ("computer.socialweb.ingress.listen", "market")].iter().enumerate() {
                        h = h.call(Call::new(format!("del-{k}"), "com.atproto.repo.deleteRecord").input(json!({ "repo": did, "collection": col, "rkey": rk })));
                    }
                }
                d if d.starts_with("del-") => {
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        return Ok(s.pass());
                    }
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}
