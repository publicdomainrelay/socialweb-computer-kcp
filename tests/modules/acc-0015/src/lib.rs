// docs/acceptance/0015-vm-memory.md: this run's own VM fills ~95% of its RAM (incompressible) and writes with fsync for
// 5 min; the VM lives through it: its contract stays Held, no vm.terminated, the guest never reboots, its ComputeVM is not
// Lost (OOMKilled). Then it is released and every record is gone. Prod and staging (it touches only its own VM).
use abi::{Call, Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, VM_TERMINATED, get_record, host, kube, out, termination_rkey};

/// The host's edge-triggered memory events (optional: older bidders write none).
const PRESSURE: &str = "com.publicdomainrelay.temp.compute.events.vm.memoryPressure";
const OOM_KILLED: &str = "com.publicdomainrelay.temp.compute.events.vm.oomKilled";

/// In the guest, detached: the hog holds RAM down to (100 - pct)% available, a 64 MiB fsync'd write loops beside it for
/// `secs`; one status line per write. `$1` secs, `$2` pct.
const FILL: &str = r#"#!/bin/sh
D=$1; P=$2; S=/var/tmp/memfill.status
echo "boot $(cat /proc/sys/kernel/random/boot_id)" > $S
total=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
avail=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo)
fill=$(( avail - total * (100 - P) / 100 ))
echo "plan totalKiB=$total availKiB=$avail fillKiB=$fill" >> $S
python3 -c '
import os, sys, time
n = int(sys.argv[1]) * 1024
hold, got = [], 0
while got < n:
    c = min(64 << 20, n - got)
    hold.append(os.urandom(c))
    got += c
time.sleep(float(sys.argv[2]))
' $fill $D &
hog=$!
end=$(( $(date +%s) + D )); n=0
while [ $(date +%s) -lt $end ]; do
  dd if=/dev/urandom of=/var/tmp/memfill.fsync bs=1M count=64 conv=fsync status=none && n=$((n+1))
  echo "tick $(date +%s) availKiB=$(awk '/^MemAvailable:/{print $2}' /proc/meminfo) fsyncs=$n hog=$(kill -0 $hog 2>/dev/null && echo up || echo gone)" >> $S
done
wait $hog; echo "hogexit $?" >> $S
rm -f /var/tmp/memfill.fsync
echo "done fsyncs=$n" >> $S
"#;

/// One line the module parses: the boot id now, then the status file's sums.
const PROBE: &str = r#"echo "PROBE $(cat /proc/sys/kernel/random/boot_id) $(awk '
/^boot /{b=$2} /^plan /{split($2,t,"=");tot=t[2]}
/^tick /{split($3,a,"=");if(min==""||a[2]+0<min)min=a[2]+0;split($4,f,"=");fs=f[2];if($5=="hog=gone")gone=1}
/^hogexit /{he=$2} /^done /{d=1}
END{printf "boot=%s total=%s min=%s fsyncs=%s hoggone=%d hogexit=%s done=%d", b, tot, min, fs, gone, he, d}' /var/tmp/memfill.status 2>/dev/null) oom=$(dmesg 2>/dev/null | grep -c 'Out of memory' || sudo -n dmesg 2>/dev/null | grep -c 'Out of memory')""#;

fn ssh(id: &str, name: &str, exec: String) -> Call {
    host(id, "request.ssh", json!({ "name": name, "exec": exec }))
}

/// The PROBE line's fields.
fn probe(o: &Value) -> Option<Value> {
    let line = o["output"].as_str()?.lines().find(|l| l.starts_with("PROBE "))?.to_string();
    let mut w = line.split_whitespace().skip(1);
    let mut v = json!({ "bootNow": w.next()? });
    for kv in w {
        if let Some((k, x)) = kv.split_once('=') {
            v[k] = x.parse::<i64>().map(Value::from).unwrap_or(Value::from(x));
        }
    }
    Some(v)
}

fn list(id: &str, repo: &str, collection: &str) -> Call {
    Call::new(id, "com.atproto.repo.listRecords").params(json!({ "repo": repo, "collection": collection, "limit": 100 })).service(repo)
}

fn run_0015(run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let name = args["name"].as_str().ok_or("name")?.to_string();
    let secs = args["seconds"].as_u64().unwrap_or(300);
    let pct = args["pct"].as_u64().unwrap_or(95);
    let winner = s.st["winner"].as_str().unwrap_or_default().to_string();
    let receipt = s.st["receipt"].as_str().unwrap_or_default().to_string();
    let mut h = Handle::new();
    match run.event {
        Event::Start => {
            s.set("held");
            let a = json!(["--hold", "--cpus", args["cpus"].as_str().unwrap_or("2"), "--mem", args["mem"].as_str().unwrap_or("2G"), "--expires-in", "1h"]);
            h = h.call(steps::host_sub("req", "request.start", json!({ "name": name, "args": a }))).timer("deadline", 40 * MIN);
        }
        Event::Timer { id } if id == "deadline" => return Ok(s.fail(format!("not done in 40 min (phase {})", s.phase()))),
        Event::Timer { id } if id == "poll" => {
            h = h
                .call(ssh("probe", &name, PROBE.to_string()))
                .call(get_record("term", &format!("at://{winner}/{VM_TERMINATED}/{}", termination_rkey(&receipt))));
        }
        Event::Message { id, data, .. } if id == "req" => {
            if s.st["receipt"].is_null() && data["receipt"].is_string() {
                s.st["receipt"] = data["receipt"].clone();
            }
            match data["msg"].as_str().unwrap_or_default() {
                "contract" if data["phase"] == "Held" => s.flag("held"),
                // after Held, anything else before our release is the VM lost under the fill
                "contract" if s.has("held") && !s.has("releasing") && data["phase"].is_string() => {
                    return Ok(s.fail(format!("the contract left Held during the fill: {} {}", data["phase"], data["message"])));
                }
                "contract" if data["phase"] == "Failed" => return Ok(s.fail(format!("the contract failed before Held: {}", data["message"]))),
                "exited" if !s.has("releasing") => return Ok(s.fail(format!("run request exited before the release: {data}"))),
                _ => {}
            }
            if s.is("held") && s.has("held") && s.st["receipt"].is_string() {
                let r = s.st["receipt"].as_str().unwrap_or_default().to_string();
                let w = r.trim_start_matches("at://").split('/').next().unwrap_or_default().to_string();
                s.st["winner"] = json!(w);
                s.reading("winner", w.clone());
                s.reading("held_s", s.t());
                s.st["cluster"] = args["bidders"].as_array().into_iter().flatten().find(|b| b["did"] == w.as_str()).map(|b| b["cluster"].clone()).unwrap_or(Value::Null);
                s.set("fill");
                s.mark("fill");
                let start = format!(
                    "cat > /var/tmp/memfill.sh <<'FILL_EOF'\n{FILL}FILL_EOF\nsetsid nohup sh /var/tmp/memfill.sh {secs} {pct} > /var/tmp/memfill.log 2>&1 < /dev/null & sleep 2; cat /var/tmp/memfill.status"
                );
                h = h.call(ssh("fill", &name, start));
            }
        }
        Event::Result { id, result, .. } => {
            let r = result.output();
            if id == "term" {
                if let Ok(o) = r {
                    return Ok(s.fail(format!("vm.terminated during the fill: {}", o["value"])));
                }
                return Ok(s.keep(h));
            }
            if id == "probe" && (r.is_err() || r.as_ref().is_ok_and(|o| o["ok"] != true || probe(o).is_none())) {
                // the guest may answer slowly while it is full: four misses in a row (2 min) fail
                let n = s.st["sshMissRun"].as_u64().unwrap_or(0) + 1;
                s.st["sshMissRun"] = json!(n);
                s.reading("ssh_missed", s.st["readings"]["ssh_missed"].as_u64().unwrap_or(0) + 1);
                if n >= 4 {
                    return Ok(s.fail(format!("the guest did not answer ssh 4 times in a row: {}", r.map(|o| o["output"].to_string()).unwrap_or_else(|e| e))));
                }
                return Ok(s.keep(h.timer("poll", 30_000)));
            }
            // the optional slot: a bidder that writes no memory events (yet) is not a failure
            let r = match (id.starts_with("ev:"), r) {
                (true, Err(e)) => Ok(json!({ "records": [], "error": e })),
                (_, r) => r.cloned(),
            };
            let o = match out(&s, &id, r.as_ref().map_err(String::clone)) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            match id.as_str() {
                "fill" => {
                    let text = o["output"].as_str().unwrap_or_default();
                    let Some(boot) = text.lines().find_map(|l| l.strip_prefix("boot ")) else {
                        return Ok(s.fail(format!("the fill did not start: {text}")));
                    };
                    s.st["boot"] = json!(boot.trim());
                    h = h.timer("poll", 30_000);
                }
                "probe" => {
                    s.st["sshMissRun"] = json!(0);
                    let p = probe(&o).unwrap_or_default();
                    if p["bootNow"] != s.st["boot"] || p["boot"] != s.st["boot"] {
                        return Ok(s.fail(format!("the guest rebooted during the fill: {p} (boot {})", s.st["boot"])));
                    }
                    s.reading("probe", p.clone());
                    if p["done"] != 1 {
                        if s.since("fill") > (secs + 300) as f64 {
                            return Ok(s.fail(format!("the fill did not end in {} s: {p}", secs + 300)));
                        }
                        return Ok(s.keep(h.timer("poll", 30_000)));
                    }
                    let (total, min) = (p["total"].as_i64().unwrap_or(0), p["min"].as_i64().unwrap_or(i64::MAX));
                    let used = if total > 0 { 100 - min * 100 / total } else { 0 };
                    s.reading("peak_used_pct", used);
                    s.reading("fsyncs", p["fsyncs"].clone());
                    s.reading("guest_oom_lines", p["oom"].clone());
                    s.reading("fill_s", s.since("fill"));
                    if used + 3 < pct as i64 {
                        return Ok(s.fail(format!("the fill reached {used}% used, not {pct}%: {p}")));
                    }
                    if p["fsyncs"].as_i64().unwrap_or(0) == 0 {
                        return Ok(s.fail(format!("no fsync'd write completed: {p}")));
                    }
                    s.set("vm");
                    s.st["wait"] = json!(2);
                    // the optional slot: the host's memory events of this VM, read when the bidder writes them
                    h = h.call(list("ev:pressure", &winner, PRESSURE)).call(list("ev:oom", &winner, OOM_KILLED));
                    if let Some(c) = s.st["cluster"].as_str().map(String::from) {
                        s.st["wait"] = json!(3);
                        h = h.call(kube("vms", "list", &c, json!({ "apiVersion": "socialweb.computer/v1alpha1", "kind": "ComputeVM" })));
                    } else {
                        s.reading("computevm", "unchecked: the target names no kcp for the winner");
                    }
                }
                "vms" => {
                    let mine: Vec<&Value> = o["items"].as_array().into_iter().flatten().filter(|v| v["spec"]["contract"]["receipt"]["uri"] == receipt.as_str()).collect();
                    let [vm] = mine.as_slice() else {
                        return Ok(s.fail(format!("{} ComputeVMs of this run's receipt on the winner", mine.len())));
                    };
                    let st = &vm["status"];
                    s.reading("computevm", json!({ "name": vm["metadata"]["name"], "phase": st["phase"], "lostReason": st["lostReason"] }));
                    if st["phase"] != "Running" || st["lostReason"].is_string() {
                        return Ok(s.fail(format!("the ComputeVM is {} ({}) after the fill", st["phase"], st["lostReason"])));
                    }
                }
                "ev:pressure" | "ev:oom" => {
                    // this receipt's reports: rkey `...-payload-<key_hash("<nsid>:<tag>:<receipt>")>`, tag high-n,
                    // max-n (memoryPressure) or oom-n (oomKilled), n 1..10 (MemoryEventCap)
                    let (nsid, kinds, k) = if id == "ev:oom" { (OOM_KILLED, vec!["oom"], "oomKilled") } else { (PRESSURE, vec!["high", "max"], "memoryPressure") };
                    let mine: Vec<String> = kinds.iter().flat_map(|kd| (1..=10).map(move |n| format!("{kd}-{n}"))).map(|t| format!("-payload-{}", steps::key_hash(&format!("{nsid}:{t}:{receipt}")))).collect();
                    let recs = o["records"].as_array().cloned().unwrap_or_default();
                    let ours: Vec<Value> = recs.iter().filter(|r| r["uri"].as_str().is_some_and(|u| mine.iter().any(|m| u.ends_with(m.as_str())))).map(|r| r["value"].clone()).collect();
                    s.reading(k, json!({ "ours": ours, "others": recs.len() - ours.len(), "error": o["error"] }));
                    if id == "ev:oom" && !ours.is_empty() {
                        return Ok(s.fail(format!("the host OOM-killed in this VM's cgroup: {}", json!(ours))));
                    }
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
            if s.is("vm") && ["vms", "ev:pressure", "ev:oom"].contains(&id.as_str()) {
                let left = s.st["wait"].as_u64().unwrap_or(1) - 1;
                s.st["wait"] = json!(left);
                if left == 0 {
                    s.set("release");
                    s.flag("releasing");
                    h = h.call(host("release", "request.release", json!({ "name": name })));
                }
            }
        }
        Event::Closed { id, error } if id == "req" && !s.has("releasing") => return Ok(s.fail(format!("req closed: {}", error.unwrap_or_default()))),
        _ => {}
    }
    Ok(s.keep(h))
}

entries! { run => run_0015 }
