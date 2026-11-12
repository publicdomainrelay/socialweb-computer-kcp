//! last-pass <readings.json>...: put each passing run on its runbook's row of docs/acceptance/README.md
//! (the "last pass" cell, newest first). A failing run is printed and left out.

use testkit::{Value, repo};

fn main() {
    let readme = repo().join("docs/acceptance/README.md");
    let mut text = std::fs::read_to_string(&readme).unwrap();
    for path in std::env::args().skip(1) {
        let r: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        let runbook = r["runbook"].as_str().unwrap();
        if r["pass"] != true {
            println!("{runbook} {}: FAIL ({}), not written", r["test"], r["error"]);
            continue;
        }
        let version = r["readings"]["requester"]["version"].as_str().unwrap_or("?").split_whitespace().take(2).collect::<Vec<_>>().join(" ");
        let entry = format!("{}, {} (system test `{}` vs {}, {} s)", version, &r["startedAt"].as_str().unwrap()[..10], r["test"].as_str().unwrap(), r["target"].as_str().unwrap(), r["seconds"]);
        let row_start = format!("| [{runbook}]");
        let Some(i) = text.find(&row_start) else {
            println!("{runbook}: no row in {}", readme.display());
            continue;
        };
        let end = i + text[i..].find('\n').unwrap();
        // the last cell: between the second-to-last and the last '|'
        let row = &text[i..end];
        let last = row.trim_end().rfind('|').unwrap();
        let cell = row[..last].rfind('|').unwrap();
        let new_row = format!("{}| {}; {} |", &row[..cell], entry, row[cell + 1..last].trim());
        text.replace_range(i..end, &new_row);
        println!("{runbook}: {entry}");
    }
    std::fs::write(&readme, text).unwrap();
}
