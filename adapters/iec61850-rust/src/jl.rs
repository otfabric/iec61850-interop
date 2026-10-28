// SPDX-License-Identifier: MIT
//
// JSON Lines on stdout (one flushed line per event), logs on stderr.

use std::io::Write;

use serde_json::{json, Value};

/// $ADAPTER_VERSION, or "dev".
pub fn version() -> String {
    std::env::var("ADAPTER_VERSION").unwrap_or_else(|_| "dev".to_string())
}

/// Writes one raw line to stdout and flushes.
pub fn line(s: &str) {
    let mut out = std::io::stdout().lock();
    let _ = out.write_all(s.as_bytes());
    let _ = out.write_all(b"\n");
    let _ = out.flush();
}

/// Writes one JSON value as a line.
pub fn emit(v: &Value) {
    line(&v.to_string());
}

/// Human-readable log line on stderr.
pub fn log(msg: &str) {
    let mut err = std::io::stderr().lock();
    let _ = writeln!(err, "{msg}");
    let _ = err.flush();
}

/// Logs and exits with `code`.
pub fn fatal(code: i32, msg: &str) -> ! {
    log(msg);
    std::process::exit(code)
}

/// JSON string literal of `s` (with quotes).
pub fn q(s: &str) -> String {
    Value::String(s.to_string()).to_string()
}

/// The ready event of the MMS and IED servers.
pub fn ready(addr: &str, fixture: &str, ied_name: &str) {
    line(&format!(
        "{{\"event\":\"ready\",\"address\":{},\"fixture\":{},\"adapter\":{},\"version\":{},\"ied_name\":{}}}",
        q(addr),
        q(fixture),
        q(crate::ADAPTER),
        q(&version()),
        q(ied_name)
    ));
}

/// `--version` / `--capabilities` as argv[1]: print and exit 0.
pub fn handle_meta_flags() {
    let a1 = match std::env::args().nth(1) {
        Some(a) => a,
        None => return,
    };
    let v = version();
    if a1 == "--version" {
        line(&format!(
            "{{\"event\":\"version\",\"adapterVersion\":{},\"fixtureRevision\":\"iec61850-v2\"}}",
            q(&v)
        ));
        std::process::exit(0);
    }
    if a1 == "--capabilities" {
        let cmds: Vec<String> = crate::COMMANDS.iter().map(|c| q(c)).collect();
        line(&format!(
            "{{\"event\":\"capabilities\",\"adapterVersion\":{},\"fixtureRevision\":\"iec61850-v2\",\"commands\":[{}]}}",
            q(&v),
            cmds.join(",")
        ));
        std::process::exit(0);
    }
}

/// `{"event":"error","code":"unsupported","command":..,"reason":..}` and exit 4.
pub fn unsupported_command(command: &str, reason: &str) -> ! {
    emit(&json!({"event":"error","code":"unsupported","command":command,"reason":reason}));
    std::process::exit(4)
}
