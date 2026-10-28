// SPDX-License-Identifier: MIT
//
// Shared code of the iec61850-rust adapter commands: JSON Lines output,
// C printf-style number formatting, the typed value JSON of the GOOSE
// contract, the Sampled Values seqData encoding, and small time helpers.
// The protocol work is done by the iec61850-rust crates; this crate only
// adapts them to the mms-interop command line and output contract.

pub mod ber;
pub mod cfmt;
pub mod ied;
pub mod jl;
pub mod sv;
pub mod timefmt;
pub mod typed;

/// Adapter name reported in ready events.
pub const ADAPTER: &str = "iec61850-rust";

/// The commands of this image, in the order of the other adapters.
pub const COMMANDS: [&str; 10] = [
    "iec61850-rust-mms-server",
    "iec61850-rust-mms-client",
    "iec61850-rust-ied-server",
    "iec61850-rust-ied-client",
    "iec61850-rust-ied-controller",
    "iec61850-rust-ied-reporter",
    "iec61850-rust-goose-publisher",
    "iec61850-rust-goose-subscriber",
    "iec61850-rust-sv-publisher",
    "iec61850-rust-sv-subscriber",
];

/// Installs SIGTERM/SIGINT handlers that exit 0 at once (the commands
/// flush stdout after every line), and ignores SIGPIPE. A container's PID 1
/// ignores signals without a handler, so the handlers are needed for
/// `docker stop` as well.
pub fn exit_on_signals() {
    extern "C" fn on_signal(_: libc::c_int) {
        unsafe { libc::_exit(0) };
    }
    unsafe {
        libc::signal(libc::SIGTERM, on_signal as extern "C" fn(libc::c_int) as libc::sighandler_t);
        libc::signal(libc::SIGINT, on_signal as extern "C" fn(libc::c_int) as libc::sighandler_t);
        libc::signal(libc::SIGPIPE, libc::SIG_IGN);
    }
}

/// Command-line flags as `--name value` pairs and bare `--switch`es.
pub struct Args {
    prog: &'static str,
    args: Vec<String>,
    pos: usize,
}

impl Args {
    pub fn new(prog: &'static str) -> Self {
        Args { prog, args: std::env::args().skip(1).collect(), pos: 0 }
    }

    /// The next flag, or None at the end.
    pub fn next_flag(&mut self) -> Option<String> {
        let a = self.args.get(self.pos).cloned();
        self.pos += 1;
        a
    }

    /// The value of the flag just returned by `next_flag`; exits 1 if missing.
    pub fn value(&mut self, flag: &str) -> String {
        match self.args.get(self.pos).cloned() {
            Some(v) => {
                self.pos += 1;
                v
            }
            None => jl::fatal(1, &format!("{}: {} needs a value", self.prog, flag)),
        }
    }

    pub fn prog(&self) -> &'static str {
        self.prog
    }
}

/// strtol(s, NULL, 0) as the C adapters parse numbers: optional sign,
/// 0x hex, leading-0 octal, else decimal; trailing junk is ignored and
/// an unparsable string is 0.
pub fn strtol(s: &str) -> i64 {
    let s = s.trim_start();
    let (neg, rest) = match s.as_bytes().first() {
        Some(b'-') => (true, &s[1..]),
        Some(b'+') => (false, &s[1..]),
        _ => (false, s),
    };
    let (radix, digits) = if rest.starts_with("0x") || rest.starts_with("0X") {
        (16, &rest[2..])
    } else if rest.starts_with('0') && rest.len() > 1 {
        (8, &rest[1..])
    } else {
        (10, rest)
    };
    let mut v: i64 = 0;
    for c in digits.chars() {
        match c.to_digit(radix) {
            Some(d) => v = v.saturating_mul(radix as i64).saturating_add(d as i64),
            None => break,
        }
    }
    if neg {
        -v
    } else {
        v
    }
}

/// atoi(s): decimal prefix, 0 when unparsable.
pub fn atoi(s: &str) -> i64 {
    let s = s.trim_start();
    let (neg, rest) = match s.as_bytes().first() {
        Some(b'-') => (true, &s[1..]),
        Some(b'+') => (false, &s[1..]),
        _ => (false, s),
    };
    let mut v: i64 = 0;
    for c in rest.chars() {
        match c.to_digit(10) {
            Some(d) => v = v.saturating_mul(10).saturating_add(d as i64),
            None => break,
        }
    }
    if neg {
        -v
    } else {
        v
    }
}
