// tests/compositions/staging/README.md as a state machine: rent one nested-virt VM, install the socialweb-computer
// under test on it, `dev up --profile market-network` there (own PLC, relay, PDSes, bidders, gateway), read `dev
// status`, `dev down`. Input {name (the run), requester (the target's binary spec: release:<tag> | sha:<commit>),
// cpus?, mem? (default 4, 8G), bidders? (dev up's, default 1), fault? ("status": that step fails)}.
// Result {pass, error?, phase, readings}. The host releases the VM however it ends.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host};

const BUCKET: &str = "https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com";
const BIN: &str = "socialweb-computer-linux-amd64";
/// the instance on the VM, as the account stg (dev refuses root; a short state dir: socket paths fit in 107 bytes)
const INST: &str = "--instance stg --state-dir /home/stg/stg/s";
/// dev up's market network: `bidders` bidders (nested VMs), the gateway, vm and deno compute
fn dev_up(bidders: u64) -> String {
    format!("--profile market-network --bidders {bidders} --with-gateway --compute-kinds vm,deno --host-backend proc --ready-timeout 90s --network-timeout 20m")
}

/// The bucket prefix of a binary spec: releases/<tag>, main/<commit> (CI) or local/<commit> (a hand build).
fn prefix(spec: &str) -> Result<String, String> {
    match spec.split_once(':') {
        Some(("release", tag)) => Ok(format!("releases/{tag}")),
        Some(("sha", sha)) => Ok(format!("main/{sha}")),
        Some(("local", sha)) => Ok(format!("local/{sha}")),
        _ => Err(format!("staging installs a published linux binary: set the target's requester to release:<tag>, sha:<commit> or local:<commit> (got {spec:?})")),
    }
}

/// As root on the VM: the account stg (kvm, one subordinate id range above the image's, uidmap, lingering), the
/// binary under test in /home/stg/stg checked against SHA256SUMS.
fn install(prefix: &str) -> String {
    let b = format!("{BUCKET}/{prefix}/binaries");
    format!(
        "set -e; getent group kvm >/dev/null || groupadd -r kvm; id stg >/dev/null 2>&1 || useradd -m -s /bin/bash -G kvm stg; sed -i /^stg:/d /etc/subuid /etc/subgid; \
         usermod --add-subuids 1000000-2048575 --add-subgids 1000000-2048575 stg; \
         command -v newuidmap >/dev/null || {{ apt-get update -q >/dev/null; DEBIAN_FRONTEND=noninteractive apt-get install -y -q uidmap >/dev/null; }}; \
         loginctl enable-linger stg; install -d -o stg -g stg /home/stg/stg /home/stg/stg/s; cd /home/stg/stg; \
         curl -fsSLO {b}/{BIN}; curl -fsSL {b}/SHA256SUMS | grep '  {BIN}$' | sha256sum -c; chmod 0755 {BIN}; echo \"VERSION $(./{BIN} version | head -1)\""
    )
}

/// `cmd` as the account stg (no quotes in cmd: bg wraps it in single quotes).
fn as_stg(cmd: &str) -> String {
    format!("cd /home/stg && runuser -u stg -- env HOME=/home/stg XDG_RUNTIME_DIR=/run/user/$(id -u stg) {cmd} 2>&1")
}

fn swc(args: &str) -> String {
    as_stg(&format!("/home/stg/stg/{BIN} {args}"))
}

/// The first subset of the suite that runs inside the VM (no atirc, no public https origin, no browser yet).
const SUITE: [&str; 7] = ["0003", "0005", "0006", "0007", "0008", "0012", "0013"];

/// A long VM command detached from the ssh session (a dropped session must not end it): `<step>.rc` when done.
fn bg(step: &str, cmd: String) -> String {
    let f = format!("/home/stg/{step}");
    format!("rm -f {f}.rc; (setsid nohup sh -c '{cmd} > {f}.log 2>&1; echo $? > {f}.rc' < /dev/null > /dev/null 2>&1 &); echo started")
}

/// `<rc>` (empty while it runs), then the log's tail.
fn poll(step: &str) -> String {
    format!("cat /home/stg/{step}.rc 2>/dev/null; echo; tail -c 3000 /home/stg/{step}.log")
}

fn run_staging(run: Run<Value>) -> Result<Handle, String> {
    if run.args["layout"] == "multi" {
        return run_multi(run);
    }
    let args = &run.args;
    let name = format!("{}-vm", args["name"].as_str().ok_or("name is required")?);
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let mut h = Handle::new();
    let ssh = |id: &str, exec: String| host(id, "request.ssh", json!({ "name": name, "exec": exec }));
    // dev down before the result, however it ended (the host then releases the VM)
    let down = |s: &mut S, h: Handle, result: Value| {
        if s.is("down") {
            return s.keep(h);
        }
        // a failed run keeps the instance's state (its logs) for the host to read before the release
        let keep = if result["pass"] == true { "" } else { " --keep-state" };
        s.st["result"] = result;
        s.set("down");
        s.keep(h.call(ssh("down", bg("down", swc(&format!("dev down {INST}{keep}"))))))
    };
    match run.event {
        Event::Start => {
            let p = match prefix(args["requester"].as_str().unwrap_or_default()) {
                Ok(p) => p,
                Err(e) => return Ok(s.fail(e)),
            };
            s.st["prefix"] = json!(p);
            s.set("held");
            // small until builds run on it (owner: 4 vCPU / 8 GB, one bidder; a bidder refuses more than its host has free)
            let cpus = args["cpus"].as_u64().unwrap_or(4).to_string();
            let mem = args["mem"].as_str().unwrap_or("8G");
            // the bidder ends the VM 30 min past the run's deadline, whatever becomes of the run (a cancelled CI run)
            let expires = format!("{}m", args["deadlineMin"].as_u64().unwrap_or(180) + 30);
            let a = json!(["--hold", "--nested-virt", "--cpus", cpus, "--mem", mem, "--expires-in", expires, "--exec", "true"]);
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a }))).timer("deadline", args["deadlineMin"].as_u64().unwrap_or(180) * MIN);
        }
        Event::Timer { id } if id.starts_with("poll-") => h = h.call(ssh(&format!("{}-poll", &id[5..]), poll(&id[5..]))),
        Event::Timer { id } if id == "deadline" => {
            let e = format!("not done by the deadline (phase {})", s.phase());
            return Ok(if s.has("installed") { down(&mut s, h, json!({ "pass": false, "error": e })) } else { s.fail(e) });
        }
        Event::Message { id, data, .. } if id == "req" => {
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => s.flag("vm"),
                "contract" if data["phase"] == "Failed" => {
                    let asked = format!("{} vCPU, {}, nestedVirt", args["cpus"].as_u64().unwrap_or(4), args["mem"].as_str().unwrap_or("8G"));
                    return Ok(s.fail(format!("the VM contract failed ({asked}): {}", data["message"].as_str().unwrap_or_default())));
                }
                "exited" if !s.has("vm") => return Ok(s.fail(format!("run request exited before the VM was Held: {data}"))),
                "exited" => s.flag("exited"),
                // a guest requester's held `run request` keeps its kcp and PDS up (for request.ssh and the release):
                // its ssh returning 0 is where an account's run request would exit
                "ssh returned" if args["guestRequester"] == true && data["exit"] == 0 => s.flag("exited"),
                _ => {}
            }
            if s.is("held") && s.has("vm") && s.has("exited") {
                s.mark("held");
                s.set("install");
                h = h.cancel("req").call(ssh("install", install(s.st["prefix"].as_str().unwrap_or_default())));
            }
        }
        Event::Result { id, result, .. } => {
            let o = result.output().cloned().unwrap_or_else(|e| json!({ "ok": false, "output": e.to_string() }));
            let text = o["output"].as_str().unwrap_or_default().to_string();
            match id.as_str() {
                "install" if o["ok"] == true => {
                    s.flag("installed");
                    s.st["readings"]["version"] = json!(text.lines().find_map(|l| l.strip_prefix("VERSION ")));
                    s.mark("installed");
                    s.set("up");
                    h = h.call(ssh("up", bg("up", swc(&format!("dev up {INST} {}", dev_up(args["bidders"].as_u64().unwrap_or(1)))))));
                }
                "install" => return Ok(s.fail(format!("install: {text}"))),
                "up" | "down" if o["ok"] == true => h = h.timer(&format!("poll-{id}"), 10_000),
                "up" | "down" => return Ok(s.fail(format!("{id}: starting it on the VM: {text}"))),
                "up-poll" | "down-poll" | "suite-poll" => {
                    let step = &id[..id.len() - 5];
                    let (rc, log) = text.split_once('\n').unwrap_or((text.as_str(), ""));
                    match rc.trim() {
                        _ if o["ok"] != true => h = h.timer(&format!("poll-{step}"), 10_000),
                        "" => h = h.timer(&format!("poll-{step}"), 10_000),
                        "0" if step == "up" => {
                            s.mark("up");
                            s.set("status");
                            // fault "status": dev status of an instance the VM does not have (a failing step on
                            // the run's own VM only)
                            let inst = if args["fault"] == "status" { "--instance stg-fault --state-dir /home/stg/stg/s" } else { INST };
                            h = h.call(ssh("status", swc(&format!("dev status {inst}"))));
                        }
                        rc if step == "suite" => {
                            let results: Vec<&str> = log.lines().filter_map(|l| l.strip_prefix("RESULT ")).collect();
                            s.st["readings"]["suite"] = json!(results);
                            s.mark("suite");
                            let r = if rc == "0" { json!({ "pass": true }) } else { json!({ "pass": false, "error": format!("suite exit {rc}: {}", results.join(", ")) }) };
                            return Ok(down(&mut s, h, r));
                        }
                        "0" => return Ok(finish(&mut s)),
                        rc if step == "up" => {
                            return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("dev up exit {rc}: {log}") })));
                        }
                        rc => {
                            s.st["result"] = json!({ "pass": false, "error": format!("dev down exit {rc}: {log}") });
                            return Ok(finish(&mut s));
                        }
                    }
                }
                "status" if o["ok"] == true && args["suite"] == false => {
                    s.st["readings"]["status"] = json!(text);
                    return Ok(down(&mut s, h, json!({ "pass": true })));
                }
                "status" if o["ok"] == true => {
                    s.st["readings"]["status"] = json!(text);
                    s.set("suite");
                    // one ssh session at a time: two concurrent `run request ssh` of one invocation broke a pipe
                    s.st["puts"] = json!(1);
                    h = h.call(host("put-host", "request.put", json!({ "name": name, "what": "testHost", "remote": "/home/stg/stg/swc-test-host" })));
                }
                "status" => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("dev status: {text}") }))),
                "put-host" | "put-suite" if result.output().is_err() => {
                    return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{id}: {text}") })));
                }
                "put-host" | "put-suite" => {
                    if id == "put-host" {
                        h = h.call(host("put-suite", "request.put", json!({ "name": name, "what": "suite", "remote": "/home/stg/stg/suite.tgz" })));
                    } else {
                        let list: Vec<String> = args["suite"].as_array().map(|a| a.iter().filter_map(|v| v.as_str().map(String::from)).collect()).unwrap_or_else(|| SUITE.map(String::from).to_vec());
                        let run_name = args["name"].as_str().unwrap_or_default();
                        let d = "/home/stg/stg";
                        let cmd = format!(
                            "set -e; chmod 0755 {d}/swc-test-host; rm -rf {d}/suite; mkdir -p {d}/suite; tar -xzf {d}/suite.tgz -C {d}/suite; chown -R stg:stg {d}/suite; {}",
                            bg("suite", as_stg(&format!("{d}/suite/tests/compositions/staging/suite.sh {run_name} {}", list.join(" "))))
                        );
                        h = h.call(ssh("suite", cmd));
                    }
                }
                "suite" if o["ok"] == true => h = h.timer("poll-suite", 30_000),
                "suite" => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("suite: starting it on the VM: {text}") }))),
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

/// The result kept in the state (dev status's, or the first failure), now that dev down is done.
fn finish(s: &mut S) -> Handle {
    s.mark("down");
    let r = s.st["result"].clone();
    if r["pass"] == true { s.pass() } else { s.fail(r["error"].as_str().unwrap_or_default().to_string()) }
}

/// The multi-VM layout: (name, vCPUs, memory, nested virt). Overlay address 10.44.0.<index + 1>.
const VMS: [(&str, u32, &str, bool); 4] = [("atproto", 2, "4G", false), ("bidder-1", 4, "8G", true), ("bidder-2", 4, "8G", true), ("gateway", 4, "8G", false)];
const MESHD: &str = "/home/stg/stg/socialweb-meshd";
const MESH_SECRET: &str = "/var/lib/socialweb/meshd/secret";
const PLC_PROBE: &str = "/home/stg/stg/plc";

fn overlay(i: usize) -> String {
    format!("10.44.0.{}", i + 1)
}

/// The first JSON object in a command's output that has `key` (JSON log lines may come before it).
fn json_with(text: &str, key: &str) -> Option<Value> {
    text.match_indices('{').find_map(|(i, _)| {
        let v = serde_json::Deserializer::from_str(&text[i..]).into_iter::<Value>().next()?.ok()?;
        v.get(key).is_some().then_some(v)
    })
}

/// A file on the VM from any text (single quotes escaped for the shell).
fn write_file(path: &str, text: &str) -> Result<String, String> {
    let quoted = text.replace('\'', "'\\''");
    Ok(format!("printf %s '{quoted}' > {path}.part && mv {path}.part {path} && chown stg:stg {path}"))
}

/// The joined bidder on a bidder VM: its kcp's MarketBidder `app` (curl on the admin token; no kubectl on the VM).
fn bidder_poll(handle: &str) -> String {
    let k = format!("/home/stg/.local/state/socialweb-computer/dev/b-{}/kcp/admin.kubeconfig", handle.replace('.', "-"));
    format!(
        "[ -f /home/stg/bidder.rc ] && {{ echo exited $(cat /home/stg/bidder.rc); tail -c 1500 /home/stg/bidder.log; exit 0; }}; K={k}; [ -f $K ] || {{ echo none; exit 0; }}; T=$(awk '/ token:/{{print $2; exit}}' $K); S=$(awk '/ server:/{{print $2; exit}}' $K); \
         U=$S/clusters/root/apis/socialweb.computer/v1alpha1/marketbidders/app; \
         curl -fsSk -X PATCH -H \"Authorization: Bearer $T\" -H 'Content-Type: application/merge-patch+json' $U -d '{{\"spec\":{{\"identity\":{{\"association\":\"Skip\"}}}}}}' >/dev/null || true; \
         curl -fsSk -H \"Authorization: Bearer $T\" $U | python3 -c 'import json,sys; s=json.load(sys.stdin).get(\"status\",{{}}); print(s.get(\"phase\",\"-\"), s.get(\"did\",\"-\"), s.get(\"message\",\"\"))'"
    )
}

/// tests/compositions/staging/README.md "Multi-VM": four small VMs on one meshd overlay, joined by one network value.
fn run_multi(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let base = args["name"].as_str().ok_or("name is required")?.to_string();
    let tag: String = base.chars().filter(|c| c.is_ascii_alphanumeric()).collect::<String>().to_lowercase();
    let tag = tag[tag.len().saturating_sub(6)..].to_string();
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let mut h = Handle::new();
    let ssh = |v: &str, id: &str, exec: String| host(id, "request.ssh", json!({ "name": format!("{base}-{v}"), "exec": exec }));
    let handle_of = |v: &str| format!("{}-{tag}.bob.test", v.replace("bidder-", "b"));
    let down = |s: &mut S, h: Handle, result: Value| {
        if s.is("down") {
            return s.keep(h);
        }
        s.st["result"] = result;
        if !s.has("installed:atproto") {
            let r = s.st["result"].clone();
            return if r["pass"] == true { s.pass() } else { s.fail(r["error"].as_str().unwrap_or_default().to_string()) };
        }
        s.set("down");
        s.keep(h.call(host("down", "request.ssh", json!({ "name": format!("{base}-atproto"), "exec": bg("down", swc(&format!("dev down {INST}"))) }))))
    };
    match run.event {
        Event::Start => {
            let p = match prefix(args["requester"].as_str().unwrap_or_default()) {
                Ok(p) => p,
                Err(e) => return Ok(s.fail(e)),
            };
            s.st["prefix"] = json!(p);
            s.set("held");
            let expires = format!("{}m", args["deadlineMin"].as_u64().unwrap_or(120) + 30);
            for (v, cpus, mem, nested) in VMS {
                let mut a = vec![json!("--hold")];
                if nested {
                    a.push(json!("--nested-virt"));
                }
                a.extend([json!("--cpus"), json!(cpus.to_string()), json!("--mem"), json!(mem), json!("--expires-in"), json!(expires), json!("--exec"), json!("true")]);
                // vmName: the RFP's compute.vm role (what the watch page shows), the layer
                h = h.call(steps::host_sub(&format!("req-{v}"), "request.start", json!({ "name": format!("{base}-{v}"), "args": a, "vmName": format!("stg-{v}") })));
            }
            h = h.timer("deadline", args["deadlineMin"].as_u64().unwrap_or(120) * MIN);
        }
        Event::Timer { id } if id == "deadline" => {
            let e = format!("not done by the deadline (phase {})", s.phase());
            return Ok(down(&mut s, h, json!({ "pass": false, "error": e })));
        }
        Event::Timer { id } if id.starts_with("poll-") => {
            let (v, step) = id[5..].split_once(':').unwrap_or(("atproto", &id[5..]));
            let exec = if step == "bidder" { bidder_poll(&handle_of(v)) } else { poll(step) };
            h = h.call(ssh(v, &format!("{v}:{step}-poll"), exec));
        }
        Event::Message { id, data, .. } if id.starts_with("req-") => {
            let v = id[4..].to_string();
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => s.flag(&format!("vm:{v}")),
                "contract" if data["phase"] == "Failed" => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{v}: the VM contract failed: {data}") }))),
                "exited" if !s.has(&format!("vm:{v}")) => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{v}: run request exited before the VM was Held: {data}") }))),
                "exited" => s.flag(&format!("exited:{v}")),
                _ => {}
            }
            if s.has(&format!("vm:{v}")) && s.has(&format!("exited:{v}")) && !s.has(&format!("install:{v}")) {
                s.flag(&format!("install:{v}"));
                s.mark(&format!("held:{v}"));
                s.st["readings"]["heldS"][&v] = json!(s.t());
                h = h.cancel(&id).call(ssh(&v, &format!("{v}:install"), install(s.st["prefix"].as_str().unwrap_or_default())));
            }
        }
        Event::Result { id, result, .. } => {
            let o = result.output().cloned().unwrap_or_else(|e| json!({ "ok": false, "output": e.to_string() }));
            // request.put answers {remote, bytes}; request.ssh {ok, output}
            let ok = result.output().is_ok() && (o["ok"] == true || o["bytes"].is_number());
            let text = o["output"].as_str().unwrap_or_default().to_string();
            if id == "down" || id == "atproto:down-poll" {
                let rc = text.split_once('\n').map(|(rc, _)| rc.trim()).unwrap_or(text.trim());
                if id == "down" && ok || id == "atproto:down-poll" && (!ok || rc.is_empty()) {
                    return Ok(s.keep(h.timer("poll-atproto:down", 10_000)));
                }
                if id == "down" || rc != "0" {
                    s.st["readings"]["downError"] = json!(text);
                }
                s.mark("down");
                let r = s.st["result"].clone();
                return Ok(if r["pass"] == true { s.pass() } else { s.fail(r["error"].as_str().unwrap_or_default().to_string()) });
            }
            let (v, step) = id.split_once(':').unwrap_or(("", id.as_str()));
            let (v, step) = (v.to_string(), step.to_string());
            if s.is("down") {
                return Ok(s.keep(h));
            }
            match step.as_str() {
                "install" if ok => {
                    s.flag(&format!("installed:{v}"));
                    s.st["readings"]["version"] = json!(text.lines().find_map(|l| l.strip_prefix("VERSION ")));
                    h = h.call(host(&format!("{v}:put"), "request.put", json!({ "name": format!("{base}-{v}"), "what": "meshd", "remote": MESHD })));
                }
                "put" if ok => {
                    let cmd = format!(
                        "set -e; chmod 0755 {MESHD}; install -d -m 0700 /var/lib/socialweb/meshd; \
                         [ -s {MESH_SECRET} ] || (umask 077; od -An -tx1 -N32 /dev/urandom | tr -d ' \\n' > {MESH_SECRET}); {MESHD} identity --secret-file {MESH_SECRET}"
                    );
                    h = h.call(ssh(&v, &format!("{v}:id"), cmd));
                }
                "id" if ok => {
                    let endpoint = json_with(&text, "endpoint").and_then(|j| j["endpoint"].as_str().map(String::from));
                    let Some(endpoint) = endpoint else { return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{v}: meshd identity: {text}") }))) };
                    s.st["mesh"][&v] = json!(endpoint);
                    if VMS.iter().all(|(n, ..)| s.st["mesh"][*n].is_string()) {
                        let peers: Vec<Value> = VMS.iter().enumerate().map(|(i, (n, ..))| json!({ "name": n, "endpoint": s.st["mesh"][*n], "cidrs": [format!("{}/32", overlay(i))] })).collect();
                        let roster = json!({ "peers": peers }).to_string();
                        s.set("mesh");
                        for (i, (n, ..)) in VMS.iter().enumerate() {
                            let wait = if i == 0 { format!("ip -br addr | grep -q ' {}/32'", overlay(0)) } else { format!("ping -c1 -W2 {} >/dev/null", overlay(0)) };
                            let cmd = format!(
                                "set -e; {}; (setsid nohup {MESHD} run --secret-file {MESH_SECRET} --roster /home/stg/stg/roster.json > /home/stg/meshd.log 2>&1 < /dev/null &); \
                                 for i in $(seq 90); do {wait} && break; sleep 2; done; {wait} || {{ tail -c 2000 /home/stg/meshd.log; exit 1; }}; ip -br addr show swmesh0",
                                write_file("/home/stg/stg/roster.json", &roster)?
                            );
                            h = h.call(ssh(n, &format!("{n}:mesh"), cmd));
                        }
                    }
                }
                "mesh" if ok => {
                    s.flag(&format!("mesh:{v}"));
                    if s.has("mesh:atproto") && !s.has("up") {
                        s.flag("up");
                        s.mark("up");
                        h = h.call(ssh("atproto", "atproto:up", bg("up", swc(&format!("dev up {INST} --profile market-network --bidders 0 --network-bind {} --host-backend proc --ready-timeout 90s --network-timeout 20m", overlay(0))))));
                    }
                    if VMS.iter().all(|(n, ..)| s.has(&format!("mesh:{n}"))) {
                        s.st["readings"]["meshS"] = json!(s.t());
                    }
                }
                "up" | "bidder" | "request" if ok => h = h.timer(&format!("poll-{v}:{step}"), 10_000),
                "up-poll" | "request-poll" => {
                    let step = &step[..step.len() - 5];
                    let (rc, log) = text.split_once('\n').unwrap_or((text.as_str(), ""));
                    match rc.trim() {
                        _ if !ok => h = h.timer(&format!("poll-{v}:{step}"), 10_000),
                        "" => h = h.timer(&format!("poll-{v}:{step}"), 10_000),
                        "0" if step == "up" => {
                            s.st["readings"]["upS"] = json!(s.since("up"));
                            h = h.call(ssh("atproto", "atproto:status", swc(&format!("dev status {INST}"))));
                        }
                        "0" => {
                            s.st["readings"]["requestS"] = json!(s.since("request"));
                            s.st["readings"]["request"] = json!(log.lines().filter(|l| l.starts_with("RENTED")).collect::<Vec<_>>());
                            return Ok(down(&mut s, h, json!({ "pass": true })));
                        }
                        rc => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{step} exit {rc}: {log}") }))),
                    }
                }
                "status" if ok => {
                    let Some(st) = json_with(&text, "network") else { return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("dev status: {text}") }))) };
                    let net = st["network"]["status"].clone();
                    if net["phase"] != "Ready" {
                        return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("the network is not Ready: {}", net["message"]) })));
                    }
                    s.st["readings"]["network"] = json!({ "plc": net["network"]["plcDirectoryUrl"], "bidderPds": net["bidderPdsUrl"] });
                    let file = match write_file("/home/stg/stg/net.json", &net.to_string()) {
                        Ok(f) => f,
                        Err(e) => return Ok(down(&mut s, h, json!({ "pass": false, "error": e }))),
                    };
                    s.set("bidders");
                    s.mark("bidders");
                    for v in ["bidder-1", "bidder-2"] {
                        let cmd = format!(
                            "set -e; {file}; {}",
                            bg("bidder", swc(&format!("run bidder --market-network-file /home/stg/stg/net.json --handle {} --host-backend proc --fg --vmm qemu --compute-kinds vm --bidder-policy open", handle_of(v))))
                        );
                        h = h.call(ssh(v, &format!("{v}:bidder"), cmd));
                    }
                    let probe = format!("set -e; curl -fsS -o /dev/null -w '%{{http_code}}' {}/did:plc:nobody > {PLC_PROBE} || true; cat {PLC_PROBE}", net["network"]["plcDirectoryUrl"].as_str().unwrap_or_default());
                    h = h.call(ssh("gateway", "gateway:probe", probe));
                }
                "probe" => s.st["readings"]["gatewayPlcProbe"] = json!(text.trim()),
                "bidder-poll" => {
                    let mut words = text.split_whitespace();
                    let (phase, did) = (words.next().unwrap_or_default(), words.next().unwrap_or_default());
                    s.st["readings"]["bidders"][&v] = json!(text.trim());
                    if phase == "exited" {
                        return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{v}: run bidder exited: {text}") })));
                    }
                    if phase == "Ready" && did.starts_with("did:") {
                        s.flag(&format!("ready:{v}"));
                        s.st["readings"]["bidderReadyS"][&v] = json!(s.since("bidders"));
                    } else {
                        h = h.timer(&format!("poll-{v}:bidder"), 15_000);
                    }
                    if s.has("ready:bidder-1") && s.has("ready:bidder-2") && !s.has("request") {
                        s.flag("request");
                        s.mark("request");
                        s.set("request");
                        let one = |n: &str| swc(&format!("run request --network stg --state-dir /home/stg/stg/s --name {n} --exec hostname -- --policy open"));
                        let both = format!(
                            "(sh -c \"{} > /home/stg/p1.out\" & {} > /home/stg/p2.out; rc=$?; wait $! || rc=1; grep -h -o winnerDid.* /home/stg/p1.out /home/stg/p2.out | sed s/^/RENTED/; exit $rc)",
                            one("p1"), one("p2")
                        );
                        let cmd = bg("request", both);
                        h = h.call(ssh("atproto", "atproto:request", cmd));
                    }
                }
                _ if !ok => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{id}: {text}") }))),
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_staging }
