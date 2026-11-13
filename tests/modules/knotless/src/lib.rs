// A staging network's knotless (dev up --with-knotless) as a state machine: seed the runner's composition and
// bindings in the account's repo (the gateway owner, the runner's account), then each step a script against the
// staging knotless (knotless CLI, git, the network's env), each failing on what it checks. Entries:
//   push      push a workflow to a fresh repo: the runner rents a network bidder's VM, the check lands success
//   after     build keeps an artifact, test (after: [build]) reads it with $KNOTLESS_ARTIFACTS_URL/TOKEN in its VM,
//             `knotless run artifacts --out` downloads it
//   executor  two workflows on two bindings (executor: small | big): each VM has its binding's vCPUs
//   pull      a sealed secret: the push to main sees it, a pull's run gets none
//   terminated a step powers off its own VM: its bidder's vm.terminated reaches the cicd workload as a market event
//             and the step fails at once with the reason (not after the read retries)
// Result {pass, error?, phase (the step), readings (seconds per step)}. The host deletes the repo and the seeded
// records however it ends; the runner releases each VM.
use abi::{Event, Handle, Run, entries};
use serde_json::{Value, json};
use steps::{MIN, S, host};

struct Step {
    id: &'static str,
    script: String,
}

const CI: &str = "on:\n  push:\n    refs: [refs/heads/main]\nsteps:\n  - name: where\n    run: uname -a && nproc && git log --oneline -1\n  - name: test\n    run: test \"$(cat answer.txt)\" = 42\n";
const BUILD: &str = "on:\n  push:\n    refs: [refs/heads/main]\nartifacts: [out/hello.txt]\nsteps:\n  - name: build\n    run: mkdir -p out && echo \"hello $KNOTLESS_RUN_SHA\" > out/hello.txt\n";
const TEST: &str = "on:\n  push:\n    refs: [refs/heads/main]\nafter: [build]\nsteps:\n  - name: fetch\n    run: |\n      curl -sSfo got.txt -H \"Authorization: Bearer $KNOTLESS_ARTIFACTS_TOKEN\" \"$KNOTLESS_ARTIFACTS_URL?workflow=build&name=out/hello.txt\" \n      test \"$(cat got.txt)\" = \"hello $KNOTLESS_RUN_SHA\"\n";
const SMALL: &str = "on:\n  push:\n    refs: [refs/heads/main]\nexecutor: small\nsteps:\n  - name: size\n    run: nproc && test \"$(nproc)\" = 1\n";
const BIG: &str = "on:\n  push:\n    refs: [refs/heads/main]\nexecutor: big\nsteps:\n  - name: size\n    run: nproc && test \"$(nproc)\" = 2\n";
const POWEROFF: &str = "on:\n  push:\n    refs: [refs/heads/main]\nsteps:\n  - name: poweroff\n    run: |\n      nohup sh -c 'sleep 20; echo 1 > /proc/sys/kernel/sysrq; echo o > /proc/sysrq-trigger' > /dev/null 2>&1 &\n      sleep 1200\n";
const SECRET: &str = "on:\n  push:\n    refs: [refs/heads/main]\n  pull:\n    base: [refs/heads/main]\nsecrets: [STAGING_SECRET]\nsteps:\n  - name: secret\n    run: if [ -n \"${KNOTLESS_RUN_PULL:-}\" ]; then test -z \"${STAGING_SECRET:-}\" && echo pull-has-no-secret; else test \"$STAGING_SECRET\" = staging-secret-value && echo push-has-the-secret; fi\n";

/// Repo `r`, then the wait until the runner watches it: a runner acts on no ref, pull or rerun it found already there.
// ponytail: the appview listing it, then 3 of the runner's 10 s discovery sweeps; a runner-side "watching" read when
// one exists.
fn create(r: &str) -> String {
    format!(
        "knotless repo create knotless://$KNOTLESS_IDENTIFIER/{r} --description 'swc-test knotless'\nfor i in $(seq 60); do curl -sf \"$KNOTLESS_APPVIEW/xrpc/dev.knotless.listRepos?runner=$KNOTLESS_IDENTIFIER\" | grep -q '\"name\":\"{r}\"' && break; sleep 2; done\ncurl -sf \"$KNOTLESS_APPVIEW/xrpc/dev.knotless.listRepos?runner=$KNOTLESS_IDENTIFIER\" | grep -q '\"name\":\"{r}\"'\nsleep 30\n"
    )
}

/// Repo `r` with `answer.txt` and the workflows, pushed to main; its commit in `<r>.sha`.
fn push_script(r: &str, workflows: &[(&str, &str)]) -> String {
    let mut s = format!(
        "U=knotless://$KNOTLESS_IDENTIFIER/{r}\nrm -rf {r} && mkdir -p {r}/.knotless/workflows && cd {r}\ngit init -q -b main\necho 42 > answer.txt\n"
    );
    for (name, body) in workflows {
        s.push_str(&format!(
            "cat > .knotless/workflows/{name}.yml <<'EOF'\n{body}EOF\n"
        ));
    }
    s.push_str("git add . && git commit -q -m 'swc-test knotless'\ngit remote add origin \"$U\"\ngit push -q -u origin main\ngit rev-parse HEAD > ../");
    s.push_str(&format!("{r}.sha\ncat ../{r}.sha\n"));
    s
}

/// Waits for every check of the commit in `<r>.sha` (or the pull in `<r>.pull`), then each named workflow's must be
/// success; their logs on a failure.
fn checks(r: &str, of: &str, workflows: &[&str]) -> String {
    let sel = match of {
        "pull" => format!("--pull \"$(cat {r}.pull)\""),
        _ => format!("--commit \"$(cat {r}.sha)\""),
    };
    let want: Vec<String> = workflows
        .iter()
        .map(|w| format!("grep -q \"^success\t{w}\t\" {r}.{of}.checks"))
        .collect();
    format!(
        "U=knotless://$KNOTLESS_IDENTIFIER/{r}\nknotless run list \"$U\" {sel} --wait 1500 > {r}.{of}.checks\ncat {r}.{of}.checks\n{{ {}; }} || {{ knotless run list \"$U\" {sel} --log; exit 1; }}\n",
        want.join(" && ")
    )
}

fn plan(entry: &str, r: &str) -> (Value, Vec<Step>) {
    let one = json!({ "cpus": 1, "mem": 1 });
    match entry {
        "after" => (
            json!({ "self": one }),
            vec![
                Step {
                    id: "create",
                    script: create(r),
                },
                Step {
                    id: "push",
                    script: push_script(r, &[("build", BUILD), ("test", TEST)]),
                },
                Step {
                    id: "checks",
                    script: checks(r, "push", &["build", "test"]),
                },
                Step {
                    id: "download",
                    script: format!(
                        "U=knotless://$KNOTLESS_IDENTIFIER/{r}\nrm -rf {r}.got\nknotless run artifacts \"$U\" --workflow build --commit \"$(cat {r}.sha)\"\nknotless run artifacts \"$U\" --workflow build --commit \"$(cat {r}.sha)\" --out {r}.got\nfind {r}.got -type f\ntest \"$(cat \"$(find {r}.got -name hello.txt)\")\" = \"hello $(cat {r}.sha)\"\n"
                    ),
                },
            ],
        ),
        "executor" => (
            json!({ "self": one, "small": one, "big": { "cpus": 2, "mem": 2 } }),
            vec![
                Step {
                    id: "create",
                    script: create(r),
                },
                Step {
                    id: "push",
                    script: push_script(r, &[("small", SMALL), ("big", BIG)]),
                },
                Step {
                    id: "checks",
                    script: checks(r, "push", &["small", "big"]),
                },
            ],
        ),
        "terminated" => (
            json!({ "self": one }),
            vec![
                Step {
                    id: "create",
                    script: create(r),
                },
                Step {
                    id: "push",
                    script: push_script(r, &[("terminated", POWEROFF)]),
                },
                Step {
                    id: "check",
                    script: format!(
                        "U=knotless://$KNOTLESS_IDENTIFIER/{r}\nC=\"$(cat {r}.sha)\"\nknotless run list \"$U\" --commit \"$C\" --wait 1500 > {r}.checks\ncat {r}.checks\nknotless run list \"$U\" --commit \"$C\" --log > {r}.log\ngrep -q \"^failure\tterminated\t\" {r}.checks || {{ cat {r}.log; exit 1; }}\ngrep 'the VM was terminated by its bidder: ' {r}.log || {{ cat {r}.log; exit 1; }}\nms=$(knotless run list \"$U\" --commit \"$C\" --json | jq '[.[] | select(.workflow == \"terminated\")][0].steps[0].durationMs')\necho \"step $ms ms\"\ntest \"$ms\" -lt 300000\n"
                    ),
                },
            ],
        ),
        "pull" => (
            json!({ "self": one }),
            vec![
                Step {
                    id: "create",
                    script: create(r),
                },
                Step {
                    id: "secret",
                    script: format!(
                        "printf staging-secret-value | knotless secret set knotless://$KNOTLESS_IDENTIFIER/{r} STAGING_SECRET\nknotless secret list knotless://$KNOTLESS_IDENTIFIER/{r}\n"
                    ),
                },
                Step {
                    id: "push",
                    script: push_script(r, &[("secret", SECRET)]),
                },
                Step {
                    id: "checks",
                    script: checks(r, "push", &["secret"]),
                },
                Step {
                    id: "open-pull",
                    script: format!(
                        "U=knotless://$KNOTLESS_IDENTIFIER/{r}\ncd {r}\ngit checkout -q -b change\necho 43 > answer.txt\ngit commit -q -am 'a pull'\ngit push -q origin change\nknotless pull create \"$U\" main \"$U\" change 'swc-test pull' -m 'a pull gets no secrets' | tee ../{r}.pull.out\ngrep -o 'at://[^ ]*' ../{r}.pull.out | head -1 > ../{r}.pull\ntest -s ../{r}.pull\n"
                    ),
                },
                Step {
                    id: "pull-checks",
                    script: checks(r, "pull", &["secret"]),
                },
                Step {
                    id: "pull-log",
                    script: format!(
                        "knotless run list knotless://$KNOTLESS_IDENTIFIER/{r} --pull \"$(cat {r}.pull)\" --log | tee {r}.pull.log\ngrep -q pull-has-no-secret {r}.pull.log\n"
                    ),
                },
            ],
        ),
        _ => (
            json!({ "self": one }),
            vec![
                Step {
                    id: "create",
                    script: create(r),
                },
                Step {
                    id: "push",
                    script: push_script(r, &[("ci", CI)]),
                },
                Step {
                    id: "checks",
                    script: checks(r, "push", &["ci"]),
                },
            ],
        ),
    }
}

fn run_entry(entry: &'static str, run: Run<Value>) -> Result<Handle, String> {
    let args = &run.args;
    let repo = format!(
        "{}-{entry}",
        args["name"].as_str().ok_or("name is required")?
    );
    let mut s = S::new(run.state, args["now"].as_str().unwrap_or_default());
    if let Some(h) = steps::market_event(&mut s, &run.event) {
        return Ok(h);
    }
    let (bindings, steps) = plan(entry, &repo);
    let mut h = Handle::new();
    let step = |s: &mut S, h: Handle, i: usize| -> Handle {
        s.st["i"] = json!(i);
        s.set(steps[i].id);
        s.mark("step");
        h.call(host(
            &format!("step-{i}"),
            "knotless.sh",
            json!({ "script": steps[i].script, "timeoutSecs": 1800 }),
        ))
    };
    match run.event {
        Event::Start => {
            s.set("seed");
            s.mark("step");
            let cleanup = format!(
                "knotless repo delete knotless://$KNOTLESS_IDENTIFIER/{repo} --yes || true"
            );
            h = h
                .call(host(
                    "cleanup",
                    "knotless.sh",
                    json!({ "cleanup": cleanup }),
                ))
                .call(host(
                    "seed",
                    "knotless.seed",
                    json!({ "bindings": bindings }),
                ))
                .timer("deadline", 60 * MIN);
        }
        Event::Timer { id } if id == "deadline" => {
            return Ok(s.fail(format!("not done in 60 min (step {})", s.phase())));
        }
        Event::Result { id, result, .. } => {
            let o = match steps::out(&s, &id, result.output()) {
                Ok(o) => o,
                Err(f) => return Ok(f),
            };
            if id == "cleanup" {
                return Ok(s.keep(h));
            }
            let t = s.since("step");
            if id == "seed" {
                s.reading("seed", json!({ "seconds": t, "composition": o["composition"], "modules": o["modules"] }));
                h = step(&mut s, h, 0);
                return Ok(s.keep(h));
            }
            let i = s.st["i"].as_u64().unwrap_or_default() as usize;
            if o["exit"] != 0 {
                let out = o["stdout"].as_str().unwrap_or_default();
                let tail: String = out
                    .chars()
                    .rev()
                    .take(1500)
                    .collect::<Vec<_>>()
                    .into_iter()
                    .rev()
                    .collect();
                return Ok(s.fail(format!(
                    "step {}: exit {}: {} {}",
                    steps[i].id, o["exit"], tail, o["stderr"]
                )));
            }
            s.reading(steps[i].id, t);
            if i + 1 == steps.len() {
                return Ok(s.pass());
            }
            h = step(&mut s, h, i + 1);
        }
        _ => {}
    }
    Ok(s.keep(h))
}

fn push_entry(r: Run<Value>) -> Result<Handle, String> {
    run_entry("push", r)
}

fn after_entry(r: Run<Value>) -> Result<Handle, String> {
    run_entry("after", r)
}

fn executor_entry(r: Run<Value>) -> Result<Handle, String> {
    run_entry("executor", r)
}

fn pull_entry(r: Run<Value>) -> Result<Handle, String> {
    run_entry("pull", r)
}

fn terminated_entry(r: Run<Value>) -> Result<Handle, String> {
    run_entry("terminated", r)
}

entries! { push => push_entry, after => after_entry, executor => executor_entry, pull => pull_entry, terminated => terminated_entry }
