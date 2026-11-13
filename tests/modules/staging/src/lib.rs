// tests/compositions/staging/README.md as a state machine: rent one nested-virt VM, install the socialweb-computer
// under test on it, `dev up --profile market-network` there (own PLC, relay, PDSes, bidders, gateway), read `dev
// status`, `dev down`. Input {name (the run), requester (the target's binary spec: release:<tag> | sha:<commit>),
// cpus?, mem? (default 16, 32G)}.
// Result {pass, error?, phase, readings}. The host releases the VM however it ends.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host};

const BUCKET: &str = "https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com";
const BIN: &str = "socialweb-computer-linux-amd64";
/// the instance on the VM, as the account stg (dev refuses root; a short state dir: socket paths fit in 107 bytes)
const INST: &str = "--instance stg --state-dir /home/stg/stg/s";
/// dev up's market network: 3 bidders (nested VMs), the gateway, vm and deno compute
const DEV_UP: &str = "--profile market-network --bidders 3 --with-gateway --compute-kinds vm,deno --host-backend proc --ready-timeout 90s --network-timeout 20m";

/// The bucket prefix of a binary spec: releases/<tag> or main/<commit>.
fn prefix(spec: &str) -> Result<String, String> {
    match spec.split_once(':') {
        Some(("release", tag)) => Ok(format!("releases/{tag}")),
        Some(("sha", sha)) => Ok(format!("main/{sha}")),
        _ => Err(format!("staging installs a published linux binary: set the target's requester to release:<tag> or sha:<commit> (got {spec:?})")),
    }
}

/// As root on the VM: the account stg (kvm, one subordinate id range above the image's, uidmap, lingering), the
/// binary under test in /home/stg/stg checked against SHA256SUMS.
fn install(prefix: &str) -> String {
    let b = format!("{BUCKET}/{prefix}/binaries");
    format!(
        "set -e; id stg >/dev/null 2>&1 || useradd -m -s /bin/bash -G kvm stg; sed -i /^stg:/d /etc/subuid /etc/subgid; \
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
    let args = &run.args;
    let name = format!("{}-vm", args["name"].as_str().ok_or("name is required")?);
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let mut h = Handle::new();
    let ssh = |id: &str, exec: String| host(id, "request.ssh", json!({ "name": name, "exec": exec }));
    // dev down before the result, however it ended (the host then releases the VM)
    let down = |s: &mut S, h: Handle, result: Value| {
        s.st["result"] = result;
        s.set("down");
        s.keep(h.call(ssh("down", bg("down", swc(&format!("dev down {INST}"))))))
    };
    match run.event {
        Event::Start => {
            let p = match prefix(args["requester"].as_str().unwrap_or_default()) {
                Ok(p) => p,
                Err(e) => return Ok(s.fail(e)),
            };
            s.st["prefix"] = json!(p);
            s.set("held");
            // the CI VM's size (owner: 16 vCPU / 32 GB; a bidder refuses more than its host has)
            let cpus = args["cpus"].as_u64().unwrap_or(16).to_string();
            let mem = args["mem"].as_str().unwrap_or("32G");
            let a = json!(["--hold", "--nested-virt", "--cpus", cpus, "--mem", mem, "--expires-in", "4h", "--exec", "true"]);
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
                "contract" if data["phase"] == "Failed" => return Ok(s.fail(format!("the VM contract failed: {data}"))),
                "exited" if !s.has("vm") => return Ok(s.fail(format!("run request exited before the VM was Held: {data}"))),
                "exited" => s.flag("exited"),
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
                    h = h.call(ssh("up", bg("up", swc(&format!("dev up {INST} {DEV_UP}")))));
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
                            h = h.call(ssh("status", swc(&format!("dev status {INST}"))));
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
                    s.st["puts"] = json!(2);
                    h = h
                        .call(host("put-host", "request.put", json!({ "name": name, "what": "testHost", "remote": "/home/stg/stg/swc-test-host" })))
                        .call(host("put-suite", "request.put", json!({ "name": name, "what": "suite", "remote": "/home/stg/stg/suite.tgz" })));
                }
                "status" => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("dev status: {text}") }))),
                "put-host" | "put-suite" if result.output().is_err() => {
                    return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("{id}: {text}") })));
                }
                "put-host" | "put-suite" => {
                    let left = s.st["puts"].as_u64().unwrap_or(1) - 1;
                    s.st["puts"] = json!(left);
                    if left == 0 {
                        let list: Vec<String> = args["suite"].as_array().map(|a| a.iter().filter_map(|v| v.as_str().map(String::from)).collect()).unwrap_or_else(|| SUITE.map(String::from).to_vec());
                        let run_name = args["name"].as_str().unwrap_or_default();
                        let d = "/home/stg/stg";
                        let cmd = format!(
                            "set -e; chmod 0755 {d}/swc-test-host; rm -rf {d}/suite; mkdir -p {d}/suite; tar -xzf {d}/suite.tgz -C {d}/suite; chown -R stg:stg {d}/suite; {}",
                            bg("suite", as_stg(&format!("{d}/suite/compositions/staging/suite.sh {run_name} {}", list.join(" "))))
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

entries! { run => run_staging }
