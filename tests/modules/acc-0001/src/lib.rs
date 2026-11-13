// docs/acceptance/0001-auth-modes.md sections 3-6 on the gateway: a tenant cluster, /readyz through the kubeconfig's
// /clusters/... path, a no-expiry kubeconfig and its revoke over xrpc and on the k8s page (/api with Origin), then a
// deployment with required pod anti-affinity scaled 1 -> 2 -> 3 -> 2 -> 1 -> 0, each replica on its own node; per step
// the gateway requester's VMs on the bidders the host reaches and each released VM's vm.terminated. Discovery only.
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, names, out, ready_pods, termination_rkey};

const N: &str = "com.publicdomainrelay.temp.cluster.";
const STEPS: [usize; 6] = [1, 2, 3, 2, 1, 0];
const DEPLOY: &str = r#"apiVersion: apps/v1
kind: Deployment
metadata:
  name: scale-check
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: scale-check
  template:
    metadata:
      labels:
        app: scale-check
    spec:
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector:
                matchLabels:
                  app: scale-check
              topologyKey: kubernetes.io/hostname
      containers:
        - name: run
          image: docker.io/library/busybox:1.36.1
          command: ["sh", "-c", "sleep 3600"]
"#;

fn ns() -> Value {
    json!({ "apiVersion": "v1", "kind": "Namespace" })
}

fn run_0001(run: Run<Value>) -> Result<Handle, String> {
    let a = &run.args;
    let mut s = S::new(run.state, a["now"].as_str().unwrap_or_default());
    let gw = a["gateway"]["url"].as_str().ok_or("gateway.url")?.trim_end_matches('/').to_string();
    let cl = a["name"].as_str().ok_or("name")?.to_string();
    let tenant = format!("tenant:{cl}");
    let xrpc = |id: &str, m: &str, input: Value| Call::new(id, format!("{N}{m}")).input(input).service(gw.clone());
    let query = |id: &str, m: &str, params: Value| Call::new(id, format!("{N}{m}")).params(params).service(gw.clone());
    let page = |id: &str, path: &str, body: Value| host(id, "page.fetch", json!({ "url": format!("{gw}{path}"), "method": "POST", "body": body }));
    let bidders: Vec<(String, String)> = a["bidders"].as_array().into_iter().flatten().filter_map(|b| Some((b["did"].as_str()?.to_string(), b["cluster"].as_str()?.to_string()))).collect();
    let gw_did = a["gateway"]["requesterDid"].as_str().unwrap_or_default().to_string();
    let poll = |s: &S, h: Handle| s.keep(h.timer("poll", 5_000));
    // the gateway requester's VMs: a list per bidder cluster
    let list_vms = |h: Handle| bidders.iter().enumerate().fold(h, |h, (j, (_, c))| h.call(kube(&format!("vm-{j}"), "list", c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" }))));
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("create");
            h = h.call(xrpc("create", "createCluster", json!({ "name": cl }))).timer("deadline", 90 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 90 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = match s.phase().as_str() {
                "create" | "gone" => h.call(query("list", "listClusters", json!({}))),
                "refused-ne" => h.call(kube("q-ne", "list", &format!("{tenant}-ne"), ns())),
                "refused-pg" => h.call(kube("q-pg", "list", &format!("{tenant}-pg"), ns())),
                "settle" => h.call(kube("nodes", "list", &tenant, json!({ "apiVersion": "v1", "kind": "Node" }))),
                _ => h,
            };
        }
        Event::Timer { id } if id == "relist" => {
            s.st["fan"] = json!(bidders.len());
            s.st["vms"] = json!([]);
            h = list_vms(h);
        }
        // the bidder pins vm.terminated seconds after its VM is gone: read it again every 3 s for up to 2 min
        Event::Timer { id } if id.starts_with("term-") => {
            let uri = s.st["term"][&id[5..]].as_str().unwrap_or_default().to_string();
            h = h.call(get_record(&id, &uri));
        }
        Event::Result { id, result, .. } => {
            let r = result.output().map(Value::clone);
            if id.starts_with("term-") && r.is_err() && s.since("term") < 120.0 {
                return Ok(s.keep(h.timer(&id, 3_000)));
            }
            // refusals the run waits for: an error is the answer
            if id == "q-ne" || id == "q-pg" || id == "final" {
                let refused = r.is_err();
                match id.as_str() {
                    "q-ne" if refused => {
                        s.set("revoked-ok");
                        return Ok(s.keep(h.call(kube("ns2", "list", &tenant, ns()))));
                    }
                    "q-pg" if refused => {
                        s.set("page-ok");
                        return Ok(s.keep(h.call(kube("ns3", "list", &tenant, ns()))));
                    }
                    "final" if refused => return Ok(s.pass()),
                    "final" => return Ok(s.fail("the 8 h kubeconfig still works after deleteCluster")),
                    _ if s.since("revoke") > 30.0 => return Ok(s.fail(format!("{id}: a revoked kubeconfig still works after 30 s"))),
                    _ => return Ok(poll(&s, h)),
                }
            }
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "create" => {
                    s.mark("create");
                    return Ok(poll(&s, h));
                }
                "list" => {
                    let found = o["clusters"].as_array().into_iter().flatten().find(|c| c["name"] == cl.as_str()).cloned();
                    match (s.phase().as_str(), found) {
                        ("create", Some(c)) if c["phase"] == "Ready" => {
                            let t = s.since("create");
                            s.reading("cluster_ready_s", t);
                            s.set("kubeconfig");
                            h = h.call(host("kc", "gateway.kubeconfig", json!({ "name": cl, "ttlSeconds": 28800, "as": cl })));
                        }
                        ("create", _) if s.since("create") > 300.0 => return Ok(s.fail("the cluster is not Ready 5 min after createCluster")),
                        ("gone", None) => {
                            let t = s.since("delete");
                            s.reading("cluster_deleted_s", t);
                            h = h.call(kube("final", "list", &tenant, ns()));
                        }
                        ("gone", Some(_)) if s.since("delete") > 300.0 => return Ok(s.fail("the cluster is listed 5 min after deleteCluster")),
                        _ => return Ok(poll(&s, h)),
                    }
                }
                // the host keeps the kubeconfig (no secret reaches the module or its ticks)
                "kc" => h = h.call(kube("readyz", "raw", &tenant, json!({ "path": "/readyz" }))),
                "readyz" => {
                    if o["text"].as_str().map(str::trim) != Some("ok") {
                        return Ok(s.fail(format!("/readyz through the /clusters/... path: {o}")));
                    }
                    h = h.call(kube("ns", "list", &tenant, ns()));
                }
                "ns" => {
                    s.reading("namespaces", names(&o).len());
                    h = h.call(host("kcne", "gateway.kubeconfig", json!({ "name": cl, "ttlSeconds": 0, "as": format!("{cl}-ne") })));
                }
                "kcne" => h = h.call(kube("nsne", "list", &format!("{tenant}-ne"), ns())),
                "nsne" => h = h.call(query("lk", "listKubeconfigs", json!({ "name": cl }))),
                "lk" | "pks" => {
                    let body = if id == "pks" { &o["body"] } else { &o };
                    let serial = body["kubeconfigs"]
                        .as_array()
                        .into_iter()
                        .flatten()
                        .filter(|k| k["noExpiry"] == true && k["revoked"] != true)
                        .max_by_key(|k| k["createdAt"].as_str().unwrap_or_default().to_string())
                        .and_then(|k| k["serial"].as_str())
                        .map(String::from);
                    let Some(serial) = serial else { return Ok(s.fail(format!("{id}: no live noExpiry kubeconfig: {o}"))) };
                    h = if id == "lk" {
                        h.call(xrpc("rv", "revokeKubeconfig", json!({ "name": cl, "serial": serial })))
                    } else {
                        h.call(page("prv", "/api/clusters/kubeconfigs/revoke", json!({ "name": cl, "serial": serial })))
                    };
                }
                "rv" | "prv" => {
                    let revoked = if id == "rv" { o["revoked"] == true } else { o["status"] == 200 && o["body"]["revoked"] == true };
                    if !revoked {
                        return Ok(s.fail(format!("{id}: not revoked: {o}")));
                    }
                    s.mark("revoke");
                    s.set(if id == "rv" { "refused-ne" } else { "refused-pg" });
                    return Ok(poll(&s, h));
                }
                "ns2" => {
                    s.set("page");
                    h = h.call(host("ps", "page.signin", json!({ "url": format!("{gw}/"), "done": "Signed in as" })));
                }
                "ps" => h = h.call(host("pk", "page.kubeconfig", json!({ "name": cl, "ttlSeconds": 0, "as": format!("{cl}-pg") }))),
                "pk" => h = h.call(kube("nspg", "list", &format!("{tenant}-pg"), ns())),
                "nspg" => h = h.call(page("pks", "/api/clusters/kubeconfigs", json!({ "name": cl }))),
                "ns3" => {
                    s.set("deploy");
                    h = h.call(kube("dep", "apply", &tenant, json!({ "yaml": DEPLOY })));
                }
                "dep" => {
                    s.st["i"] = json!(0);
                    s.st["fan"] = json!(bidders.len());
                    s.st["vms"] = json!([]);
                    s.set("vms0");
                    h = list_vms(h);
                    if bidders.is_empty() {
                        s.set("scale");
                        h = h.call(kube("scale", "patch", &tenant, json!({ "apiVersion": "apps/v1", "kind": "Deployment", "namespace": "default", "name": "scale-check", "patch": { "spec": { "replicas": STEPS[0] } } })));
                        s.mark("step");
                    }
                }
                v if v.starts_with("vm-") => {
                    for vm in o["items"].as_array().into_iter().flatten().filter(|v| v["spec"]["requester"]["did"] == gw_did.as_str()) {
                        let j: usize = v[3..].parse().unwrap_or(0);
                        s.st["vms"].as_array_mut().unwrap().push(json!([bidders[j].0, vm["spec"]["contract"]["receipt"]["uri"]]));
                    }
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left > 0 {
                        return Ok(s.keep(h));
                    }
                    let now: Vec<Value> = s.st["vms"].as_array().cloned().unwrap_or_default();
                    let before: Vec<Value> = s.st["prev"].as_array().cloned().unwrap_or_default();
                    let i = s.st["i"].as_u64().unwrap_or(0) as usize;
                    if s.is("orphans") {
                        let n = STEPS[i];
                        if !gw_did.is_empty() && now.len() != n {
                            // a node leaves the cluster seconds before its VM is released: ask again for up to 2 min
                            if s.since("orphans") < 120.0 {
                                return Ok(s.keep(h.timer("relist", 5_000)));
                            }
                            return Ok(s.fail(format!("step {n}: {} gateway VMs on the bidders, want {n} (2 min after the step)", now.len())));
                        }
                        let gone: Vec<Value> = before.iter().filter(|v| !now.contains(v)).cloned().collect();
                        s.st["prev"] = json!(now);
                        if !gone.is_empty() {
                            s.st["fan"] = json!(gone.len());
                            s.set("terminated");
                            s.mark("term");
                            for (k, g) in gone.iter().enumerate() {
                                let uri = format!("at://{}/{VM_TERMINATED}/{}", g[0].as_str().unwrap_or_default(), termination_rkey(g[1].as_str().unwrap_or_default()));
                                s.st["term"][k.to_string()] = json!(uri);
                                h = h.call(get_record(&format!("term-{k}"), &uri));
                            }
                            return Ok(s.keep(h));
                        }
                        { let h = next_step(&mut s, h, &tenant); return Ok(s.keep(h)); }
                    }
                    // vms0: the baseline, then the first step
                    s.st["prev"] = json!(now);
                    s.set("scale");
                    s.mark("step");
                    h = h.call(kube("scale", "patch", &tenant, json!({ "apiVersion": "apps/v1", "kind": "Deployment", "namespace": "default", "name": "scale-check", "patch": { "spec": { "replicas": STEPS[0] } } })));
                }
                "scale" => {
                    s.set("settle");
                    return Ok(poll(&s, h));
                }
                "nodes" => {
                    s.st["nodes"] = json!(names(&o));
                    return Ok(s.keep(h.call(kube("pods", "list", &tenant, json!({ "apiVersion": "v1", "kind": "Pod", "namespace": "default", "labelSelector": "app=scale-check" })))));
                }
                "pods" => {
                    let i = s.st["i"].as_u64().unwrap_or(0) as usize;
                    let n = STEPS[i];
                    let nodes: Vec<String> = serde_json::from_value(s.st["nodes"].clone()).unwrap_or_default();
                    let pods = ready_pods(&o);
                    let mut on: Vec<&String> = pods.iter().map(|(_, n)| n).collect();
                    on.sort();
                    on.dedup();
                    if nodes.len() == n && pods.len() == n && on.len() == n && on.iter().all(|x| nodes.contains(x)) {
                        let t = s.since("step");
                        if !s.st["readings"]["steps"].is_array() {
                            s.st["readings"]["steps"] = json!([]);
                        }
                        s.st["readings"]["steps"].as_array_mut().unwrap().push(json!({ "replicas": n, "seconds": t, "nodes": nodes }));
                        s.set("orphans");
                        s.mark("orphans");
                        s.st["fan"] = json!(bidders.len());
                        s.st["vms"] = json!([]);
                        if bidders.is_empty() {
                            { let h = next_step(&mut s, h, &tenant); return Ok(s.keep(h)); }
                        }
                        return Ok(s.keep(list_vms(h)));
                    }
                    if s.since("step") > 900.0 {
                        return Ok(s.fail(format!("step to {n} replicas: {} nodes, {} Ready pods on {} nodes after 15 min", nodes.len(), pods.len(), on.len())));
                    }
                    return Ok(poll(&s, h));
                }
                t if t.starts_with("term-") => {
                    if o["value"]["reason"] != "deleted_on_request" || !o["value"]["request"]["uri"].is_string() {
                        return Ok(s.fail(format!("vm.terminated {o}")));
                    }
                    let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
                    s.st["fan"] = json!(left);
                    if left == 0 {
                        { let h = next_step(&mut s, h, &tenant); return Ok(s.keep(h)); }
                    }
                }
                "deld" => h = h.call(xrpc("delc", "deleteCluster", json!({ "name": cl }))),
                "delc" => {
                    s.mark("delete");
                    s.set("gone");
                    return Ok(poll(&s, h));
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

/// After a step's checks: the next replica count, or the end (delete the deployment, then the cluster).
fn next_step(s: &mut S, h: Handle, tenant: &str) -> Handle {
    let i = s.st["i"].as_u64().unwrap_or(0) as usize + 1;
    s.st["i"] = json!(i);
    if i == STEPS.len() {
        s.set("delete");
        return h.call(kube("deld", "delete", tenant, json!({ "apiVersion": "apps/v1", "kind": "Deployment", "namespace": "default", "name": "scale-check" })));
    }
    s.set("scale");
    s.mark("step");
    h.call(kube("scale", "patch", tenant, json!({ "apiVersion": "apps/v1", "kind": "Deployment", "namespace": "default", "name": "scale-check", "patch": { "spec": { "replicas": STEPS[i] } } })))
}

mod s2;

entries! { run => run_0001, s2 => s2::s2 }
