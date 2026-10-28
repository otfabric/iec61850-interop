// SPDX-License-Identifier: MIT
//
// iec61850-rust-mms-client — MMS client on iec61850_mms::mms::client::
// MmsConnection, the counterpart of libiec61850-mms-client.
//
//   iec61850-rust-mms-client [--host HOST] [--port PORT]
//
// The operation sequence, targets and value formatting follow
// mms_client.c and mms_client_services.c of the C adapter:
//   connection 1: identify, name lists, scalar reads/writes, data-model
//     operations, negative tests, then the fixture mms-v2 services
//     (status, name lists, type specs, alternate access, named variable
//     lists in domain/VMD/association scope, files, journals,
//     InformationReport), conclude;
//   connection 2: read boolean, conclude;
//   connection 3: read boolean, abort;
//   connection 4: write -1 to report-trigger (server-abort check).
//
// Requests travel over MmsConnection (ISO stack, Initiate, Conclude,
// stashing of unconfirmed PDUs). The iec61850_mms service helpers cover
// only part of the sequence, so every request is built here with the
// iec61850_mms codecs for values, type specifications and journals and a
// small BER writer (adapter::ber) for the rest, and sent with
// MmsConnection::send_mms_pdu. The upstream client has no A-ABORT: the
// abort of connection 3 closes the transport without one.
//
// Exit codes: 0 sequence completed, 1 argument error, 2 connection failure.

use std::time::Duration;

use bytes::Bytes;
use iec61850_mms::mms::client::{ClientError, MmsConnection};
use iec61850_mms::mms::pdu::read_journal::ReadJournalRequest;
use iec61850_mms::mms::pdu::{ConfirmedRequestRejectReason, MmsPdu, RejectReason};
use iec61850_mms::{MmsData, TypeSpecification};
use tokio::time::Instant;

use adapter::ber::{self, Name, Reader, Tlv};
use adapter::{cfmt, jl, timefmt, typed};

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
enum CErr {
    Svc(u32, u64),
    Rej(RejectReason),
    Timeout,
    Lost,
    Parse,
}

/// mms_err_name of mms_client_services.c (libiec61850 MmsError names).
fn mms_err_name(e: &CErr) -> &'static str {
    match e {
        CErr::Lost => "connection-lost",
        CErr::Timeout => "service-timeout",
        CErr::Parse => "parsing-response",
        CErr::Rej(RejectReason::PduError(_)) => "reject-invalid-pdu",
        CErr::Rej(RejectReason::ConfirmedRequest(ConfirmedRequestRejectReason::UnrecognizedService)) => {
            "reject-unrecognized-service"
        }
        CErr::Rej(_) => "mms-error",
        CErr::Svc(7, 2) => "object-non-existent",
        CErr::Svc(7, 3) => "object-access-denied",
        CErr::Svc(2, 1) => "object-undefined",
        CErr::Svc(11, 7) => "file-non-existent",
        CErr::Svc(11, 0) => "file-other",
        CErr::Svc(4, 0) => "service-other",
        CErr::Svc(..) => "mms-error",
    }
}

/// mms_error_str of mms_client.c (the short table of the main sequence).
fn mms_error_str(e: &CErr) -> &'static str {
    match e {
        CErr::Lost => "connection-lost",
        CErr::Timeout => "service-timeout",
        _ => "mms-error",
    }
}

fn dae_name(code: u8) -> &'static str {
    match code {
        0 => "object-invalidated",
        1 => "hardware-fault",
        2 => "temporarily-unavailable",
        3 => "object-access-denied",
        4 => "object-undefined",
        5 => "invalid-address",
        6 => "type-unsupported",
        7 => "type-inconsistent",
        8 => "object-attribute-inconsistent",
        9 => "object-access-unsupported",
        10 => "object-non-existent",
        11 => "object-value-invalid",
        _ => "unknown",
    }
}

fn map_client_error(e: ClientError) -> CErr {
    match e {
        ClientError::ServiceTimeout { .. } => CErr::Timeout,
        ClientError::PduParse(_) => CErr::Parse,
        ClientError::IsoError(s) if s.contains("timeout") => CErr::Timeout,
        _ => CErr::Lost,
    }
}

/// An AccessResult: a value or a DataAccessError code.
type Res = Result<MmsData, u8>;

// ---------------------------------------------------------------------------
// Value formatting
// ---------------------------------------------------------------------------

/// sb_value of mms_client_services.c.
fn sb_value(v: &MmsData) -> String {
    match v {
        MmsData::Boolean(b) => b.to_string(),
        MmsData::Integer(i) => i.to_string(),
        MmsData::Unsigned(u) => (*u as u32).to_string(),
        MmsData::Float32(f) => cfmt::g(*f as f64, 9),
        MmsData::Float64(d) => {
            if (*d as f32) as f64 == *d {
                cfmt::g(*d, 9)
            } else {
                cfmt::g(*d, 17)
            }
        }
        MmsData::VisibleString(s) | MmsData::MmsString(s) => jl::q(s),
        MmsData::OctetString(b) => jl::q(&typed::hex(b)),
        MmsData::BitString { padding, data } => jl::q(&typed::bits_string(*padding, data)),
        MmsData::UtcTime(raw) => timefmt::utc_ms_trunc(raw).to_string(),
        MmsData::BinaryTime(b) => timefmt::binary_time_ms(b).to_string(),
        MmsData::Array(items) | MmsData::Structure(items) => {
            format!("[{}]", items.iter().map(sb_value).collect::<Vec<_>>().join(","))
        }
        _ => "null".into(),
    }
}

fn sb_res(r: &Res) -> String {
    match r {
        Ok(v) => sb_value(v),
        Err(c) => format!("{{\"error\":\"{}\"}}", dae_name(*c)),
    }
}

fn spec_type_name(s: &TypeSpecification, services: bool) -> &'static str {
    use TypeSpecification as T;
    match s {
        T::Boolean => "boolean",
        T::Integer { .. } => "integer",
        T::Unsigned { .. } => "unsigned",
        T::FloatingPoint { .. } => "float32",
        T::VisibleString { .. } => "visible-string",
        T::MmsString { .. } if services => "mms-string",
        T::OctetString { .. } => "octet-string",
        T::BitString { .. } => "bit-string",
        T::UtcTime => "utc-time",
        T::BinaryTime { .. } if services => "binary-time",
        T::Array { .. } => "array",
        T::Structure { .. } => "structure",
        _ => "unknown",
    }
}

fn sb_spec(s: &TypeSpecification) -> String {
    let mut out = format!("{{\"type\":\"{}\"", spec_type_name(s, true));
    match s {
        TypeSpecification::Array { element_count, element_type } => {
            out += &format!(",\"count\":{element_count},\"element\":{}", sb_spec(element_type));
        }
        TypeSpecification::Structure { components } => {
            let c: Vec<String> =
                components.iter().map(|c| format!("{{\"name\":{},\"spec\":{}}}", jl::q(&c.name), sb_spec(&c.type_spec))).collect();
            out += &format!(",\"components\":[{}]", c.join(","));
        }
        _ => {}
    }
    out + "}"
}

// ---------------------------------------------------------------------------
// Output (mms_client_services.c helpers)
// ---------------------------------------------------------------------------

fn begin(op: &str, target: &str, ok: bool) -> String {
    format!("{{\"operation\":{},\"target\":{},\"ok\":{ok}", jl::q(op), jl::q(target))
}

fn emit_ok(op: &str, target: &str) {
    jl::line(&(begin(op, target, true) + "}"));
}

fn emit_fail(op: &str, target: &str, reason: &str) {
    jl::line(&format!("{},\"error\":{}}}", begin(op, target, false), jl::q(reason)));
}

fn emit_value(op: &str, target: &str, value: &str) {
    jl::line(&format!("{},\"value\":{value}}}", begin(op, target, true)));
}

fn emit_names(target: &str, names: &[String]) {
    let n: Vec<String> = names.iter().map(|s| jl::q(s)).collect();
    jl::line(&format!("{},\"names\":[{}]}}", begin("get-name-list", target, true), n.join(",")));
}

/// emit_read: service error, data access error or the value.
fn emit_read(op: &str, target: &str, r: Result<Res, CErr>) {
    match r {
        Err(e) => emit_fail(op, target, mms_err_name(&e)),
        Ok(Err(c)) => emit_fail(op, target, dae_name(c)),
        Ok(Ok(v)) => emit_value(op, target, &sb_value(&v)),
    }
}

fn emit_write(op: &str, target: &str, r: Result<Vec<Option<u8>>, CErr>) {
    match r {
        Ok(o) => match o.first() {
            Some(None) => emit_ok(op, target),
            Some(Some(c)) => emit_fail(op, target, dae_name(*c)),
            None => emit_fail(op, target, "mms-error"),
        },
        Err(e) => emit_fail(op, target, mms_err_name(&e)),
    }
}

fn emit_access_results(op: &str, target: &str, r: Result<Vec<Option<u8>>, CErr>) {
    match r {
        Err(e) => emit_fail(op, target, mms_err_name(&e)),
        Ok(o) => match o.iter().flatten().last() {
            Some(c) => emit_fail(op, target, dae_name(*c)),
            None => emit_ok(op, target),
        },
    }
}

// mms_client.c helpers (main sequence)

fn jl_ok(op: &str) {
    jl::line(&format!("{{\"operation\":\"{op}\",\"ok\":true}}"));
}

fn jl_fail(op: &str, error: &str) {
    jl::line(&format!("{{\"operation\":\"{op}\",\"ok\":false,\"error\":{}}}", jl::q(error)));
}

fn jl_ok_target(op: &str, target: &str) {
    jl::line(&format!("{{\"operation\":\"{op}\",\"target\":{},\"ok\":true}}", jl::q(target)));
}

fn jl_fail_target(op: &str, target: &str, error: &str) {
    jl::line(&format!("{{\"operation\":\"{op}\",\"target\":{},\"ok\":false,\"error\":{}}}", jl::q(target), jl::q(error)));
}

fn jl_read(target: &str, value: &str) {
    jl::line(&format!("{{\"operation\":\"read\",\"target\":{},\"ok\":true,\"value\":{value}}}", jl::q(target)));
}

fn jl_namelist(target: Option<&str>, names: &[String]) {
    let t = target.map(|t| format!(",\"target\":{}", jl::q(t))).unwrap_or_default();
    let n: Vec<String> = names.iter().map(|s| jl::q(s)).collect();
    jl::line(&format!("{{\"operation\":\"get-name-list\"{t},\"ok\":true,\"names\":[{}]}}", n.join(",")));
}

/// Main-sequence value printing (jl_read_* of mms_client.c).
fn main_value(v: &MmsData) -> String {
    match v {
        MmsData::Boolean(b) => b.to_string(),
        MmsData::Integer(i) => (*i as i32).to_string(),
        MmsData::Unsigned(u) => (*u as u32).to_string(),
        MmsData::Float32(f) => cfmt::g6(*f as f64),
        MmsData::Float64(d) => cfmt::g6(*d as f32 as f64),
        MmsData::VisibleString(s) | MmsData::MmsString(s) => jl::q(s),
        MmsData::OctetString(b) => jl::q(&typed::hex(b)),
        MmsData::BitString { padding, data } => jl::q(&typed::bits_string(*padding, data)),
        MmsData::UtcTime(raw) => timefmt::utc_ms_trunc(raw).to_string(),
        MmsData::Array(items) => {
            let n: Vec<String> = items
                .iter()
                .map(|x| match x {
                    MmsData::Integer(i) => (*i as i32).to_string(),
                    MmsData::Unsigned(u) => (*u as i32).to_string(),
                    _ => "0".into(),
                })
                .collect();
            format!("[{}]", n.join(","))
        }
        MmsData::Structure(items) => {
            let b = matches!(items.first(), Some(MmsData::Boolean(true)));
            let i = match items.get(1) {
                Some(MmsData::Integer(i)) => *i as i32,
                Some(MmsData::Unsigned(u)) => *u as i32,
                _ => 0,
            };
            format!("[{b},{i}]")
        }
        _ => "null".into(),
    }
}

/// jl_values_array of mms_client.c.
fn values_array(op: &str, target: &str, rs: &[Res]) {
    let v: Vec<String> = rs
        .iter()
        .map(|r| match r {
            Ok(MmsData::Boolean(b)) => b.to_string(),
            Ok(MmsData::Integer(i)) => (*i as i32).to_string(),
            Ok(MmsData::Unsigned(u)) => (*u as u32).to_string(),
            Ok(MmsData::Float32(f)) => cfmt::g6(*f as f64),
            _ => "null".into(),
        })
        .collect();
    jl::line(&format!("{{\"operation\":\"{op}\",\"target\":{},\"ok\":true,\"values\":[{}]}}", jl::q(target), v.join(",")));
}

// ---------------------------------------------------------------------------
// Request builders
// ---------------------------------------------------------------------------

fn dom(d: &str, i: &str) -> Name {
    Name::Dom(d.into(), i.into())
}

/// listOfVariable [0] of (name, alternateAccess content).
fn list_of_variable(vars: &[(Name, Option<Vec<u8>>)]) -> Vec<u8> {
    let mut out = Vec::new();
    for (n, alt) in vars {
        let mut e = ber::cc(0, &n.encode());
        if let Some(a) = alt {
            e.extend(ber::cc(5, a));
        }
        out.extend(ber::seq(&e));
    }
    ber::cc(0, &out)
}

/// variableListName [1].
fn list_name(n: &Name) -> Vec<u8> {
    ber::cc(1, &n.encode())
}

fn data_bytes(v: &MmsData) -> Vec<u8> {
    let mut b = bytes::BytesMut::new();
    v.encode(&mut b);
    b.to_vec()
}

fn alt_index(i: u32) -> Vec<u8> {
    ber::cp(2, &ber::uint_bytes(i as u64))
}

fn alt_range(lo: u32, n: u32) -> Vec<u8> {
    ber::cc(3, &ber::cat(&[&ber::cp(0, &ber::uint_bytes(lo as u64)), &ber::cp(1, &ber::uint_bytes(n as u64))]))
}

fn alt_component(c: &str) -> Vec<u8> {
    ber::cp(1, c.as_bytes())
}

enum Scope<'a> {
    Vmd,
    Domain(&'a str),
    Aa,
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

struct Client {
    conn: MmsConnection,
    invoke: u32,
    timeout: Duration,
    reports: bool,
}

impl Client {
    async fn connect(host: &str, port: u16, timeout_ms: u64, reports: bool) -> Result<Client, String> {
        let mut conn: MmsConnection = MmsConnection::new();
        conn.request_timeout_ms = 60_000;
        let ip = adapter::ied::resolve(host, port).await;
        match tokio::time::timeout(Duration::from_secs(10), conn.connect(&ip, port)).await {
            Ok(Ok(())) => Ok(Client { conn, invoke: 0, timeout: Duration::from_millis(timeout_ms), reports }),
            Ok(Err(e)) => Err(e.to_string()),
            Err(_) => Err("timeout".into()),
        }
    }

    /// Emits the InformationReports stashed while a confirmed call waited.
    fn drain_reports(&mut self) {
        while let Some(inner) = self.conn.pop_pending_unconfirmed() {
            if self.reports {
                emit_report(&inner);
            }
        }
    }

    /// One confirmed request; returns the response service TLV.
    async fn req(&mut self, service: Vec<u8>) -> Result<Vec<u8>, CErr> {
        self.invoke += 1;
        let inv = self.invoke;
        let inner = ber::cat(&[&ber::tlv(ber::UNIV, false, 2, &ber::uint_bytes(inv as u64)), &service]);
        self.conn.send_mms_pdu(&MmsPdu::ConfirmedRequest(Bytes::from(inner))).await.map_err(map_client_error)?;
        let deadline = Instant::now() + self.timeout;
        loop {
            let pdu = match tokio::time::timeout_at(deadline, self.conn.recv_mms_pdu_confirmed()).await {
                Err(_) => return Err(CErr::Timeout),
                Ok(Err(e)) => return Err(map_client_error(e)),
                Ok(Ok(p)) => p,
            };
            match pdu {
                MmsPdu::ConfirmedResponse(inner) => {
                    let mut r = Reader::new(&inner);
                    if r.next().map(|t| t.uint()) != Some(inv as u64) {
                        continue;
                    }
                    return r.next().map(|t| t.raw.to_vec()).ok_or(CErr::Parse);
                }
                MmsPdu::ConfirmedError(inner) => {
                    let mut r = Reader::new(&inner);
                    if r.next().map(|t| t.uint()) != Some(inv as u64) {
                        continue;
                    }
                    let class = r
                        .find(|t| t.ctx(true, 2))
                        .and_then(|se| se.children().next())
                        .and_then(|ec| ec.children().next())
                        .ok_or(CErr::Parse)?;
                    return Err(CErr::Svc(class.num, class.uint()));
                }
                MmsPdu::Reject(r) => return Err(CErr::Rej(r.reason)),
                MmsPdu::ConfirmedRequest(inner) => {
                    // A server-initiated request (ObtainFile reading from us):
                    // this client has no file store.
                    let id = Reader::new(&inner).next().map(|t| t.uint()).unwrap_or(0);
                    let err = ber::cat(&[&ber::cp(0, &ber::uint_bytes(id)), &ber::cc(2, &ber::cc(0, &ber::cp(11, &[7])))]);
                    let _ = self.conn.send_mms_pdu(&MmsPdu::ConfirmedError(Bytes::from(err))).await;
                }
                _ => {}
            }
        }
    }

    async fn read(&mut self, with_spec: bool, vas: Vec<u8>) -> Result<Vec<Res>, CErr> {
        let mut body = Vec::new();
        if with_spec {
            body.extend(ber::cp(0, &ber::bool_bytes(true)));
        }
        body.extend(ber::cc(1, &vas));
        let resp = self.req(ber::cc(4, &body)).await?;
        let (t, _) = ber::read(&resp).ok_or(CErr::Parse)?;
        let list = t.children().find(|x| x.ctx(true, 1)).ok_or(CErr::Parse)?;
        let mut out = Vec::new();
        for r in list.children() {
            if r.ctx(false, 0) {
                out.push(Err(r.uint() as u8));
            } else {
                out.push(Ok(MmsData::decode(r.raw).map_err(|_| CErr::Parse)?.0));
            }
        }
        Ok(out)
    }

    async fn read_one(&mut self, d: &str, i: &str, alt: Option<Vec<u8>>) -> Result<Res, CErr> {
        let mut r = self.read(false, list_of_variable(&[(dom(d, i), alt)])).await?;
        if r.len() != 1 {
            return Err(CErr::Parse);
        }
        Ok(r.remove(0))
    }

    async fn write(&mut self, vas: Vec<u8>, values: &[MmsData]) -> Result<Vec<Option<u8>>, CErr> {
        let data: Vec<u8> = values.iter().flat_map(data_bytes).collect();
        let resp = self.req(ber::cc(5, &ber::cat(&[&vas, &ber::cc(0, &data)]))).await?;
        let (t, _) = ber::read(&resp).ok_or(CErr::Parse)?;
        Ok(t.children().map(|r| if r.ctx(false, 0) { Some(r.uint() as u8) } else { None }).collect())
    }

    async fn write_one(&mut self, d: &str, i: &str, alt: Option<Vec<u8>>, v: MmsData) -> Result<Vec<Option<u8>>, CErr> {
        self.write(list_of_variable(&[(dom(d, i), alt)]), &[v]).await
    }

    async fn name_list(&mut self, class: u8, scope: Scope<'_>) -> Result<Vec<String>, CErr> {
        let sc = match scope {
            Scope::Vmd => ber::cp(0, &[]),
            Scope::Domain(d) => ber::cp(1, d.as_bytes()),
            Scope::Aa => ber::cp(2, &[]),
        };
        let mut names: Vec<String> = Vec::new();
        loop {
            let mut body = ber::cat(&[&ber::cc(0, &ber::cp(0, &[class])), &ber::cc(1, &sc)]);
            if let Some(last) = names.last() {
                body.extend(ber::cp(2, last.as_bytes()));
            }
            let resp = self.req(ber::cc(1, &body)).await?;
            let (t, _) = ber::read(&resp).ok_or(CErr::Parse)?;
            let mut r = t.children();
            let list = r.opt_ctx(true, 0).ok_or(CErr::Parse)?;
            let before = names.len();
            names.extend(list.children().map(|x| x.str()));
            let more = r.opt_ctx(false, 1).map(|b| b.bool()).unwrap_or(true);
            if !more || names.len() == before {
                return Ok(names);
            }
        }
    }

    async fn gvaa(&mut self, d: &str, i: &str) -> Result<TypeSpecification, CErr> {
        let resp = self.req(ber::cc(6, &ber::cc(0, &dom(d, i).encode()))).await?;
        let (t, _) = ber::read(&resp).ok_or(CErr::Parse)?;
        let ts = t.children().find(|x| x.ctx(true, 2)).ok_or(CErr::Parse)?;
        Ok(TypeSpecification::decode(ts.val, 0).map_err(|_| CErr::Parse)?.0)
    }

    async fn define_nvl(&mut self, n: &Name) -> Result<(), CErr> {
        let members = list_of_variable(&[(dom("interop", "boolean"), None), (dom("interop", "integer"), None)]);
        self.req(ber::cc(11, &ber::cat(&[&n.encode(), &members]))).await.map(|_| ())
    }

    /// (deletable, members) of a named variable list.
    async fn nvl_attrs(&mut self, n: &Name) -> Result<(bool, Vec<String>), CErr> {
        let resp = self.req(ber::cc(12, &n.encode())).await?;
        let (t, _) = ber::read(&resp).ok_or(CErr::Parse)?;
        let mut r = t.children();
        let del = r.opt_ctx(false, 0).map(|b| b.bool()).unwrap_or(false);
        let list = r.opt_ctx(true, 1).ok_or(CErr::Parse)?;
        let mut vars = Vec::new();
        for e in list.children() {
            let name = e
                .children()
                .find(|x| x.ctx(true, 0))
                .and_then(|vs| vs.children().next())
                .and_then(|n| Name::parse(&n))
                .ok_or(CErr::Parse)?;
            vars.push(match name {
                Name::Dom(d, i) => format!("{d}/{i}"),
                Name::Vmd(i) | Name::Aa(i) => i,
            });
        }
        Ok((del, vars))
    }

    /// true when the list was deleted.
    async fn delete_nvl(&mut self, n: &Name) -> Result<bool, CErr> {
        let resp = self.req(ber::cc(13, &ber::cat(&[&ber::cp(0, &[0]), &ber::cc(1, &n.encode())]))).await?;
        let (t, _) = ber::read(&resp).ok_or(CErr::Parse)?;
        Ok(t.children().find(|x| x.ctx(false, 1)).map(|x| x.uint()).unwrap_or(0) > 0)
    }

    async fn conclude(&mut self) -> Result<(), CErr> {
        tokio::time::timeout(self.timeout, self.conn.disconnect())
            .await
            .map_err(|_| CErr::Timeout)?
            .map_err(map_client_error)
    }
}

/// Emits an InformationReport (the inner bytes of an Unconfirmed PDU).
fn emit_report(inner: &[u8]) {
    let Some((ir, _)) = ber::read(inner).filter(|(t, _)| t.ctx(true, 0)) else { return };
    let mut r = ir.children();
    let Some(spec) = r.next() else { return };
    let Some(results) = r.next() else { return };
    let vals: Vec<Res> = results
        .children()
        .map(|x| if x.ctx(false, 0) { Err(x.uint() as u8) } else { MmsData::decode(x.raw).map(|d| d.0).map_err(|_| 0) })
        .collect();
    let target = |n: Name| match n {
        Name::Dom(d, i) => format!("{d}/{i}"),
        Name::Vmd(i) | Name::Aa(i) => i,
    };
    if spec.ctx(true, 1) {
        let Some(n) = spec.children().next().and_then(|n| Name::parse(&n)) else { return };
        let v: Vec<String> = vals.iter().map(sb_res).collect();
        emit_value("information-report", &format!("list:{}", target(n)), &format!("[{}]", v.join(",")));
    } else {
        for (i, e) in spec.children().enumerate() {
            let Some(n) = e.children().find(|x| x.ctx(true, 0)).and_then(|vs| vs.children().next()).and_then(|n| Name::parse(&n))
            else {
                continue;
            };
            let v = vals.get(i).map(sb_res).unwrap_or("null".into());
            emit_value("information-report", &format!("var:{}", target(n)), &v);
        }
    }
}

// ---------------------------------------------------------------------------
// Main sequence (mms_client.c run_sequence)
// ---------------------------------------------------------------------------

/// Main-sequence read: mms_error_str for service errors.
async fn main_read(c: &mut Client, item: &str) {
    let t = format!("interop/{item}");
    match c.read_one("interop", item, None).await {
        Ok(Ok(v)) => jl_read(&t, &main_value(&v)),
        Ok(Err(code)) => jl_fail_target("read", &t, dae_name(code)),
        Err(e) => jl_fail_target("read", &t, mms_error_str(&e)),
    }
}

/// emit_write_result of mms_client.c.
fn main_write_result(target: &str, r: Result<Vec<Option<u8>>, CErr>) {
    match r {
        Ok(o) => match o.first() {
            Some(None) => jl_ok_target("write", target),
            Some(Some(c)) => jl_fail_target("write", target, dae_name(*c)),
            None => jl_fail_target("write", target, "mms-error"),
        },
        Err(e) => jl_fail_target("write", target, mms_error_str(&e)),
    }
}

/// check_read_val path of the negative tests.
async fn negative_read(c: &mut Client, d: &str, i: &str) {
    let t = format!("{d}/{i}");
    match c.read_one(d, i, None).await {
        Ok(Ok(_)) => jl_ok_target("read", &t),
        Ok(Err(code)) => jl_fail_target("read", &t, dae_name(code)),
        Err(e) => jl_fail_target("read", &t, mms_error_str(&e)),
    }
}

async fn phase1(c: &mut Client) {
    // identify
    match c.req(ber::cp(2, &[])).await {
        Ok(resp) => {
            let mut f = [String::new(), String::new(), String::new()];
            if let Some((t, _)) = ber::read(&resp) {
                for x in t.children() {
                    if x.num < 3 {
                        f[x.num as usize] = x.str();
                    }
                }
            }
            jl::line(&format!(
                "{{\"operation\":\"identify\",\"ok\":true,\"value\":{{\"vendor\":{},\"model\":{},\"revision\":{}}}}}",
                jl::q(&f[0]),
                jl::q(&f[1]),
                jl::q(&f[2])
            ));
        }
        Err(e) => jl_fail("identify", mms_error_str(&e)),
    }
    match c.name_list(9, Scope::Vmd).await {
        Ok(n) => jl_namelist(None, &n),
        Err(e) => jl_fail("get-name-list", mms_error_str(&e)),
    }
    match c.name_list(0, Scope::Domain("interop")).await {
        Ok(n) => jl_namelist(Some("interop"), &n),
        Err(e) => jl_fail_target("get-name-list", "interop", mms_error_str(&e)),
    }
}

async fn run_sequence(host: &str, port: u16, full: bool) -> i32 {
    let mut c = match Client::connect(host, port, 5000, full).await {
        Ok(c) => c,
        Err(e) => {
            jl::log(&format!("connect to {host}:{port} failed: {e}"));
            return 2;
        }
    };
    jl::log(&format!("connected to {host}:{port}"));
    if full {
        phase1(&mut c).await;
    }
    main_read(&mut c, "boolean").await;
    if full {
        main_read(&mut c, "integer").await;
        main_read(&mut c, "float32").await;
        let r = c.write_one("interop", "float32", None, MmsData::Float32(99.0)).await;
        main_write_result("interop/float32", r);
        main_read(&mut c, "float32").await;
        let r = c.write_one("interop", "octet-string", None, MmsData::OctetString(vec![0xde, 0xad])).await;
        main_write_result("interop/octet-string", r);
        main_read(&mut c, "boolean").await;

        // Step 5: remaining scalar types
        main_read(&mut c, "unsigned").await;
        main_read(&mut c, "visible-string").await;
        let r = c.write_one("interop", "visible-string", None, MmsData::VisibleString("hello".into())).await;
        main_write_result("interop/visible-string", r);
        for item in ["visible-string", "octet-string", "bit-string", "utc-time", "array", "structure"] {
            main_read(&mut c, item).await;
        }

        // Step 6: data-model operations
        match c.gvaa("interop", "boolean").await {
            Ok(s) => jl::line(&format!(
                "{{\"operation\":\"get-var-access-attr\",\"target\":\"interop/boolean\",\"ok\":true,\"type\":\"{}\"}}",
                spec_type_name(&s, false)
            )),
            Err(e) => jl_fail_target("get-var-access-attr", "interop/boolean", mms_error_str(&e)),
        }
        let both = [(dom("interop", "boolean"), None), (dom("interop", "integer"), None)];
        match c.read(false, list_of_variable(&both)).await {
            Ok(rs) => values_array("read-multiple", "interop", &rs),
            Err(e) => jl_fail_target("read-multiple", "interop", mms_error_str(&e)),
        }
        match c.write(list_of_variable(&both), &[MmsData::Boolean(false), MmsData::Integer(0)]).await {
            Ok(o) if o.iter().all(Option::is_none) => jl_ok_target("write-multiple", "interop"),
            Ok(_) => jl_fail_target("write-multiple", "interop", "partial-failure"),
            Err(e) => jl_fail_target("write-multiple", "interop", mms_error_str(&e)),
        }
        let tl = dom("interop", "testlist");
        match c.define_nvl(&tl).await {
            Ok(()) => jl_ok_target("define-nvl", "interop/testlist"),
            Err(e) => jl_fail_target("define-nvl", "interop/testlist", mms_error_str(&e)),
        }
        match c.read(false, list_name(&tl)).await {
            Ok(rs) => values_array("read-nvl", "interop/testlist", &rs),
            Err(e) => jl_fail_target("read-nvl", "interop/testlist", mms_error_str(&e)),
        }
        match c.delete_nvl(&tl).await {
            Ok(true) => jl_ok_target("delete-nvl", "interop/testlist"),
            Ok(false) => jl_fail_target("delete-nvl", "interop/testlist", "none"),
            Err(e) => jl_fail_target("delete-nvl", "interop/testlist", mms_error_str(&e)),
        }

        // Step 7: negative tests
        negative_read(&mut c, "unknown-domain", "boolean").await;
        negative_read(&mut c, "interop", "nonexistent").await;
        let r = c.write_one("interop", "integer", None, MmsData::Boolean(true)).await;
        main_write_result("interop/integer-wrong-type", r);

        run_services(&mut c).await;
    }
    match c.conclude().await {
        Ok(()) => jl_ok("conclude"),
        Err(e) => jl_fail("conclude", mms_error_str(&e)),
    }
    0
}

// ---------------------------------------------------------------------------
// Fixture mms-v2 services (mms_client_services.c)
// ---------------------------------------------------------------------------

async fn run_services(c: &mut Client) {
    svc_status(c).await;
    svc_name_lists(c).await;
    svc_type_specs(c).await;
    svc_alternate_access(c).await;
    svc_named_variable_lists(c).await;
    svc_files(c).await;
    svc_journals(c).await;
    svc_reports(c).await;
}

async fn svc_status(c: &mut Client) {
    for (ext, target) in [(false, ""), (true, "extended")] {
        match c.req(ber::cp(0, &ber::bool_bytes(ext))).await {
            Ok(resp) => {
                let (mut l, mut p) = (-1i64, -1i64);
                if let Some((t, _)) = ber::read(&resp) {
                    for x in t.children() {
                        match x.num {
                            0 => l = x.int(),
                            1 => p = x.int(),
                            _ => {}
                        }
                    }
                }
                emit_value("status", target, &format!("{{\"logical\":{l},\"physical\":{p}}}"));
            }
            Err(e) => emit_fail("status", target, mms_err_name(&e)),
        }
    }
}

async fn svc_name_lists(c: &mut Client) {
    for (class, scope, target) in [
        (8u8, Scope::Domain("interop"), "interop/journals"),
        (0, Scope::Vmd, "vmd/variables"),
        (0, Scope::Domain("unknown-domain"), "unknown-domain/variables"),
    ] {
        match c.name_list(class, scope).await {
            Ok(n) => emit_names(target, &n),
            Err(e) => emit_fail("get-name-list", target, mms_err_name(&e)),
        }
    }
}

async fn svc_type_specs(c: &mut Client) {
    for item in ["array-rw", "structure-rw"] {
        let target = format!("interop/{item}");
        match c.gvaa("interop", item).await {
            Ok(s) => jl::line(&format!(
                "{},\"type\":\"{}\",\"value\":{}}}",
                begin("get-var-access-attr", &target, true),
                spec_type_name(&s, true),
                sb_spec(&s)
            )),
            Err(e) => emit_fail("get-var-access-attr", &target, mms_err_name(&e)),
        }
    }
}

async fn svc_alternate_access(c: &mut Client) {
    let r = c.read_one("interop", "array", Some(alt_range(1, 3))).await;
    emit_read("read-array-elements", "interop/array[1:4]", r);
    let r = c.read_one("interop", "array", Some(alt_index(2))).await;
    emit_read("read-array-elements", "interop/array[2]", r);
    let r = c.read_one("interop", "structure-rw", Some(alt_component("count"))).await;
    emit_read("read-component", "interop/structure-rw$count", r);

    let r = c.write_one("interop", "array-rw", Some(alt_index(1)), MmsData::Integer(21)).await;
    emit_write("write-array-elements", "interop/array-rw[1]", r);
    let range = MmsData::Array(vec![MmsData::Integer(33), MmsData::Integer(44)]);
    let r = c.write_one("interop", "array-rw", Some(alt_range(2, 2)), range).await;
    emit_write("write-array-elements", "interop/array-rw[2:4]", r);
    let r = c.write_one("interop", "structure-rw", Some(alt_component("count")), MmsData::Integer(99)).await;
    emit_write("write-component", "interop/structure-rw$count", r);

    let r = c.read_one("interop", "array-rw", None).await;
    emit_read("read", "interop/array-rw", r);
    let r = c.read_one("interop", "structure-rw", None).await;
    emit_read("read", "interop/structure-rw", r);
}

fn emit_nvl_directory(target: &str, r: Result<(bool, Vec<String>), CErr>) {
    match r {
        Err(e) => emit_fail("get-nvl-attrs", target, mms_err_name(&e)),
        Ok((del, vars)) => {
            let v: Vec<String> = vars.iter().map(|s| jl::q(s)).collect();
            emit_value("get-nvl-attrs", target, &format!("{{\"deletable\":{del},\"variables\":[{}]}}", v.join(",")));
        }
    }
}

/// emit_read of a whole named-variable-list read (value = array of results).
fn emit_nvl_read(target: &str, r: Result<Vec<Res>, CErr>) {
    match r {
        Err(e) => emit_fail("read-nvl", target, mms_err_name(&e)),
        Ok(rs) => {
            let v: Vec<String> = rs.iter().map(sb_res).collect();
            emit_value("read-nvl", target, &format!("[{}]", v.join(",")));
        }
    }
}

fn emit_define(target: &str, r: Result<(), CErr>) {
    match r {
        Ok(()) => emit_ok("define-nvl", target),
        Err(e) => emit_fail("define-nvl", target, mms_err_name(&e)),
    }
}

fn emit_delete(target: &str, r: Result<bool, CErr>) {
    match r {
        Ok(true) => emit_ok("delete-nvl", target),
        Ok(false) => emit_fail("delete-nvl", target, "none"),
        Err(e) => emit_fail("delete-nvl", target, mms_err_name(&e)),
    }
}

async fn svc_named_variable_lists(c: &mut Client) {
    // Domain scope
    let n = dom("interop", "svclist");
    let r = c.define_nvl(&n).await;
    emit_define("interop/svclist", r);
    match c.name_list(2, Scope::Domain("interop")).await {
        Ok(l) => emit_names("interop/nvl", &l),
        Err(e) => emit_fail("get-name-list", "interop/nvl", mms_err_name(&e)),
    }
    let r = c.nvl_attrs(&n).await;
    emit_nvl_directory("interop/svclist", r);
    let r = c.read(true, list_name(&n)).await;
    emit_nvl_read("interop/svclist?spec", r);
    let r = c.write(list_name(&n), &[MmsData::Boolean(true), MmsData::Integer(77)]).await;
    emit_access_results("write-nvl", "interop/svclist", r);
    let r = c.read(false, list_name(&n)).await;
    emit_nvl_read("interop/svclist", r);
    let r = c.delete_nvl(&n).await;
    emit_delete("interop/svclist", r);

    // VMD scope
    let n = Name::Vmd("vmdlist".into());
    let r = c.define_nvl(&n).await;
    emit_define("vmd/vmdlist", r);
    let r = c.nvl_attrs(&n).await;
    emit_nvl_directory("vmd/vmdlist", r);
    let r = c.read(false, list_name(&n)).await;
    emit_nvl_read("vmd/vmdlist", r);
    let r = c.delete_nvl(&n).await;
    emit_delete("vmd/vmdlist", r);

    // Server-defined list behind the fixture's named-list InformationReport
    let n = Name::Vmd("interop-report".into());
    let r = c.nvl_attrs(&n).await;
    emit_nvl_directory("vmd/interop-report", r);
    let r = c.read(false, list_name(&n)).await;
    emit_nvl_read("vmd/interop-report", r);

    // Association scope
    let n = Name::Aa("aalist".into());
    let r = c.define_nvl(&n).await;
    emit_define("aa/aalist", r);
    match c.name_list(2, Scope::Aa).await {
        Ok(l) => emit_names("aa/nvl", &l),
        Err(e) => emit_fail("get-name-list", "aa/nvl", mms_err_name(&e)),
    }
    let r = c.nvl_attrs(&n).await;
    emit_nvl_directory("aa/aalist", r);
    let r = c.read(false, list_name(&n)).await;
    emit_nvl_read("aa/aalist", r);
    let r = c.write(list_name(&n), &[MmsData::Boolean(true), MmsData::Integer(-123)]).await;
    emit_access_results("write-nvl", "aa/aalist", r);
    let r = c.delete_nvl(&n).await;
    emit_delete("aa/aalist", r);
}

// --- files -----------------------------------------------------------------

fn fname(s: &str) -> Vec<u8> {
    ber::file_name(s)
}

/// (name, size, lastModified) of the directory entries in a
/// listOfDirectoryEntry [0], with or without the inner SEQUENCE wrapper.
fn dir_entries(list: &Tlv, out: &mut Vec<(String, u64, i64)>) {
    for e in list.children() {
        let first = e.children().next();
        if first.is_some_and(|f| f.is(ber::UNIV, true, 16)) {
            dir_entries(&e, out);
            continue;
        }
        let mut name = String::new();
        let (mut size, mut mtime) = (0u64, 0i64);
        for x in e.children() {
            if x.ctx(true, 0) {
                name = ber::parse_file_name(x.val).unwrap_or_default();
            } else if x.ctx(true, 1) {
                for a in x.children() {
                    match a.num {
                        0 => size = a.uint(),
                        1 => mtime = ber::parse_gen_time(&a.str()).unwrap_or(0),
                        _ => {}
                    }
                }
            }
        }
        out.push((name, size, mtime));
    }
}

async fn file_directory(c: &mut Client, spec: Option<&str>, target: &str) {
    let mut entries: Vec<(String, u64, i64)> = Vec::new();
    let mut after: Option<String> = None;
    loop {
        let mut body = Vec::new();
        if let Some(s) = spec {
            body.extend(ber::cc(0, &fname(s)));
        }
        if let Some(a) = &after {
            body.extend(ber::cc(1, &fname(a)));
        }
        let resp = match c.req(ber::cc(77, &body)).await {
            Ok(r) => r,
            Err(e) => return emit_fail("file-directory", target, mms_err_name(&e)),
        };
        let Some((t, _)) = ber::read(&resp) else { return emit_fail("file-directory", target, "parsing-response") };
        let mut r = t.children();
        let before = entries.len();
        if let Some(list) = r.opt_ctx(true, 0) {
            dir_entries(&list, &mut entries);
        }
        let more = r.opt_ctx(false, 1).map(|b| b.bool()).unwrap_or(false);
        let last = entries.last().map(|e| e.0.clone());
        if !more || entries.len() == before || last == after {
            break;
        }
        after = last;
    }
    let v: Vec<String> = entries
        .iter()
        .map(|(n, s, m)| format!("{{\"name\":{},\"size\":{s},\"lastModified\":{m}}}", jl::q(n)))
        .collect();
    emit_value("file-directory", target, &format!("[{}]", v.join(",")));
}

fn fnv1a64(d: &[u8]) -> u64 {
    d.iter().fold(0xcbf29ce484222325u64, |h, b| (h ^ *b as u64).wrapping_mul(0x100000001b3))
}

async fn file_read(c: &mut Client, name: &str, pos: u32, target: &str) {
    let open = ber::cc(72, &ber::cat(&[&ber::cc(0, &fname(name)), &ber::cp(1, &ber::uint_bytes(pos as u64))]));
    let resp = match c.req(open).await {
        Ok(r) => r,
        Err(e) => return emit_fail("file-read", target, mms_err_name(&e)),
    };
    let (mut frsm, mut size, mut mtime) = (0i64, 0u64, 0i64);
    if let Some((t, _)) = ber::read(&resp) {
        for x in t.children() {
            if x.ctx(false, 0) {
                frsm = x.int();
            } else if x.ctx(true, 1) {
                for a in x.children() {
                    match a.num {
                        0 => size = a.uint(),
                        1 => mtime = ber::parse_gen_time(&a.str()).unwrap_or(0),
                        _ => {}
                    }
                }
            }
        }
    }
    let mut data = Vec::new();
    let mut err = None;
    loop {
        match c.req(ber::cp(73, &ber::int_bytes(frsm))).await {
            Ok(resp) => {
                let Some((t, _)) = ber::read(&resp) else {
                    err = Some(CErr::Parse);
                    break;
                };
                let mut r = t.children();
                let chunk = r.opt_ctx(false, 0).map(|x| x.val.to_vec()).unwrap_or_default();
                let more = r.opt_ctx(false, 1).map(|b| b.bool()).unwrap_or(true);
                let empty = chunk.is_empty();
                data.extend(chunk);
                if !more || empty {
                    break;
                }
            }
            Err(e) => {
                err = Some(e);
                break;
            }
        }
    }
    match err {
        Some(e) => emit_fail("file-read", target, mms_err_name(&e)),
        None => {
            let mut v = format!(
                "{{\"size\":{size},\"lastModified\":{mtime},\"length\":{},\"fnv1a\":\"{:016x}\"",
                data.len(),
                fnv1a64(&data)
            );
            if data.len() <= 256 {
                v += &format!(",\"data\":{}", jl::q(&typed::hex(&data)));
            }
            emit_value("file-read", target, &(v + "}"));
        }
    }
    match c.req(ber::cp(74, &ber::int_bytes(frsm))).await {
        Ok(_) => emit_ok("file-close", target),
        Err(e) => emit_fail("file-close", target, mms_err_name(&e)),
    }
}

async fn svc_files(c: &mut Client) {
    file_directory(c, None, "").await;
    file_directory(c, Some("COMTRADE"), "COMTRADE").await;
    file_directory(c, Some("COMTRADE\\"), "COMTRADE\\").await;

    file_read(c, "REC01.CFG", 0, "REC01.CFG").await;
    file_read(c, "REC01.DAT", 4, "REC01.DAT@4").await;
    file_read(c, "LARGE.BIN", 0, "LARGE.BIN").await;
    file_read(c, "NOTIME.TXT", 0, "NOTIME.TXT").await;
    file_read(c, "NO-SUCH.FILE", 0, "NO-SUCH.FILE").await;

    match c.req(ber::cc(76, &fname("DELETE.ME"))).await {
        Ok(_) => emit_ok("file-delete", "DELETE.ME"),
        Err(e) => emit_fail("file-delete", "DELETE.ME", mms_err_name(&e)),
    }
    match c.req(ber::cc(75, &ber::cat(&[&ber::cc(0, &fname("RENAME.ME")), &ber::cc(1, &fname("RENAMED.ME"))]))).await {
        Ok(_) => emit_ok("file-rename", "RENAME.ME->RENAMED.ME"),
        Err(e) => emit_fail("file-rename", "RENAME.ME->RENAMED.ME", mms_err_name(&e)),
    }
    match c.req(ber::cc(46, &ber::cat(&[&ber::cc(1, &fname("REC01.CFG")), &ber::cc(2, &fname("OBTAINED.CFG"))]))).await {
        Ok(_) => emit_ok("obtain-file", "REC01.CFG->OBTAINED.CFG"),
        Err(e) => emit_fail("obtain-file", "REC01.CFG->OBTAINED.CFG", mms_err_name(&e)),
    }
    file_directory(c, None, "after-changes").await;
}

// --- journals --------------------------------------------------------------

async fn read_journal(c: &mut Client, req: ReadJournalRequest, target: &str) {
    let mut b = bytes::BytesMut::new();
    req.encode(&mut b);
    let resp = match c.req(b.to_vec()).await {
        Ok(r) => r,
        Err(e) => return emit_fail("read-journal", target, mms_err_name(&e)),
    };
    let Some((t, _)) = ber::read(&resp) else { return emit_fail("read-journal", target, "parsing-response") };
    let mut r = t.children();
    let mut entries = Vec::new();
    if let Some(list) = r.opt_ctx(true, 0) {
        for e in list.children() {
            let (mut id, mut time, mut vars) = (String::from("null"), String::from("null"), Vec::new());
            for x in e.children() {
                if x.ctx(false, 0) {
                    id = jl::q(&typed::hex(x.val));
                } else if x.ctx(true, 2) {
                    for y in x.children() {
                        if y.ctx(false, 0) {
                            time = timefmt::binary_time_ms(y.val).to_string();
                        } else if y.ctx(true, 2) {
                            for lv in y.children().filter(|z| z.ctx(true, 1)) {
                                for v in lv.children() {
                                    let mut vr = v.children();
                                    let tag = vr.opt_ctx(false, 0).map(|z| z.str()).unwrap_or_default();
                                    let val = vr
                                        .opt_ctx(true, 1)
                                        .and_then(|z| MmsData::decode(z.val).ok())
                                        .map(|d| sb_value(&d.0))
                                        .unwrap_or("null".into());
                                    vars.push(format!("{{\"tag\":{},\"value\":{val}}}", jl::q(&tag)));
                                }
                            }
                        }
                    }
                }
            }
            entries.push(format!("{{\"id\":{id},\"time\":{time},\"variables\":[{}]}}", vars.join(",")));
        }
    }
    let more = r.opt_ctx(false, 1).map(|b| b.bool()).unwrap_or(false);
    emit_value("read-journal", target, &format!("{{\"moreFollows\":{more},\"entries\":[{}]}}", entries.join(",")));
}

async fn svc_journals(c: &mut Client) {
    const T1: u64 = 1704067201000;
    const T4: u64 = 1704067204000;
    read_journal(c, ReadJournalRequest::time_range("interop", "log1", T1, T4), "interop/log1").await;
    let id2 = [2u8, 0, 0, 0, 0, 0, 0, 0];
    read_journal(c, ReadJournalRequest::start_after("interop", "log1", T1 + 1000, id2), "interop/log1@after-2").await;
    read_journal(c, ReadJournalRequest::time_range("interop", "nolog", T1, T4), "interop/nolog").await;
}

// --- InformationReport -----------------------------------------------------

async fn svc_reports(c: &mut Client) {
    let r = c.write_one("interop", "report-trigger", None, MmsData::Integer(1)).await;
    // Reports that arrived before the write response were stashed by
    // MmsConnection; they precede the write line, as with the C handler.
    c.drain_reports();
    let got_before = r.is_ok();
    emit_write("write", "interop/report-trigger", r);

    // Wait up to 3 s for reports, then 300 ms of quiet after the last.
    let (mut waited, mut quiet, mut seen) = (0u64, 0u64, false);
    while got_before && waited < 3000 && quiet < 300 {
        match c.conn.recv_mms_pdu_with_timeout(Some(Duration::from_millis(50))).await {
            Ok(Some(MmsPdu::Unconfirmed(inner))) => {
                emit_report(&inner);
                seen = true;
                quiet = 0;
            }
            Ok(Some(_)) => {}
            Ok(None) => {
                if seen {
                    quiet += 50;
                }
            }
            Err(_) => break,
        }
        waited += 50;
    }
    let r = c.read_one("interop", "boolean", None).await;
    emit_read("read", "interop/boolean#after-reports", r);
}

// ---------------------------------------------------------------------------
// Connections 3 and 4
// ---------------------------------------------------------------------------

async fn run_abort(host: &str, port: u16) {
    let mut c = match Client::connect(host, port, 5000, false).await {
        Ok(c) => c,
        Err(_) => return emit_fail("abort", "", "mms-error"),
    };
    let r = c.read_one("interop", "boolean", None).await;
    emit_read("read", "interop/boolean#before-abort", r);
    // No A-ABORT in the upstream client: close the transport.
    c.conn.close_raw();
    emit_ok("abort", "");
}

async fn run_server_abort(host: &str, port: u16) {
    let mut c = match Client::connect(host, port, 3000, false).await {
        Ok(c) => c,
        Err(_) => return emit_fail("server-abort", "", "mms-error"),
    };
    // Writing -1 to report-trigger asks the server to abort the association.
    match c.write_one("interop", "report-trigger", None, MmsData::Integer(-1)).await {
        Err(CErr::Lost) => emit_ok("server-abort", ""),
        _ => emit_fail("server-abort", "", "connection-not-lost"),
    }
    c.conn.close_raw();
}

fn main() {
    jl::handle_meta_flags();
    let args: Vec<String> = std::env::args().collect();
    let mut host = "127.0.0.1".to_string();
    let mut port: i64 = 1102;
    for i in 1..args.len().saturating_sub(1) {
        match args[i].as_str() {
            "--host" => host = args[i + 1].clone(),
            "--port" => port = adapter::atoi(&args[i + 1]),
            _ => {}
        }
    }
    if !(1..=65535).contains(&port) {
        jl::fatal(1, &format!("mms-client: invalid port {port}"));
    }
    let port = port as u16;
    adapter::exit_on_signals();
    jl::log(&format!("connecting to {host}:{port}"));
    let rt = adapter::ied::runtime();
    let code = rt.block_on(async {
        if run_sequence(&host, port, true).await == 2 {
            jl::log(&format!("fatal: connection 1 failed to connect to {host}:{port}"));
            return 2;
        }
        if run_sequence(&host, port, false).await == 2 {
            jl::log(&format!("fatal: connection 2 (reconnect) failed to connect to {host}:{port}"));
            return 2;
        }
        run_abort(&host, port).await;
        run_server_abort(&host, port).await;
        0
    });
    std::process::exit(code);
}
