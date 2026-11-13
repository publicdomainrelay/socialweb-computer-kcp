// docs/acceptance/0002-ssh-gateway.md: `ssh -F /dev/null -p 22 <handle>@<gateway>` rents a VM as the user
// (discovery): sign in on the ssh web page, a temporary key on /keys (removed after), a transient command; then
// `atirc agent run <name>` with the agent config on stdin, transient and held (`LC_SESSION`), each joining the owner
// channel and answering an observer's prompt as the guest user; the disconnect ends each.
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
            h = h.call(host("signin", "page.signin", json!({ "url": format!("https://ssh.{ssh}/"), "startApi": true }))).timer("deadline", 40 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 40 min (phase {})", s.phase()))),
        Event::Message { id, data, .. } if id == "obs" => {
            let held = s.is("held");
            let a = agent(held);
            let from = data["nick"] == a.as_str() || data["identity"].as_str().is_some_and(|i| i.starts_with(&format!("{a}.")));
            let text = data["text"].as_str().unwrap_or_default();
            let form = if held { "held" } else { "transient" };
            // the agent JOINs ~18 s before it reads: prompt only after its "online - monitoring atirc" line
            if (s.is("transient") || s.is("held")) && from && text.contains("online") && !s.has(&format!("{form}-joined")) {
                s.flag(&format!("{form}-joined"));
                let t = s.since("session");
                s.st["readings"][form]["join_s"] = json!(t);
                s.mark("prompt");
                h = h.call(host("prompt", "atirc.observerPub", json!({ "nick": obs, "text": format!("{a}: uname -m; id -un") })));
            } else if from && text.contains("x86_64") && text.contains(a.as_str()) && s.has(&format!("{form}-joined")) {
                let t = s.since("prompt");
                s.st["readings"][form]["answer_s"] = json!(t);
                h = h.cancel(format!("agent-{form}"));
                if held {
                    return Ok(s.pass());
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

entries! { run => run_0002 }
