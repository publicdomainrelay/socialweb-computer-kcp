//! The bookkeeping of a runbook's state machine: `S` holds the state (phase, marks, readings, flags), the clock of
//! this event, and ends the run as pass or fail; calls to the test host and to kube by name; the market's rkeys.
use abi::{Call, Event, Handle};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

pub const T: &str = "computer.socialweb.test.";
pub const MIN: u64 = 60_000;
pub const ASSOC: &str = "com.publicdomainrelay.temp.market.association";
pub const ENDORSE: &str = "link.ident.endorsement";
pub const VM_TERMINATED: &str = "com.publicdomainrelay.temp.compute.events.vm.terminated";

/// Seconds of an ISO time (UTC, `Z`).
pub fn secs(iso: &str) -> f64 {
    let n = |a: usize, b: usize| iso.get(a..b).and_then(|s| s.parse::<i64>().ok()).unwrap_or(0);
    let (y, m, d) = (n(0, 4), n(5, 7), n(8, 10));
    let (y2, m2) = if m <= 2 { (y - 1, m + 9) } else { (y, m - 3) };
    let days = 365 * y2 + y2 / 4 - y2 / 100 + y2 / 400 + (153 * m2 + 2) / 5 + d;
    let frac = iso.get(19..).and_then(|f| f.trim_end_matches('Z').parse::<f64>().ok()).unwrap_or(0.0);
    (days * 86400 + n(11, 13) * 3600 + n(14, 16) * 60 + n(17, 19)) as f64 + frac
}

/// termination-payload-<first 32 hex of sha256("terminated:<receipt uri>")>: the receipt's vm.terminated rkey.
pub fn termination_rkey(receipt: &str) -> String {
    let h = Sha256::digest(format!("terminated:{receipt}"));
    format!("termination-payload-{}", h[..16].iter().map(|b| format!("{b:02x}")).collect::<String>())
}

/// A call to the test host: computer.socialweb.test.<what>.
pub fn host(id: &str, what: &str, input: Value) -> Call {
    Call::new(id, format!("{T}{what}")).input(input)
}

/// A subscription of the test host (its log events, an observer): atlibs takes a non-exec subscription's arguments as
/// params, never input, and makes every param a string (an array would be comma-joined): the arguments go as one
/// `json` param, which the host parses.
pub fn host_sub(id: &str, what: &str, args: Value) -> Call {
    Call::new(id, format!("{T}{what}")).params(json!({ "json": args.to_string() })).subscribe()
}

/// A kube call: com.fedfork.exec.kube.<verb> on `cluster`.
pub fn kube(id: &str, verb: &str, cluster: &str, mut input: Value) -> Call {
    input["cluster"] = json!(cluster);
    Call::new(id, format!("com.fedfork.exec.kube.{verb}")).input(input)
}

/// A getRecord of an at:// URI, a public read of its repo.
pub fn get_record(id: &str, uri: &str) -> Call {
    let mut p = uri.trim_start_matches("at://").splitn(3, '/');
    let (repo, collection, rkey) = (p.next().unwrap_or_default(), p.next().unwrap_or_default(), p.next().unwrap_or_default());
    Call::new(id, "com.atproto.repo.getRecord").params(json!({ "repo": repo, "collection": collection, "rkey": rkey })).service(repo)
}

pub fn sw(kind: &str, name: &str) -> Value {
    json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": kind, "name": name })
}

/// Before an apply: every ssh public key in a templated spec is THIS run's (from the host), and there is one. A key
/// baked into a file (an earlier run's) or a placeholder left in is refused: the guest would refuse our ssh.
pub fn keys_are(spec: &str, key: &str) -> Result<(), String> {
    let key = key.trim();
    if key.is_empty() || !key.starts_with("ssh-") {
        return Err("the run has no ssh public key from the host".into());
    }
    let mut n = 0;
    for l in spec.lines() {
        let t = l.trim().trim_start_matches("- ").trim().trim_matches(|c| c == '"' || c == '\'');
        if ["ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-", "sk-ssh-"].iter().any(|p| t.starts_with(p)) {
            if t != key {
                return Err(format!("the spec carries an ssh key that is not this run's: {}", t.chars().take(40).collect::<String>()));
            }
            n += 1;
        }
    }
    if n == 0 {
        return Err("the spec carries no ssh key".into());
    }
    Ok(())
}

pub struct S {
    pub st: Value,
    pub now: f64,
}

impl S {
    pub fn new(state: Value, now_iso: &str) -> Self {
        let now = secs(now_iso);
        let st = if state.is_null() { json!({ "phase": "start", "t0": now, "readings": {}, "f": {} }) } else { state };
        Self { st, now }
    }

    pub fn phase(&self) -> String {
        self.st["phase"].as_str().unwrap_or_default().to_string()
    }

    pub fn is(&self, p: &str) -> bool {
        self.st["phase"] == p
    }

    pub fn set(&mut self, p: &str) {
        self.st["phase"] = json!(p);
    }

    /// Seconds since the run started.
    pub fn t(&self) -> f64 {
        ((self.now - self.st["t0"].as_f64().unwrap_or(self.now)) * 10.0).round() / 10.0
    }

    pub fn mark(&mut self, k: &str) {
        self.st["marks"][k] = json!(self.now);
    }

    /// Seconds since mark `k`.
    pub fn since(&self, k: &str) -> f64 {
        ((self.now - self.st["marks"][k].as_f64().unwrap_or(self.now)) * 10.0).round() / 10.0
    }

    pub fn reading(&mut self, k: &str, v: impl Into<Value>) {
        self.st["readings"][k] = v.into();
    }

    pub fn flag(&mut self, k: &str) {
        self.st["f"][k] = json!(true);
    }

    pub fn has(&self, k: &str) -> bool {
        self.st["f"][k] == true
    }

    pub fn fail(&self, error: impl Into<String>) -> Handle {
        Handle::new().result(json!({ "pass": false, "error": error.into(), "phase": self.st["phase"], "readings": self.st["readings"] })).done()
    }

    pub fn pass(&mut self) -> Handle {
        let t = self.t();
        self.reading("seconds", t);
        Handle::new().result(json!({ "pass": true, "readings": self.st["readings"] })).done()
    }

    pub fn skip(&self, why: &str) -> Handle {
        Handle::new().result(json!({ "pass": true, "readings": { "skipped": why } })).done()
    }

    pub fn keep(&self, h: Handle) -> Handle {
        h.state(self.st.clone())
    }
}

/// A `requester_market_event` message: the requester's line for a market event of a contract this run made, which the
/// host forwards like Held. An OOM kill (guest.oomKill, vm.oomKilled, vm.terminated reason oom) fails the run naming
/// the VM; memory pressure (guest.memoryPressure, vm.memoryPressure) is a warning in readings.memory. Call it first.
pub fn market_event(s: &mut S, event: &Event) -> Option<Handle> {
    let Event::Message { id, data, .. } = event else {
        return None;
    };
    let e = data["event"].as_str().unwrap_or_default();
    let kill = data["kill"] == true;
    if data["msg"] != "requester_market_event" || (!kill && !e.ends_with("memoryPressure")) {
        return None;
    }
    // the invocation's instance names the VM's run (a contract is "contract" in its own workspace)
    let vm = [&data["instance"], &data["contract"]].iter().find_map(|v| v.as_str()).unwrap_or(id).to_string();
    let entry = json!({ "event": e, "vm": vm, "kill": kill, "payload": data["payload"] });
    match s.st["readings"]["memory"].as_array_mut() {
        Some(a) => a.push(entry),
        None => s.st["readings"]["memory"] = json!([entry]),
    }
    kill.then(|| s.fail(format!("OOM: {e} on VM {vm}: {}", data["payload"])))
}

/// A call's output, or the run's failure naming the call.
pub fn out(s: &S, id: &str, r: Result<&Value, String>) -> Result<Value, Handle> {
    r.map(Value::clone).map_err(|e| s.fail(format!("{id}: {e}")))
}

/// Ready pods (name, node) of a pods list.
pub fn ready_pods(list: &Value) -> Vec<(String, String)> {
    list["items"]
        .as_array()
        .into_iter()
        .flatten()
        .filter(|p| p["status"]["conditions"].as_array().is_some_and(|c| c.iter().any(|c| c["type"] == "Ready" && c["status"] == "True")))
        .map(|p| (p["metadata"]["name"].as_str().unwrap_or_default().to_string(), p["spec"]["nodeName"].as_str().unwrap_or_default().to_string()))
        .collect()
}

pub fn names(list: &Value) -> Vec<String> {
    list["items"].as_array().into_iter().flatten().filter_map(|o| o["metadata"]["name"].as_str().map(String::from)).collect()
}
