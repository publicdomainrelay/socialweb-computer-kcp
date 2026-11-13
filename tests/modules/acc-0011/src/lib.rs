// docs/acceptance/0011-atirc.md. run: section 2's deploy gates against the target's atirc (plain HTTP): the binary for
// curl, /chat, the root page links it, an unauthenticated revoke is 401. token: section 3 from the client side, a
// token minted the normal way (`atirc token`) posts and sees its own line, and after `atirc revoke` a post is refused.
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{S, host, out};

fn gates(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let Some(base) = args["atirc"].as_str().map(|b| b.trim_end_matches('/').to_string()) else { return Ok(s.skip("the target names no atirc")) };
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.st["fan"] = json!(4);
            h = h
                .call(Call::http("bin", "HEAD", &format!("{base}/"), json!({ "user-agent": "curl/8" }), None))
                .call(Call::http("chat", "GET", &format!("{base}/chat"), json!({}), None))
                .call(Call::http("page", "GET", &format!("{base}/"), json!({ "user-agent": "Mozilla/5.0" }), None))
                .call(Call::http("revoke", "POST", &format!("{base}/xrpc/chat.atirc.revoke"), json!({ "content-type": "application/json" }), Some("{}".into())));
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            let ok = match id.as_str() {
                // the binary (HEAD: 50 MB is past a module's input)
                "bin" => o["status"] == 200 && o["headers"].to_string().contains("octet-stream"),
                "chat" => o["status"] == 200,
                "page" => o["status"] == 200 && o["body"].as_str().is_some_and(|b| b.contains("/chat")),
                "revoke" => o["status"] == 401,
                _ => true,
            };
            if !ok {
                return Ok(s.fail(format!("{id}: HTTP {}", o["status"])));
            }
            let left = s.st["fan"].as_u64().unwrap_or(1) - 1;
            s.st["fan"] = json!(left);
            if left == 0 {
                return Ok(s.pass());
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

fn token_handler(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let nick = format!("sttok{}", args["name"].as_str().unwrap_or_default().chars().rev().take(5).collect::<String>());
    let text = format!("st-0011 {nick}");
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("sub");
            h = h.call(host("obs", "atirc.observer", json!({ "nick": nick })).subscribe()).call(host("pub", "atirc.observerPub", json!({ "nick": nick, "text": text }))).timer("deadline", 120_000);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 2 min (phase {})", s.phase()))),
        Event::Message { id, data, .. } if id == "obs" && data["text"] == text.as_str() && s.is("sub") => {
            s.set("revoke");
            h = h.call(host("revoke", "atirc.observerRevoke", json!({ "nick": nick })));
        }
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "pub" if o["ok"] != true => return Ok(s.fail("a fresh token could not post")),
                "revoke" => h = h.call(host("after", "atirc.observerPub", json!({ "nick": nick, "text": "after revoke" }))),
                "after" if o["ok"] == true => return Ok(s.fail("a revoked token still posts")),
                "after" => return Ok(s.pass()),
                _ => {}
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => gates, token => token_handler }
