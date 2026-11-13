// docs/acceptance/0009-browser-page.md A3 / A6: the page's requester module in headless Chrome (page-round.mjs of the
// requester page repo, an app-password session) rents a VM over each bidder's webrtc endpoint (A6 is A3 per bidder),
// a shell over the guest datachannel, the release, nothing it wrote left, no ingress offer left, no https submit.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, out};

fn run_0009(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    let bidders: Vec<Value> = args["bidders"].as_array().cloned().unwrap_or_default();
    let i = s.st["i"].as_u64().unwrap_or(0) as usize;
    let round = |i: usize| host("round", "page.round", json!({ "bidder": bidders[i]["did"] }));
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if bidders.is_empty() {
                return Ok(s.skip("no bidders in the target"));
            }
            h = h.call(round(0)).timer("deadline", (16 * bidders.len() as u64) * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done (round {i})"))),
        Event::Result { id, result, .. } if id == "round" => {
            if result.output().is_err_and(|e| e.contains("pageRepo")) {
                return Ok(s.skip("the target names no pageRepo"));
            }
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            let lines: Vec<String> = o["lines"].as_array().into_iter().flatten().filter_map(|l| l.as_str().map(String::from)).collect();
            let line = |k: &str| lines.iter().find(|l| l.contains(k)).cloned().unwrap_or_default();
            let name = bidders[i]["name"].as_str().unwrap_or_default().to_string();
            let checks = [
                (o["ok"] == true && line("contract").contains("ok"), line("contract")),
                (line("shell").contains("Linux") && line("shell").contains("uid=0"), line("shell")),
                (line("https submit* calls").trim_end().ends_with(" 0"), line("https submit* calls")),
                (!line("deleted, now").contains(",200]"), line("deleted, now")),
                (line("ingress.offer records left").trim_end().ends_with(" 0"), line("ingress.offer records left")),
            ];
            if let Some((_, l)) = checks.iter().find(|(ok, _)| !ok) {
                return Ok(s.fail(format!("{name}: {l}")));
            }
            s.reading(&name, line("contract"));
            if i + 1 == bidders.len() {
                return Ok(s.pass());
            }
            s.st["i"] = json!(i + 1);
            h = h.call(round(i + 1));
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0009 }
