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

fn swc(args: &str) -> String {
    format!("cd /home/stg && runuser -u stg -- env HOME=/home/stg XDG_RUNTIME_DIR=/run/user/$(id -u stg) /home/stg/stg/{BIN} {args} 2>&1")
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
        s.keep(h.call(ssh("down", swc(&format!("dev down {INST}")))))
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
            let a = json!(["--hold", "--nested-virt", "--cpus", cpus, "--mem", mem, "--expires-in", "2h", "--exec", "true"]);
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a }))).timer("deadline", 60 * MIN);
        }
        Event::Timer { id } if id == "deadline" => {
            let e = format!("not up in 60 min (phase {})", s.phase());
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
                    h = h.call(ssh("up", swc(&format!("dev up {INST} {DEV_UP}"))));
                }
                "install" => return Ok(s.fail(format!("install: {text}"))),
                "up" if o["ok"] == true => {
                    s.mark("up");
                    s.set("status");
                    h = h.call(ssh("status", swc(&format!("dev status {INST}"))));
                }
                "up" => return Ok(down(&mut s, h, json!({ "pass": false, "error": format!("dev up: {text}") }))),
                "status" => {
                    s.st["readings"]["status"] = json!(text);
                    let pass = o["ok"] == true;
                    let r = json!({ "pass": pass, "error": (!pass).then(|| format!("dev status: {text}")) });
                    return Ok(down(&mut s, h, r));
                }
                "down" => {
                    let mut r = s.st["result"].clone();
                    if o["ok"] != true && r["pass"] == true {
                        r = json!({ "pass": false, "error": format!("dev down: {text}") });
                    }
                    return Ok(if r["pass"] == true { s.pass() } else { s.fail(r["error"].as_str().unwrap_or_default().to_string()) });
                }
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_staging }
