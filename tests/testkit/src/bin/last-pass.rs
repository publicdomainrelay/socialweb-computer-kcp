//! last-pass [--bidders <label>] <readings.json>...
//! last-pass [--bidders <label>] --gate-dir <dir>
//!
//! Puts each passing run on its runbook's row of docs/acceptance/README.md (the "last pass" cell, newest first). A skipped or
//! failing run is printed and left out. The entry names the requester CLI (its version and commit, from the run's readings) and
//! the bidders' version (`--bidders`, what the bidders under test ran: e.g. `23b9c7d9`; `?` when not given), the day, the entry,
//! the target, the run's real duration and the account.
//!
//! A readings file's duration is endedAt - startedAt. `--gate-dir` reads a `tests/gate.sh` output directory: one entry per
//! `<runbook>-<entry>.tsv` (entry, result, seconds, account) with its readings file named in `<runbook>-<entry>.log`, oldest first.

use std::path::{Path, PathBuf};
use testkit::{Value, repo};

fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

/// Seconds (with milliseconds) since the epoch of `2026-11-14T05:18:38.794Z`.
fn epoch(ts: &str) -> Option<f64> {
    let (date, time) = ts.trim_end_matches('Z').split_once('T')?;
    let mut d = date.split('-').map(|p| p.parse::<i64>());
    let (y, m, day) = (d.next()?.ok()?, d.next()?.ok()?, d.next()?.ok()?);
    let mut t = time.split(':');
    let (h, mi) = (t.next()?.parse::<f64>().ok()?, t.next()?.parse::<f64>().ok()?);
    let s = t.next()?.parse::<f64>().ok()?;
    Some(days_from_civil(y, m, day) as f64 * 86400.0 + h * 3600.0 + mi * 60.0 + s)
}

fn seconds_of(r: &Value) -> Option<u64> {
    let (a, b) = (epoch(r["startedAt"].as_str()?)?, epoch(r["endedAt"].as_str()?)?);
    Some((b - a).round().max(0.0) as u64)
}

fn cli_of(r: &Value) -> String {
    let v = r["readings"]["requester"]["version"].as_str().unwrap_or("?");
    let mut w = v.split_whitespace();
    let version = w.next().unwrap_or("?");
    match w.next() {
        Some(commit) => format!("{version} {}", &commit[..commit.len().min(8)]),
        None => version.to_string(),
    }
}

struct Run {
    runbook: String,
    what: String,
    readings: Value,
    seconds: Option<u64>,
    account: Option<String>,
}

fn load(path: &Path) -> Value {
    serde_json::from_slice(&std::fs::read(path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))).unwrap()
}

fn gate_runs(dir: &Path) -> Vec<Run> {
    let mut tsvs: Vec<PathBuf> = std::fs::read_dir(dir)
        .unwrap_or_else(|e| panic!("{}: {e}", dir.display()))
        .filter_map(|e| e.ok().map(|e| e.path()))
        .filter(|p| p.extension().is_some_and(|x| x == "tsv") && p.file_name().is_some_and(|n| n != "results.tsv"))
        .collect();
    tsvs.sort();
    let mut runs = vec![];
    for tsv in tsvs {
        let row = std::fs::read_to_string(&tsv).unwrap_or_default();
        let f: Vec<&str> = row.trim_end_matches('\n').split('\t').collect();
        if f.len() < 6 || f[1] != "PASS" {
            continue;
        }
        let log = std::fs::read_to_string(tsv.with_extension("log")).unwrap_or_default();
        let Some(path) = log.lines().find_map(|l| l.strip_prefix("readings: ")) else {
            println!("{}: no readings file in its log, not written", f[0]);
            continue;
        };
        let readings = load(Path::new(path.trim()));
        runs.push(Run {
            runbook: f[0].split(':').next().unwrap_or("").to_string(),
            what: f[0].to_string(),
            readings,
            seconds: f[2].parse().ok(),
            account: Some(f[5].to_string()).filter(|a| !a.is_empty()),
        });
    }
    runs.sort_by(|a, b| a.readings["startedAt"].as_str().cmp(&b.readings["startedAt"].as_str()));
    runs
}

fn main() {
    let readme = repo().join("docs/acceptance/README.md");
    let mut text = std::fs::read_to_string(&readme).unwrap();
    let mut bidders = "?".to_string();
    let mut runs: Vec<Run> = vec![];
    let mut args = std::env::args().skip(1);
    while let Some(a) = args.next() {
        match a.as_str() {
            "--bidders" => bidders = args.next().expect("--bidders <label>"),
            "--gate-dir" => runs.extend(gate_runs(Path::new(&args.next().expect("--gate-dir <dir>")))),
            path => {
                let r = load(Path::new(path));
                runs.push(Run {
                    runbook: r["runbook"].as_str().unwrap().to_string(),
                    what: r["test"].as_str().unwrap().to_string(),
                    seconds: seconds_of(&r),
                    account: None,
                    readings: r,
                });
            }
        }
    }
    for run in runs {
        let r = &run.readings;
        if !r["readings"]["skipped"].is_null() {
            println!("{} {}: skipped ({}), not written", run.runbook, run.what, r["readings"]["skipped"]);
            continue;
        }
        if r["pass"] != true {
            println!("{} {}: FAIL ({}), not written", run.runbook, run.what, r["error"]);
            continue;
        }
        let seconds = run.seconds.or_else(|| seconds_of(r)).map_or("?".to_string(), |s| s.to_string());
        let entry = format!(
            "CLI {}, bidders {}, {} (`{}` vs {}, {} s{})",
            cli_of(r),
            bidders,
            &r["startedAt"].as_str().unwrap()[..10],
            run.what,
            r["target"].as_str().unwrap(),
            seconds,
            run.account.as_deref().map(|a| format!(", {a}")).unwrap_or_default()
        );
        let row_start = format!("| [{}]", run.runbook);
        let Some(i) = text.find(&row_start) else {
            println!("{}: no row in {}", run.runbook, readme.display());
            continue;
        };
        let end = i + text[i..].find('\n').unwrap();
        // the last cell: between the second-to-last and the last '|'
        let row = &text[i..end];
        let last = row.trim_end().rfind('|').unwrap();
        let cell = row[..last].rfind('|').unwrap();
        let old = row[cell + 1..last].trim();
        let old = if old == "not yet run" { "" } else { old };
        let new_row = format!("{}| {}{} |", &row[..cell], entry, if old.is_empty() { String::new() } else { format!("; {old}") });
        text.replace_range(i..end, &new_row);
        println!("{}: {entry}", run.runbook);
    }
    std::fs::write(&readme, text).unwrap();
}
