// docs/acceptance/0018-downloads-integrity.md: the downloads a bidder's GuestControlPlane and the web app serve are
// content-addressed by a build record embedded in the binary (internal/assets/built.go), not by a versions.json pin. The
// host runs the target's own binary three times against a tampered copy of what it extracts (assets.tamper):
//   file         one byte of a download flipped, its sidecar untouched: `dev verify-assets` on the same asset cache succeeds
//                and the file and sidecar are the recorded bytes again (the tree no longer matched its record: it was
//                replaced from the embedded blob, never used)
//   fileSidecar  the file flipped AND its sidecar rewritten to match (the loaders alone accept that): healed the same way
//   record       one digit of the embedded record flipped in a copy of the binary: it refuses (the blob unpacks to another
//                tree than its record names) and extracts nothing
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host, out};

const VARIANTS: [&str; 3] = ["file", "fileSidecar", "record"];

fn run_0018(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            if args["target"] == "prod" {
                return Ok(s.skip("staging only: it extracts the binary's assets and tampers them on the test host"));
            }
            s.st["left"] = json!(VARIANTS.len());
            for v in VARIANTS {
                h = h.call(host(v, "assets.tamper", json!({ "variant": v })));
            }
            h = h.timer("deadline", 10 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail("not done in 10 min")),
        Event::Result { id, result, .. } => {
            let o = match out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            s.reading(&id, o.clone());
            let ok = match id.as_str() {
                "file" | "fileSidecar" => o["tampered"] == true && o["healed"] == true && o["verifyOk"] == true,
                "record" => o["recordFlipped"] == true && o["refused"] == true && o["extractedNothing"] == true,
                _ => true,
            };
            if !ok {
                return Ok(s.fail(format!("{id}: {o}")));
            }
            let left = s.st["left"].as_u64().unwrap_or(1) - 1;
            s.st["left"] = json!(left);
            if left == 0 {
                return Ok(s.pass());
            }
        }
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0018 }
