// SPDX-License-Identifier: MIT
//
// Shared pieces of the IED client commands (ied-client, ied-controller,
// ied-reporter) on iec61850_client::IedConnection.

pub use iec61850_client::IsoConnectionParameters;
use iec61850_client::{ClientError, IedConnection};
use iec61850_mms::iso::cotp::TSelector;
use iec61850_mms::DataAccessError;

/// The libiec61850 IedClientError string of a DataAccessError, as the C
/// adapters print it (iedConnection_mapDataAccessErrorToIedError followed by
/// their ied_error_str table; codes the table lacks are "unknown-error").
pub fn dae_str(d: DataAccessError, with_unsupported: bool) -> &'static str {
    match d {
        DataAccessError::ObjectAccessDenied => "access-denied",
        DataAccessError::TypeInconsistent => "type-inconsistent",
        DataAccessError::ObjectAccessUnsupported if with_unsupported => "object-access-unsupported",
        DataAccessError::ObjectNonExistent => "object-does-not-exist",
        _ => "unknown-error",
    }
}

/// libiec61850-style error string of an iec61850-client error.
/// `with_unsupported`: the C table of the command lists
/// object-access-unsupported (ied-client and ied-controller do, the
/// reporter does not).
pub fn err_str(e: &ClientError, with_unsupported: bool) -> String {
    match e {
        ClientError::DataAccessError(d) => dae_str(*d, with_unsupported).into(),
        ClientError::NotConnected => "not-connected".into(),
        ClientError::Mms(s) => mms_err_str(s, with_unsupported).into(),
        _ => "unknown-error".into(),
    }
}

/// The client crate flattens MMS-layer errors into their Display text.
fn mms_err_str(s: &str, with_unsupported: bool) -> &'static str {
    if s.contains("timed out") {
        return "timeout";
    }
    if s.contains("connection was lost") || s.contains("not connected") {
        return "connection-lost";
    }
    if s.contains("class=Access(2)") || s.contains("class=Definition(1)") {
        return "object-does-not-exist";
    }
    if s.contains("class=Access(3)") {
        return "access-denied";
    }
    if s.contains("class=Access(1)") && with_unsupported {
        return "object-access-unsupported";
    }
    "unknown-error"
}

/// The IP address of `host`: the iec61850-rust connect functions parse
/// "host:port" as a socket address and do not resolve names. IPv4 is
/// preferred; an unresolvable name is passed on unchanged.
pub async fn resolve(host: &str, port: u16) -> String {
    match tokio::net::lookup_host((host, port)).await {
        Ok(it) => {
            let addrs: Vec<std::net::SocketAddr> = it.collect();
            addrs
                .iter()
                .find(|a| a.is_ipv4())
                .or(addrs.first())
                .map(|a| a.ip().to_string())
                .unwrap_or_else(|| host.to_string())
        }
        Err(_) => host.to_string(),
    }
}

/// Connects, mapping a failure to the libiec61850 string.
pub async fn connect(host: &str, port: u16) -> Result<IedConnection, String> {
    connect_with(host, port, None).await
}

/// Parses a T-selector of 1..4 octets written as hex ("0001").
pub fn parse_tsel(hex: &str) -> Option<TSelector> {
    if hex.is_empty() || hex.len() % 2 != 0 || !hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        return None;
    }
    let bytes: Vec<u8> = (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).ok())
        .collect::<Option<Vec<u8>>>()?;
    TSelector::from_slice(&bytes).ok()
}

/// Like [`connect`], with the given ISO parameters instead of upstream's
/// defaults when `params` is set.
pub async fn connect_with(
    host: &str,
    port: u16,
    params: Option<IsoConnectionParameters>,
) -> Result<IedConnection, String> {
    let conn = match params {
        Some(p) => IedConnection::with_iso_parameters(p),
        None => IedConnection::new(),
    };
    let ip = resolve(host, port).await;
    match conn.connect(&ip, port).await {
        Ok(()) => Ok(conn),
        Err(e) => {
            crate::jl::log(&format!("connect detail: {e}"));
            // IedConnection_connect gives IED_ERROR_CONNECTION_REJECTED or
            // IED_ERROR_TIMEOUT; only "timeout" is in the C string tables.
            Err(match &e {
                ClientError::Mms(s) if s.contains("timed out") => "timeout".into(),
                _ => "unknown-error".into(),
            })
        }
    }
}

/// --host/--port parsing of the IED client commands (atoi port, other
/// flags passed to `extra`).
pub fn host_port(prog: &str, mut extra: impl FnMut(&str, &str)) -> (String, u16) {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut host = "localhost".to_string();
    let mut port: i64 = 1102;
    let mut i = 0;
    while i < args.len() {
        let has_val = i + 1 < args.len();
        match args[i].as_str() {
            "--host" if has_val => {
                host = args[i + 1].clone();
                i += 1;
            }
            "--port" if has_val => {
                port = crate::atoi(&args[i + 1]);
                i += 1;
            }
            f if has_val && f.starts_with("--") => {
                extra(f, &args[i + 1]);
                i += 1;
            }
            _ => {}
        }
        i += 1;
    }
    if port <= 0 || port > 65535 {
        crate::jl::fatal(1, &format!("{prog}: invalid port {port}"));
    }
    (host, port as u16)
}

/// The logical device of the fixture, as the SCL names it.
pub const LD_NAME: &str = "InteropLD";

/// Contract 1.3 LD choice from a server directory: "InteropLD", else the
/// first name ending in it (an IED-name-prefixed domain such as
/// "InteropIEDInteropLD"), else the first.
pub fn pick_ld(names: &[String]) -> Option<String> {
    if names.iter().any(|n| n == LD_NAME) {
        return Some(LD_NAME.into());
    }
    if let Some(n) = names.iter().find(|n| n.ends_with(LD_NAME)) {
        return Some(n.clone());
    }
    names.first().cloned()
}

/// The logical device of the reporter and controller: $IEC61850_RUST_LD
/// when set, else [`pick_ld`] over the server directory, else "InteropLD"
/// (the fallback is logged on stderr, not emitted).
pub async fn resolve_ld(conn: &IedConnection, prog: &str) -> String {
    if let Ok(ld) = std::env::var("IEC61850_RUST_LD") {
        return ld;
    }
    match conn.get_server_directory(false).await {
        Ok(lds) => pick_ld(&lds).unwrap_or_else(|| {
            crate::jl::log(&format!("{prog}: server directory is empty; using {LD_NAME}"));
            LD_NAME.into()
        }),
        Err(e) => {
            crate::jl::log(&format!("{prog}: get server directory: {e}; using {LD_NAME}"));
            LD_NAME.into()
        }
    }
}

/// A small multi-thread tokio runtime for the client commands.
pub fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .expect("tokio runtime")
}

/// `{"operation":op[,"target":t],"ok":false,"error":e}`
pub fn emit_error(op: &str, target: Option<&str>, e: &str) {
    match target {
        Some(t) => crate::jl::line(&format!(
            "{{\"operation\":\"{op}\",\"target\":{},\"ok\":false,\"error\":{}}}",
            crate::jl::q(t),
            crate::jl::q(e)
        )),
        None => crate::jl::line(&format!("{{\"operation\":\"{op}\",\"ok\":false,\"error\":{}}}", crate::jl::q(e))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn v(names: &[&str]) -> Vec<String> {
        names.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn ld_resolution() {
        assert_eq!(pick_ld(&v(&["A", "InteropLD", "XInteropLD"])).as_deref(), Some("InteropLD"));
        assert_eq!(pick_ld(&v(&["A", "InteropIEDInteropLD"])).as_deref(), Some("InteropIEDInteropLD"));
        assert_eq!(pick_ld(&v(&["A", "B"])).as_deref(), Some("A"));
        assert_eq!(pick_ld(&[]), None);
    }
}
