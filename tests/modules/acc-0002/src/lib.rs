// docs/acceptance/0002-ssh-gateway.md: `ssh -F /dev/null -p 22 <handle>@<gateway>` rents a VM as the user
// (discovery): sign in on the ssh web page, a temporary key on /keys (removed after), a transient command; then
// `atirc agent run <name>` with the agent config on stdin, transient and held (`LC_SESSION`), each joining the owner
// channel and answering an observer's prompt as the guest user; the disconnect ends each: from the ssh gateway's log
// (target gateway.log) each token revoked before its contract's release, every record deleted, and no market record
// of the account created during the run left in its repo.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, out};

fn run_0002(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let ssh = args["gateway"]["ssh"].as_str().ok_or("gateway.ssh")?.to_string();
    let n = args["name"].as_str().unwrap_or_default().rsplit('-').next().unwrap_or("0").chars().rev().take(5).collect::<String>();
    let obs = format!("stobs{n}");
    let agent = |held: bool| format!("st{}{n}", if held { "h" } else { "t" });
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.st["since"] = args["now"].clone();
            h = h.call(host("signin", "page.signin", json!({ "url": format!("https://ssh.{ssh}/"), "startApi": true }))).timer("deadline", 40 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 40 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "log" => {
            let msgs = ["requester_session_atirc_revoked", "requester_release", "requester_released", "requester_contract_records_deleted"];
            h = h.call(host("log", "gateway.log", json!({ "did": args["did"], "since": s.st["since"], "msgs": msgs })));
        }
        Event::Timer { id } if id == "reprompt" => {
            let form = if s.is("held") { "held" } else { "transient" };
            if s.since("prompt1") > 300.0 {
                return Ok(s.fail(format!("{form}: no answer 5 min after the first prompt")));
            }
            let n = s.st["readings"][form]["prompts"].as_u64().unwrap_or(1) + 1;
            s.st["readings"][form]["prompts"] = json!(n);
            s.mark("prompt");
            h = h.call(host("prompt", "atirc.observerPub", json!({ "nick": obs, "text": format!("{}: uname -m; id -un", agent(s.is("held"))) }))).timer("reprompt", 30_000);
        }
        Event::Message { id, data, .. } if id == "obs" => {
            let held = s.is("held");
            let a = agent(held);
            let from = data["nick"] == a.as_str() || data["identity"].as_str().is_some_and(|i| i.starts_with(&format!("{a}.")));
            let text = data["text"].as_str().unwrap_or_default();
            let form = if held { "held" } else { "transient" };
            // the agent JOINs some 18 s before it reads, and its "online" line is the model's choice (rc165: "Standing
            // by" on stdout only): prompt at the JOIN or that line, again every 30 s until it answers
            if (s.is("transient") || s.is("held")) && from && (text.contains("online") || data["event"] == "join") && !s.has(&format!("{form}-joined")) {
                s.flag(&format!("{form}-joined"));
                let t = s.since("session");
                s.st["readings"][form]["join_s"] = json!(t);
                s.mark("prompt");
                s.mark("prompt1");
                s.st["readings"][form]["prompts"] = json!(1);
                h = h.call(host("prompt", "atirc.observerPub", json!({ "nick": obs, "text": format!("{a}: uname -m; id -un") }))).timer("reprompt", 30_000);
            } else if from && text.contains("x86_64") && text.contains(a.as_str()) && s.has(&format!("{form}-joined")) {
                let t = s.since("prompt");
                s.st["readings"][form]["answer_s"] = json!(t);
                s.st["readings"][form]["answer_after_first_prompt_s"] = json!(s.since("prompt1"));
                h = h.cancel(format!("agent-{form}")).cancel("reprompt");
                if held {
                    // the disconnect: the held session's token is revoked at once, its VM released at the idle timeout
                    // (5 min); then the gateway's log and the repo say how each session ended
                    s.set("ended");
                    s.mark("ended");
                    return Ok(s.keep(h.timer("log", 15_000)));
                }
                s.set("held");
                s.mark("session");
                h = h.call(abi::Call::new("agent-held", "com.fedfork.exec.ssh.session").input(json!({ "dest": "gateway", "command": format!("atirc agent run {}", agent(true)), "stdinFile": "agentConfig", "env": { "LC_SESSION": agent(true) } })).subscribe());
            }
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "log" => {
                    let lines = o["lines"].as_array().cloned().unwrap_or_default();
                    let handle = args["handle"].as_str().unwrap_or_default();
                    let mut done = true;
                    for held in [false, true] {
                        let form = if held { "held" } else { "transient" };
                        match session_end(&lines, &format!("{}.{handle}", agent(held)), held) {
                            Ok(r) => s.st["readings"][form]["end"] = r,
                            Err(e) if s.since("ended") > 480.0 => return Ok(s.fail(format!("{form} session: {e} (8 min after the disconnect)"))),
                            Err(_) => done = false,
                        }
                    }
                    if !done {
                        return Ok(s.keep(h.timer("log", 15_000)));
                    }
                    h = h.call(host("since", "assert.noneSince", json!({ "did": args["did"], "since": s.st["since"] })));
                }
                "since" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) || o["errors"].as_array().is_some_and(|e| !e.is_empty()) {
                        return Ok(s.fail(format!("records of the sessions left: {} {}", o["left"], o["errors"])));
                    }
                    return Ok(s.pass());
                }
                "signin" => h = h.call(host("key", "ssh.tempKey", json!({}))),
                "key" => {
                    s.mark("exec");
                    h = h.call(abi::Call::new("exec", "com.fedfork.exec.ssh.exec").input(json!({ "dest": "gateway", "command": "uname -a; id; hostname" })));
                }
                "exec" => {
                    let out = o["stdout"].as_str().unwrap_or_default();
                    if o["exit"] != 0 || !out.contains("Linux") || !out.contains("uid=") {
                        return Ok(s.fail(format!("transient command: exit {} {}", o["exit"], o["stderr"])));
                    }
                    let t = s.since("exec");
                    s.reading("ssh_s", t);
                    s.set("transient");
                    s.mark("session");
                    h = h.call(steps::host_sub("obs", "atirc.observer", json!({ "nick": obs }))).call(
                        abi::Call::new("agent-transient", "com.fedfork.exec.ssh.session").input(json!({ "dest": "gateway", "command": format!("atirc agent run {}", agent(false)), "stdinFile": "agentConfig" })).subscribe(),
                    );
                }
                _ => {}
            }
        }
        Event::Closed { id, error } if id == "obs" => return Ok(s.fail(format!("the observer stream closed: {}", error.unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}

/// How a session ended, from the ssh gateway's log: its token (`subhandle`) revoked, then its contract (transient
/// `ssh-run-*`, held `ssh-*`) released, and every record of it deleted. Err: not (yet) so.
fn session_end(lines: &[Value], subhandle: &str, held: bool) -> Result<Value, String> {
    let t = |l: &Value| steps::secs(l["time"].as_str().unwrap_or_default());
    let revoked = lines.iter().find(|l| l["msg"] == "requester_session_atirc_revoked" && l["subhandle"] == subhandle).ok_or(format!("{subhandle} not revoked"))?;
    let ours = |c: &str| c.starts_with("ssh-") && c.starts_with("ssh-run-") != held;
    let release = lines
        .iter()
        .find(|l| l["msg"] == "requester_release" && t(l) >= t(revoked) && l["contract"].as_str().is_some_and(ours))
        .ok_or("no release after the revoke")?;
    let contract = release["contract"].as_str().unwrap_or_default();
    let of = |m: &str| lines.iter().find(|l| l["msg"] == m && l["contract"] == contract);
    of("requester_released").ok_or(format!("{contract} not released"))?;
    let del = of("requester_contract_records_deleted").ok_or(format!("{contract}: no records deleted"))?;
    if del["deleted"] != del["of"] {
        return Err(format!("{contract}: {} of {} records deleted", del["deleted"], del["of"]));
    }
    Ok(json!({ "contract": contract, "revokedBeforeRelease_s": ((t(release) - t(revoked)) * 1000.0).round() / 1000.0, "records": format!("{}/{}", del["deleted"], del["of"]), "receipt": release["receipt"] }))
}

entries! { run => run_0002 }
