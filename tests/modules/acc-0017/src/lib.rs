// docs/acceptance/0017-owner-release.md: the owner `run request --hold` (on a pseudo-terminal, so it opens and holds an
// interactive ssh session into its guest, no --exec) and a SEPARATE `run request release <name>` process. run: the release
// returns Released within 60 s, and the owner exits only after the contract is Released (its summary says Released and
// kcpRunning false at its exit). killed: the owner is SIGKILLed 2 s after the release was asked; the release client takes
// the invocation over and still settles within 60 s. A release that waits out its timeout on an owner that stopped its
// kcp (main before the fix: 27 minutes) fails here at 90 s.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, host_sub};

const RELEASE_MAX_S: f64 = 60.0;

fn phase_of(summary: &Value) -> String {
    summary["contract"]["phase"].as_str().or(summary["phase"].as_str()).unwrap_or_default().to_lowercase()
}

fn run_with(run: Run<Value>, killed: bool, kill_ms: u64) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            h = h
                .call(host_sub("req", "request.start", json!({ "name": name, "pty": true, "args": ["--hold", "--expires-in", "1h"] })))
                .timer("deadline", 25 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 25 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "settle" && s.is("held") && s.has("held") && s.has("sshing") && !s.has("exited") => {
            s.set("release");
            s.mark("release");
            h = h.call(host("release", "request.release", json!({ "name": name }))).timer("relguard", 90_000);
            if killed {
                h = h.timer("kill", kill_ms);
            }
        }
        Event::Timer { id } if id == "settle" => return Ok(s.fail(format!("the owner is not holding an interactive ssh session (held {}, ssh {}, exited {})", s.has("held"), s.has("sshing"), s.has("exited")))),
        Event::Timer { id } if id == "kill" => h = h.call(host("kill", "request.kill", json!({ "name": name }))),
        Event::Timer { id } if id == "relguard" && !s.has("released") => {
            return Ok(s.fail(format!("the release did not return in 90 s after it was asked (owner exited {}, killed {}); main waited out the release timeout on an owner that had stopped its kcp", s.has("exited"), s.has("killed"))));
        }
        Event::Timer { id } if id == "exitguard" && !s.has("exited") => return Ok(s.fail("the owner did not exit within 60 s after the contract was Released")),
        Event::Message { id, data, .. } if id == "req" => match data["msg"].as_str().unwrap_or_default() {
            "contract" if data["phase"] == "Failed" && !s.has("held") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
            "contract" if data["phase"] == "Held" && !s.has("held") => {
                s.flag("held");
                let t = s.t();
                s.reading("held_s", t);
            }
            m if m.starts_with("no --exec and stdin is not a terminal") => return Ok(s.fail("the owner got no terminal on stdin: no interactive session (the pty wrapper did not work)")),
            m if m.starts_with("ssh to the guest over") && !s.has("sshing") => {
                s.flag("sshing");
                h = h.timer("settle", 30_000);
            }
            "ssh returned" if !s.is("release") => return Ok(s.fail(format!("the interactive ssh session ended before the release was asked: {data}"))),
            "exited" => {
                s.flag("exited");
                s.reading("owner_exit", data["code"].clone());
                if !s.is("release") && !s.is("records") {
                    return Ok(s.fail(format!("the owner exited before the release was asked: {data}")));
                }
                if !killed {
                    // the owner exits only after Released: its summary at its exit
                    h = h.call(host("exit_summary", "request.summary", json!({ "name": name })));
                }
            }
            _ => {}
        },
        Event::Closed { id, error } if id == "req" && !s.has("exited") => return Ok(s.fail(format!("the owner's subscription closed before it exited: {}", error.unwrap_or_default()))),
        Event::Result { id, result, .. } => {
            if id == "summary" {
                if let Err(e) = result.output() {
                    return Ok(s.fail(format!("the release returned but the invocation was left unsettled (no summary: {e}): the owner stopped mid-release and the release client did not take the invocation over")));
                }
            }
            let o = match steps::out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "kill" => {
                    s.flag("killed");
                    s.reading("killed_pids", o["killed"].clone());
                }
                "release" => {
                    let secs = s.since("release");
                    s.flag("released");
                    s.reading("release_s", o["seconds"].clone());
                    s.reading("release_asked_to_returned_s", secs);
                    if o["seconds"].as_f64().unwrap_or(f64::MAX) > RELEASE_MAX_S {
                        return Ok(s.fail(format!("the release took {} s (more than {RELEASE_MAX_S})", o["seconds"])));
                    }
                    h = h.call(host("summary", "request.summary", json!({ "name": name }))).timer("exitguard", 60_000);
                }
                "exit_summary" => {
                    let phase = phase_of(&o);
                    s.reading("owner_exit_phase", phase.clone());
                    s.reading("owner_exit_kcp_running", o["kcpRunning"].clone());
                    if phase != "released" || o["kcpRunning"] != false {
                        return Ok(s.fail(format!("the owner exited with the contract in phase {phase:?} and kcpRunning {}: it stopped its kcp before Released", o["kcpRunning"])));
                    }
                    s.flag("exitSummaryOk");
                }
                "summary" => {
                    let phase = phase_of(&o);
                    s.reading("summary_phase", phase.clone());
                    s.reading("summary_kcp_running", o["kcpRunning"].clone());
                    // killedlate: the owner died after the release was acked, while stopping its kcp: its summary is its last word
                    // (kcpRunning true); the second release below is what settles the invocation
                    if phase != "released" || (kill_ms < 1000 && o["kcpRunning"] != false) {
                        return Ok(s.fail(format!("after the release the summary says phase {phase:?}, kcpRunning {}", o["kcpRunning"])));
                    }
                    s.flag("summaryOk");
                    if killed {
                        // a second release of the taken-over invocation only removes it
                        h = h.call(host("release2", "request.release", json!({ "name": name })));
                    }
                }
                "release2" => {
                    s.reading("release2_s", o["seconds"].clone());
                    h = h.call(host("state", "request.state", json!({ "name": name })));
                }
                "state" => {
                    if o["exists"] != false {
                        return Ok(s.fail(format!("after the second release the invocation directory {} is still there", o["dir"])));
                    }
                    s.flag("removedOk");
                }
                "records" => {
                    s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left after the release: {} {}", o["left"], o["errors"])));
                    }
                    return Ok(s.pass());
                }
                _ => {}
            }
        }
        _ => {}
    }
    if s.is("release") && s.has("released") && s.has("summaryOk") && s.has("exited") && (killed || s.has("exitSummaryOk")) && (!killed || s.has("removedOk")) {
        s.set("records");
        h = h.call(host("records", "request.records", json!({ "name": name })));
    }
    Ok(s.keep(h))
}

// half: an invocation whose network application is gone but whose own summary does NOT say its contract ended must not be
// "released" by a release command: it fails naming the phase and keeps the state. Made from the test's own invocation: a
// normal release (application removed, summary Released), then its summary phase rewritten to Held.
fn half_0017(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            h = h.call(host_sub("req", "request.start", json!({ "name": name, "args": ["--hold", "--exec", "id"] }))).timer("deadline", 20 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 20 min (phase {})", s.phase()))),
        Event::Message { id, data, .. } if id == "req" => match data["msg"].as_str().unwrap_or_default() {
            "contract" if data["phase"] == "Failed" && !s.has("held") => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
            "contract" if data["phase"] == "Held" => s.flag("held"),
            "ssh returned" => {
                if data["exit"] != 0 {
                    return Ok(s.fail(format!("id over ssh: {data}")));
                }
                s.flag("ssh");
            }
            "exited" => {
                s.flag("exited");
                if !s.is("held") && !s.is("released") {
                    return Ok(s.fail(format!("the owner exited before the release was asked: {data}")));
                }
            }
            _ => {}
        },
        Event::Result { id, result, .. } => {
            let o = match steps::out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "release" => {
                    s.flag("released");
                    h = h.call(host("state1", "request.state", json!({ "name": name })));
                }
                "state1" => {
                    let phase = phase_of(&o["summary"]);
                    if o["exists"] != true || phase != "released" {
                        return Ok(s.fail(format!("after the normal release the invocation directory exists {} with phase {phase:?}", o["exists"])));
                    }
                    s.flag("state1Ok");
                }
                "setphase" => h = h.call(host("try", "request.releaseTry", json!({ "name": name }))),
                "try" => {
                    let out = o["output"].as_str().unwrap_or_default().to_string();
                    s.reading("half_release_output", out.clone());
                    if o["ok"] == true {
                        return Ok(s.fail(format!("a release of an invocation whose state says Held and whose application is gone exited OK: {out}")));
                    }
                    if !out.contains("Held") {
                        return Ok(s.fail(format!("the failed release does not name the phase (Held): {out}")));
                    }
                    h = h.call(host("state2", "request.state", json!({ "name": name })));
                }
                "state2" => {
                    if o["exists"] != true {
                        return Ok(s.fail("the failed release removed the invocation directory: a VM may still be held"));
                    }
                    h = h.call(host("remove", "request.removeState", json!({ "name": name })));
                }
                "remove" => return Ok(s.pass()),
                _ => {}
            }
        }
        Event::Closed { id, error } if id == "req" && !s.has("exited") => return Ok(s.fail(format!("the owner's subscription closed before it exited: {}", error.unwrap_or_default()))),
        _ => {}
    }
    if s.is("held") && s.has("held") && s.has("ssh") && !s.has("released") && !s.has("releasing") {
        s.flag("releasing");
        h = h.call(host("release", "request.release", json!({ "name": name })));
    }
    if s.has("released") && s.has("exited") && s.has("state1Ok") && !s.has("edited") {
        s.flag("edited");
        h = h.call(host("setphase", "request.setPhase", json!({ "name": name, "phase": "Held" })));
    }
    Ok(s.keep(h))
}

fn run_0017(run: Run<Value>) -> Result<Handle, String> {
    run_with(run, false, 0)
}

fn killed_0017(run: Run<Value>) -> Result<Handle, String> {
    run_with(run, true, 100)
}

// killedlate: the owner is killed 2 s after the release was asked, while it is stopping its kcp after Released (the release
// client then sees the owner go): the invocation it leaves must still end with the summary Released, kcpRunning false
fn killedlate_0017(run: Run<Value>) -> Result<Handle, String> {
    run_with(run, true, 2000)
}

entries! { run => run_0017, killed => killed_0017, killedlate => killedlate_0017, half => half_0017 }
