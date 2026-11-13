// docs/acceptance/0016-vm-image.md: a VM rented with a requester-chosen image (input image {url, sha256 of its image.json}
// and marker) boots that image's kernel and rootfs: the contract is Held only after the requester matched the guest's
// reported marker with the image.json's, /etc/swc-image in the guest is the marker, and the baked toolchain answers; then
// the release and every record gone. `badsha`: the same with the sha256's first digit changed: no bidder takes the RFP
// (it refuses before it bids), so the contract fails before any receipt, nothing is held and nothing is booted.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host};

const PROBE: &str = r#"echo "MARKER $(cat /etc/swc-image)"; . /opt/ci/env; echo "GO $(go version)"; echo "DOCKER $(command -v dockerd)"; echo "DENO $(deno --version | head -1)"; echo "CARGO $(cargo --version)""#;

fn flipped(sha: &str) -> String {
    let mut c = sha.chars();
    let first = if c.next() == Some('0') { '1' } else { '0' };
    format!("{first}{}", c.as_str())
}

fn image_of(args: &Value, bad: bool) -> Option<(String, String, String)> {
    let url = args["image"]["url"].as_str()?.to_string();
    let sha = args["image"]["sha256"].as_str()?.to_string();
    let marker = args["marker"].as_str().unwrap_or_default().to_string();
    Some((url, if bad { flipped(&sha) } else { sha }, marker))
}

fn run_with(run: Run<Value>, bad: bool) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let Some((url, sha, marker)) = image_of(args, bad) else {
        return Ok(s.skip("the input names no image {url, sha256} (deploy/vm-images/ci-toolchain.json)"));
    };
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            let a = json!(["--hold", "--cpus", "2", "--mem", "4G", "--expires-in", "1h", "--image", format!("{url}@{sha}")]);
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a }))).timer("deadline", 50 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 50 min (phase {})", s.phase()))),
        Event::Message { id, data, .. } if id == "req" => {
            if s.st["receipt"].is_null() && data["receipt"].is_string() {
                s.st["receipt"] = data["receipt"].clone();
            }
            match data["msg"].as_str().unwrap_or_default() {
                "requester_guest_image" if !bad => {
                    s.reading("guest_image", json!({ "marker": data["marker"], "expected": data["expected"], "ok": data["ok"] }));
                    if data["ok"] != true || data["marker"] != marker.as_str() {
                        return Ok(s.fail(format!("the guest reported a marker the requester did not accept: {data}")));
                    }
                    s.flag("imageOk");
                }
                "contract" if bad && data["phase"] == "Failed" => {
                    if !s.st["receipt"].is_null() || s.has("held") {
                        return Ok(s.fail(format!("a VM was rented with a wrong image sha256: {data}")));
                    }
                    s.reading("refused", json!({ "message": data["message"] }));
                    s.reading("refused_s", s.t());
                    return Ok(s.pass());
                }
                "contract" if bad && data["phase"] == "Held" => return Ok(s.fail("the contract was Held with a wrong image sha256")),
                "exited" if bad && !s.has("held") => return Ok(s.fail(format!("run request exited without failing the contract: {data}"))),
                "contract" if !bad && data["phase"] == "Failed" => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                "contract" if !bad && data["phase"] == "Held" => s.flag("held"),
                "exited" if !bad && !s.has("releasing") => return Ok(s.fail(format!("run request exited before the release: {data}"))),
                _ => {}
            }
            if !bad && s.is("held") && s.has("held") && s.has("imageOk") && s.st["receipt"].is_string() {
                s.reading("held_s", s.t());
                s.set("probe");
                h = h.call(host("probe", "request.ssh", json!({ "name": name, "exec": PROBE })));
            }
        }
        Event::Result { id, result, .. } => {
            let o = match steps::out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "probe" => {
                    let text = o["output"].as_str().unwrap_or_default();
                    let line = |k: &str| text.lines().find_map(|l| l.strip_prefix(k)).map(|v| v.trim().to_string()).unwrap_or_default();
                    let guest = line("MARKER ");
                    s.reading("guest_marker", guest.clone());
                    s.reading("toolchain", json!({ "go": line("GO "), "dockerd": line("DOCKER "), "deno": line("DENO "), "cargo": line("CARGO ") }));
                    if guest != marker {
                        return Ok(s.fail(format!("/etc/swc-image in the guest is {guest:?}, not {marker:?}")));
                    }
                    if !line("GO ").contains("go1.") || !line("CARGO ").starts_with("cargo ") {
                        return Ok(s.fail(format!("the baked toolchain does not answer: {text}")));
                    }
                    s.set("release");
                    s.flag("releasing");
                    h = h.call(host("release", "request.release", json!({ "name": name })));
                }
                "release" => {
                    s.reading("release_s", o["seconds"].clone());
                    h = h.call(host("records", "request.records", json!({ "name": name })));
                }
                "records" => {
                    s.reading("records_checked", o["uris"].as_array().map_or(0, Vec::len));
                    h = h.call(host("gone", "assert.recordsGone", json!({ "uris": o["uris"] })));
                }
                "gone" => {
                    if o["left"].as_array().is_none_or(|l| !l.is_empty()) {
                        return Ok(s.fail(format!("records left: {} {}", o["left"], o["errors"])));
                    }
                    return Ok(s.pass());
                }
                _ => {}
            }
        }
        Event::Closed { id, error } if id == "req" && !s.has("releasing") && !bad => return Ok(s.fail(format!("req closed: {}", error.unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}

fn run_0016(run: Run<Value>) -> Result<Handle, String> {
    run_with(run, false)
}

fn badsha_0016(run: Run<Value>) -> Result<Handle, String> {
    run_with(run, true)
}

entries! { run => run_0016, badsha => badsha_0016 }
