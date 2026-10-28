// SPDX-License-Identifier: MIT
//
// iec61850-rust-mms-server — generic MMS server over a fixture, the
// counterpart of libiec61850-mms-server.
//
//   iec61850-rust-mms-server [--fixture PATH] [--port PORT] [--filestore DIR]
//
// iec61850-rust's server crate serves an IEC 61850 data model only, so the
// association is run here with the iec61850_mms ISO stack (COTP, session,
// presentation, ACSE and the MMS Initiate/Conclude handling of
// iec61850_mms::mms::server::parse_message), replicating the connection
// loop of iec61850-server's lifecycle.rs. Confirmed requests are served
// from the fixture: values and type specifications are encoded with the
// iec61850_mms codecs, the services it has no codec for with a small BER
// writer (adapter::ber).
//
// Behaviour follows the libiec61850 generic server of the C adapter:
//   - GetNameList of domain variables lists structure components as
//     flattened "name$component" names, sorted; VMD-scope variables are
//     refused (access object-access-unsupported);
//   - unknown variables read as object-non-existent, writes to read-only
//     variables are object-access-denied, mistyped writes type-inconsistent;
//   - alternate access: component, index and index range (read and write);
//   - named variable lists in domain, VMD and association scope;
//   - writing interop/report-trigger sends the fixture's InformationReports
//     to the writing association before the write response (both the
//     listOfVariable and the named-list form);
//   - ReadJournal over the fixture journals (entry id = 8 LE bytes, plus the
//     ReasonCode variable);
//   - file services over an in-memory store (--filestore is accepted and
//     ignored); ObtainFile reads the file from the client.
//
// Exit codes: 0 clean shutdown, 1 startup/argument error, 3 fixture error.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::BytesMut;
use iec61850_mms::iso::acse::{encode_aare, encode_associate_failed, encode_rlre, AcseConnection, AcseIndication};
use iec61850_mms::iso::cotp::{CotpConnection, CotpOptions};
use iec61850_mms::iso::presentation::{
    encode_cpa, encode_user_data, encode_user_data_acse, parse_connect as pres_parse_connect, parse_user_data,
    IsoPresentation, ACSE_CONTEXT_ID, MMS_CONTEXT_ID,
};
use iec61850_mms::iso::session::{
    encode_accept as sess_encode_accept, encode_data, encode_disconnect, encode_finish, IsoSession,
    SessionIndication,
};
use iec61850_mms::mms::pdu::read_journal::{ReadJournalResponse, WireJournalEntry, WireJournalVariable};
use iec61850_mms::mms::server::{
    encode_reject_pdu, parse_message, ConfirmedRequest, ConfirmedResponse, MessageOutcome, MmsServerConnection,
    MmsServiceDispatcher,
};
use iec61850_mms::{
    encode_confirmed_error_pdu, ErrorClass, MmsData, ServiceError, StructComponent, TypeSpecification,
};
use iec61850_mms::mms::pdu::{ConfirmedRequestRejectReason, RejectReason};
use serde_json::Value;
use tokio::net::{TcpListener, TcpStream};

use adapter::ber::{self, Name, Reader, Tlv};
use adapter::{jl, timefmt, typed};

const REPORT_TRIGGER: &str = "report-trigger";

// DataAccessError codes
const DAE_ACCESS_DENIED: u8 = 3;
const DAE_INVALID_ADDRESS: u8 = 5;
const DAE_TYPE_INCONSISTENT: u8 = 7;
const DAE_ACCESS_UNSUPPORTED: u8 = 9;
const DAE_NON_EXISTENT: u8 = 10;

// ---------------------------------------------------------------------------
// Fixture model
// ---------------------------------------------------------------------------

struct Var {
    spec: TypeSpecification,
    value: MmsData,
    writable: bool,
}

struct JEntry {
    id: u64,
    time_ms: u64,
    tag: String,
    value: MmsData,
    reason: u8,
}

#[derive(Clone)]
struct Member {
    dom: String,
    item: String,
    alt: Option<Vec<u8>>,
}

#[derive(Clone)]
struct Nvl {
    members: Vec<Member>,
    deletable: bool,
}

struct Domain {
    vars: BTreeMap<String, Var>,
    names: Vec<String>,
    journals: BTreeMap<String, Vec<JEntry>>,
    nvls: BTreeMap<String, Nvl>,
}

struct Report {
    vmd_list: Option<String>,
    vars: Vec<String>,
}

struct FileEnt {
    data: Vec<u8>,
    mtime_ms: i64,
}

struct Model {
    revision: String,
    vendor: String,
    model: String,
    rev: String,
    logical: i64,
    physical: i64,
    domains: BTreeMap<String, Domain>,
    dom_order: Vec<String>,
    vmd_nvls: BTreeMap<String, Nvl>,
    reports: Vec<Report>,
    files: BTreeMap<String, FileEnt>,
}

fn to_data(v: iec61850_model::MmsValue) -> MmsData {
    use iec61850_model::MmsValue as V;
    match v {
        V::Boolean(b) => MmsData::Boolean(b),
        V::Integer(i) => MmsData::Integer(i),
        V::Unsigned(u) => MmsData::Unsigned(u),
        V::Float32(f) => MmsData::Float32(f),
        V::Float64(f) => MmsData::Float64(f),
        V::VisibleString(s) => MmsData::VisibleString(s),
        V::MmsString(s) => MmsData::MmsString(s),
        V::OctetString(b) => MmsData::OctetString(b),
        V::BitString { padding, data } => MmsData::BitString { padding, data },
        V::UtcTime(t) => MmsData::UtcTime(t),
        V::BinaryTime(b) => MmsData::BinaryTime(b),
        V::Array(a) => MmsData::Array(a.into_iter().map(to_data).collect()),
        V::Structure(a) => MmsData::Structure(a.into_iter().map(to_data).collect()),
    }
}

/// Type specification of a scalar fixture type (make_spec of fixture.c).
fn scalar_spec(t: &str, size: i64) -> Result<TypeSpecification, String> {
    let sized = |d: i64| if size > 0 { size } else { d };
    Ok(match t {
        "boolean" => TypeSpecification::Boolean,
        "integer" => TypeSpecification::Integer { width_bits: sized(32) as u8 },
        "unsigned" => TypeSpecification::Unsigned { width_bits: sized(32) as u8 },
        "float32" => TypeSpecification::FloatingPoint { format_width: 32, exponent_width: 8 },
        "visible-string" => TypeSpecification::VisibleString { max_chars: sized(-1) as i32 },
        "octet-string" => TypeSpecification::OctetString { max_octets: sized(-1) as i32 },
        "bit-string" => TypeSpecification::BitString { bits: sized(-1) as i32 },
        "utc-time" => TypeSpecification::UtcTime,
        _ => return Err(format!("unsupported type {t:?}")),
    })
}

/// Spec and value of one fixture variable (or structure component).
fn load_var(v: &Value) -> Result<(TypeSpecification, MmsData), String> {
    let t = v.get("type").and_then(Value::as_str).ok_or("variable without type")?;
    let size = v.get("size").and_then(Value::as_i64).unwrap_or(0);
    match t {
        "array" => {
            let et = v.get("elementType").and_then(Value::as_str).ok_or("array without elementType")?;
            let items = v.get("value").and_then(Value::as_array).ok_or("array value is not a JSON array")?;
            let count = v.get("count").and_then(Value::as_u64).unwrap_or(items.len() as u64);
            let espec = scalar_spec(et, 0)?;
            let vals = items
                .iter()
                .map(|x| typed::from_json(&serde_json::json!({"type": et, "value": x})).map(to_data))
                .collect::<Result<Vec<_>, _>>()?;
            Ok((TypeSpecification::Array { element_count: count as u32, element_type: Box::new(espec) }, MmsData::Array(vals)))
        }
        "structure" => {
            let items = v.get("value").and_then(Value::as_array).ok_or("structure value is not a JSON array")?;
            let mut comps = Vec::new();
            let mut vals = Vec::new();
            for (i, c) in items.iter().enumerate() {
                let (s, val) = load_var(c)?;
                let name = c.get("name").and_then(Value::as_str).map(str::to_string).unwrap_or(format!("comp{i}"));
                comps.push(StructComponent { name, type_spec: s });
                vals.push(val);
            }
            Ok((TypeSpecification::Structure { components: comps }, MmsData::Structure(vals)))
        }
        _ => {
            let spec = scalar_spec(t, size)?;
            let val = typed::from_json(&serde_json::json!({"type": t, "value": v.get("value").cloned().unwrap_or(Value::Null)}))
                .map_err(|e| e.to_string())?;
            Ok((spec, to_data(val)))
        }
    }
}

fn flat_names(name: &str, spec: &TypeSpecification, out: &mut Vec<String>) {
    out.push(name.to_string());
    if let TypeSpecification::Structure { components } = spec {
        for c in components {
            flat_names(&format!("{name}${}", c.name), &c.type_spec, out);
        }
    }
}

fn load_fixture(path: &str, start_ms: i64) -> Result<Model, String> {
    let text = std::fs::read_to_string(path).map_err(|e| format!("{path}: {e}"))?;
    let f: Value = serde_json::from_str(&text).map_err(|e| format!("{path}: {e}"))?;
    let s = |v: Option<&Value>, d: &str| v.and_then(Value::as_str).unwrap_or(d).to_string();
    let id = f.get("identity");
    let st = f.get("status");
    let mut m = Model {
        revision: s(f.get("revision"), "unknown"),
        vendor: s(id.and_then(|i| i.get("vendor")), ""),
        model: s(id.and_then(|i| i.get("model")), ""),
        rev: s(id.and_then(|i| i.get("revision")), ""),
        logical: st.and_then(|x| x.get("logical")).and_then(Value::as_i64).unwrap_or(0),
        physical: st.and_then(|x| x.get("physical")).and_then(Value::as_i64).unwrap_or(0),
        domains: BTreeMap::new(),
        dom_order: Vec::new(),
        vmd_nvls: BTreeMap::new(),
        reports: Vec::new(),
        files: BTreeMap::new(),
    };
    let doms = f.get("domains").and_then(Value::as_object).ok_or("fixture has no domains")?;
    for (dn, dd) in doms {
        let mut d = Domain { vars: BTreeMap::new(), names: Vec::new(), journals: BTreeMap::new(), nvls: BTreeMap::new() };
        if let Some(vars) = dd.get("variables").and_then(Value::as_object) {
            for (vn, vd) in vars {
                let (spec, value) = load_var(vd).map_err(|e| format!("{dn}/{vn}: {e}"))?;
                let writable = vd.get("writable").and_then(Value::as_bool).unwrap_or(false);
                flat_names(vn, &spec, &mut d.names);
                d.vars.insert(vn.clone(), Var { spec, value, writable });
            }
        }
        d.names.sort();
        if let Some(js) = dd.get("journals").and_then(Value::as_object) {
            for (jn, jd) in js {
                let mut entries = Vec::new();
                for e in jd.get("entries").and_then(Value::as_array).into_iter().flatten() {
                    let t = e.get("type").and_then(Value::as_str).unwrap_or("integer");
                    let value = typed::from_json(&serde_json::json!({"type": t, "value": e.get("value")}))
                        .map(to_data)
                        .map_err(|x| format!("journal {jn}: {x}"))?;
                    let time_ms = e
                        .get("occurrenceTime")
                        .and_then(Value::as_str)
                        .and_then(timefmt::parse_ms)
                        .ok_or(format!("journal {jn}: bad occurrenceTime"))?;
                    entries.push(JEntry {
                        id: e.get("entryId").and_then(Value::as_u64).unwrap_or(0),
                        time_ms: time_ms as u64,
                        tag: s(e.get("tag"), ""),
                        value,
                        reason: e.get("reasonCode").and_then(Value::as_u64).unwrap_or(0) as u8,
                    });
                }
                jl::log(&format!("  journal {dn}/{jn} ({} entries)", entries.len()));
                d.journals.insert(jn.clone(), entries);
            }
        }
        m.dom_order.push(dn.clone());
        m.domains.insert(dn.clone(), d);
    }
    for r in f.get("informationReports").and_then(Value::as_array).into_iter().flatten() {
        m.reports.push(Report {
            vmd_list: r.get("vmdList").and_then(Value::as_str).map(str::to_string),
            vars: r
                .get("variables")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|v| v.as_str().map(str::to_string))
                .collect(),
        });
    }
    if let Some(files) = f.get("files").and_then(Value::as_object) {
        for (name, fd) in files {
            let data = if let Some(c) = fd.get("content").and_then(Value::as_str) {
                c.as_bytes().to_vec()
            } else if let Some(h) = fd.get("hex").and_then(Value::as_str) {
                typed::unhex(h).ok_or(format!("file {name}: bad hex"))?
            } else if let Some(n) = fd.get("size").and_then(Value::as_u64) {
                (0..n).map(|i| (i % 251) as u8).collect()
            } else {
                Vec::new()
            };
            let mtime_ms = match fd.get("lastModified").and_then(Value::as_str) {
                Some(t) => timefmt::parse_ms(t).ok_or(format!("file {name}: bad lastModified"))?,
                None => start_ms,
            };
            jl::log(&format!("  file {name} ({} bytes)", data.len()));
            m.files.insert(name.trim_start_matches('/').to_string(), FileEnt { data, mtime_ms });
        }
    }
    Ok(m)
}

// ---------------------------------------------------------------------------
// Variable access
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
enum Sel {
    Pos(usize),
    Range(usize, usize),
}

#[derive(Debug)]
enum Step {
    Comp(String),
    Index(u32),
    Range(u32, u32),
    All,
}

/// Parses the content of alternateAccess [5] into a path of steps. Only a
/// single (possibly nested) selection is supported.
fn parse_alt(content: &[u8], out: &mut Vec<Step>) -> Result<(), u8> {
    let items: Vec<Tlv> = Reader::new(content).collect();
    if items.len() != 1 {
        return Err(DAE_ACCESS_UNSUPPORTED);
    }
    let t = items[0];
    let range = |t: &Tlv| -> Result<Step, u8> {
        let mut r = t.children();
        let lo = r.opt_ctx(false, 0).ok_or(DAE_INVALID_ADDRESS)?.uint() as u32;
        let n = r.opt_ctx(false, 1).ok_or(DAE_INVALID_ADDRESS)?.uint() as u32;
        Ok(Step::Range(lo, n))
    };
    if t.cls != ber::CTX {
        return Err(DAE_ACCESS_UNSUPPORTED);
    }
    match (t.cons, t.num) {
        (false, 1) => out.push(Step::Comp(t.str())),
        (false, 2) => out.push(Step::Index(t.uint() as u32)),
        (true, 3) => out.push(range(&t)?),
        (false, 4) => out.push(Step::All),
        (true, 0) => {
            // selectAlternateAccess { accessSelection, alternateAccess }
            let mut r = t.children();
            let sel = r.next().ok_or(DAE_INVALID_ADDRESS)?;
            match (sel.cons, sel.num) {
                (false, 0) => out.push(Step::Comp(sel.str())),
                (false, 1) => out.push(Step::Index(sel.uint() as u32)),
                (true, 2) => out.push(range(&sel)?),
                (false, 3) => out.push(Step::All),
                _ => return Err(DAE_ACCESS_UNSUPPORTED),
            }
            let rest = r.next().ok_or(DAE_INVALID_ADDRESS)?;
            if rest.is(ber::UNIV, true, 16) {
                parse_alt(rest.val, out)?;
            } else {
                parse_alt(rest.raw, out)?;
            }
        }
        _ => return Err(DAE_ACCESS_UNSUPPORTED),
    }
    Ok(())
}

struct Target {
    dom: String,
    var: String,
    path: Vec<Sel>,
    spec: TypeSpecification,
}

impl Model {
    fn resolve(&self, dom: &str, item: &str, alt: Option<&[u8]>) -> Result<Target, u8> {
        let d = self.domains.get(dom).ok_or(DAE_NON_EXISTENT)?;
        let mut parts = item.split('$');
        let base = parts.next().unwrap_or("");
        let v = d.vars.get(base).ok_or(DAE_NON_EXISTENT)?;
        let mut spec = v.spec.clone();
        let mut path = Vec::new();
        for p in parts {
            let TypeSpecification::Structure { components } = &spec else { return Err(DAE_NON_EXISTENT) };
            let i = components.iter().position(|c| c.name == p).ok_or(DAE_NON_EXISTENT)?;
            path.push(Sel::Pos(i));
            spec = components[i].type_spec.clone();
        }
        if let Some(alt) = alt {
            let mut steps = Vec::new();
            parse_alt(alt, &mut steps)?;
            let last = steps.len().saturating_sub(1);
            for (k, s) in steps.into_iter().enumerate() {
                match (s, spec.clone()) {
                    (Step::Comp(name), TypeSpecification::Structure { components }) => {
                        let i = components.iter().position(|c| c.name == name).ok_or(DAE_INVALID_ADDRESS)?;
                        path.push(Sel::Pos(i));
                        spec = components[i].type_spec.clone();
                    }
                    (Step::Index(i), TypeSpecification::Array { element_count, element_type }) if i < element_count => {
                        path.push(Sel::Pos(i as usize));
                        spec = *element_type;
                    }
                    (Step::Range(lo, n), TypeSpecification::Array { element_count, element_type })
                        if k == last && n > 0 && lo.checked_add(n).is_some_and(|e| e <= element_count) =>
                    {
                        path.push(Sel::Range(lo as usize, n as usize));
                        spec = TypeSpecification::Array { element_count: n, element_type };
                    }
                    (Step::All, TypeSpecification::Array { .. }) if k == last => {}
                    _ => return Err(DAE_INVALID_ADDRESS),
                }
            }
        }
        Ok(Target { dom: dom.into(), var: base.into(), path, spec })
    }

    fn get(&self, t: &Target) -> MmsData {
        let mut cur = &self.domains[&t.dom].vars[&t.var].value;
        for s in &t.path {
            match (s, cur) {
                (Sel::Pos(i), MmsData::Array(a) | MmsData::Structure(a)) => cur = &a[*i],
                (Sel::Range(lo, n), MmsData::Array(a)) => return MmsData::Array(a[*lo..*lo + *n].to_vec()),
                _ => break,
            }
        }
        cur.clone()
    }

    fn set(&mut self, t: &Target, nv: MmsData) {
        let mut cur = &mut self.domains.get_mut(&t.dom).unwrap().vars.get_mut(&t.var).unwrap().value;
        for s in &t.path {
            match (s, cur) {
                (Sel::Pos(i), MmsData::Array(a) | MmsData::Structure(a)) => cur = &mut a[*i],
                (Sel::Range(lo, n), MmsData::Array(a)) => {
                    if let MmsData::Array(items) = nv {
                        a[*lo..*lo + *n].clone_from_slice(&items);
                    }
                    return;
                }
                _ => return,
            }
        }
        *cur = nv;
    }

    fn write_one(&mut self, m: &Member, v: MmsData) -> Result<(), u8> {
        let t = self.resolve(&m.dom, &m.item, m.alt.as_deref())?;
        if !self.domains[&t.dom].vars[&t.var].writable {
            return Err(DAE_ACCESS_DENIED);
        }
        let nv = conform(&t.spec, v).ok_or(DAE_TYPE_INCONSISTENT)?;
        self.set(&t, nv);
        Ok(())
    }
}

/// `v` as a value of type `spec`, or None when it does not have that type.
fn conform(spec: &TypeSpecification, v: MmsData) -> Option<MmsData> {
    use TypeSpecification as T;
    Some(match (spec, v) {
        (T::Boolean, v @ MmsData::Boolean(_)) => v,
        (T::Integer { .. }, v @ MmsData::Integer(_)) => v,
        (T::Unsigned { .. }, v @ MmsData::Unsigned(_)) => v,
        (T::FloatingPoint { .. }, MmsData::Float32(f)) => MmsData::Float32(f),
        (T::FloatingPoint { .. }, MmsData::Float64(f)) => MmsData::Float32(f as f32),
        (T::VisibleString { max_chars }, MmsData::VisibleString(s)) if *max_chars <= 0 || s.len() <= *max_chars as usize => {
            MmsData::VisibleString(s)
        }
        (T::MmsString { max_chars }, MmsData::MmsString(s)) if *max_chars <= 0 || s.len() <= *max_chars as usize => {
            MmsData::MmsString(s)
        }
        (T::OctetString { max_octets }, MmsData::OctetString(b)) if *max_octets <= 0 || b.len() <= *max_octets as usize => {
            MmsData::OctetString(b)
        }
        (T::BitString { .. }, v @ MmsData::BitString { .. }) => v,
        (T::UtcTime, v @ MmsData::UtcTime(_)) => v,
        (T::BinaryTime { .. }, v @ MmsData::BinaryTime(_)) => v,
        (T::Array { element_count, element_type }, MmsData::Array(items)) if items.len() == *element_count as usize => {
            MmsData::Array(items.into_iter().map(|x| conform(element_type, x)).collect::<Option<Vec<_>>>()?)
        }
        (T::Structure { components }, MmsData::Structure(items)) if items.len() == components.len() => MmsData::Structure(
            items.into_iter().zip(components).map(|(x, c)| conform(&c.type_spec, x)).collect::<Option<Vec<_>>>()?,
        ),
        _ => return None,
    })
}

fn data_bytes(v: &MmsData) -> Vec<u8> {
    let mut b = BytesMut::new();
    v.encode(&mut b);
    b.to_vec()
}

fn access_result(r: Result<MmsData, u8>) -> Vec<u8> {
    match r {
        Ok(v) => data_bytes(&v),
        Err(code) => ber::cp(0, &[code]),
    }
}

/// libiec61850's ReasonCode bit string: bit i of the integer is bit i
/// (MSB first) of the 7-bit string.
fn reason_byte(rc: u8) -> u8 {
    (0..7).filter(|i| rc & (1 << i) != 0).fold(0u8, |a, i| a | (0x80 >> i))
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

enum SvcErr {
    Class(ErrorClass),
    Reject(ConfirmedRequestRejectReason),
}

type SvcResult = Result<Vec<u8>, SvcErr>;

fn access_err(code: u8) -> SvcErr {
    // DataAccessError -> access class: non-existent 2, denied 3, unsupported 1
    SvcErr::Class(match code {
        DAE_NON_EXISTENT => ErrorClass::Access(2),
        DAE_ACCESS_DENIED => ErrorClass::Access(3),
        DAE_ACCESS_UNSUPPORTED => ErrorClass::Access(1),
        _ => ErrorClass::Access(0),
    })
}

const INVALID: SvcErr = SvcErr::Reject(ConfirmedRequestRejectReason::InvalidArgument);
const FILE_OTHER: SvcErr = SvcErr::Class(ErrorClass::File(0));
const FILE_POSITION_INVALID: SvcErr = SvcErr::Class(ErrorClass::File(5));
const FILE_NON_EXISTENT: SvcErr = SvcErr::Class(ErrorClass::File(7));

// ---------------------------------------------------------------------------
// Initiate dispatcher (confirmed requests are served by Assoc below)
// ---------------------------------------------------------------------------

struct InitDispatcher;

/// servicesSupportedCalled: status, getNameList, identify, read, write,
/// getVariableAccessAttributes, define/getAttributes/delete
/// NamedVariableList, obtainFile, readJournal, the file services and
/// informationReport.
fn services_supported() -> [u8; 11] {
    let mut b = [0u8; 11];
    for bit in [0usize, 1, 2, 4, 5, 6, 11, 12, 13, 46, 65, 72, 73, 74, 75, 76, 77, 79] {
        b[bit / 8] |= 0x80 >> (bit % 8);
    }
    b
}

impl MmsServiceDispatcher for InitDispatcher {
    fn dispatch(&self, _: &MmsServerConnection, _: ConfirmedRequest) -> ConfirmedResponse {
        ConfirmedResponse::Reject
    }
    fn services_supported(&self) -> [u8; 11] {
        services_supported()
    }
}

// ---------------------------------------------------------------------------
// Association
// ---------------------------------------------------------------------------

struct OpenFile {
    data: Vec<u8>,
    pos: usize,
}

struct Assoc {
    cotp: CotpConnection<TcpStream>,
    session: IsoSession,
    pres: IsoPresentation,
    mms: MmsServerConnection,
    model: Arc<Mutex<Model>>,
    aa: BTreeMap<String, Nvl>,
    files: HashMap<i64, OpenFile>,
    next_frsm: i64,
    next_invoke: u32,
    reports: Vec<Vec<u8>>,
}

type R<T> = Result<T, String>;

fn e<E: std::fmt::Debug>(x: E) -> String {
    format!("{x:?}")
}

impl Assoc {
    fn max_pdu(&self) -> usize {
        self.mms.negotiated_max_pdu_size().unwrap_or(65000) as usize
    }

    async fn send_mms(&mut self, pdu: &[u8]) -> R<()> {
        let mut p = BytesMut::new();
        encode_user_data(&self.pres, pdu, &mut p);
        let mut s = BytesMut::new();
        encode_data(&p, &mut s);
        self.cotp.send_data(&s).await.map_err(e)
    }

    /// Receives one MMS PDU (for ObtainFile, while the client waits).
    async fn recv_mms(&mut self) -> R<Vec<u8>> {
        let payload = self.cotp.recv_data().await.map_err(e)?;
        let mut sess = self.session.clone();
        match sess.parse_message(&payload).map_err(e)? {
            SessionIndication::Data => {}
            other => return Err(format!("session {other:?}")),
        }
        let ud = sess.user_data(&payload).to_vec();
        let mut pres = self.pres.clone();
        let r = parse_user_data(&mut pres, &ud).map_err(e)?;
        if r.context_id != MMS_CONTEXT_ID {
            return Err("not on the MMS context".into());
        }
        Ok(r.payload(&ud).to_vec())
    }

    /// Server-initiated confirmed request; returns the response service
    /// TLV, or the client's ServiceError class.
    async fn call(&mut self, service: Vec<u8>) -> Result<Vec<u8>, SvcErr> {
        let inv = self.next_invoke;
        self.next_invoke += 1;
        let pdu = ber::cc(0, &ber::cat(&[&ber::tlv(ber::UNIV, false, 2, &ber::uint_bytes(inv as u64)), &service]));
        self.send_mms(&pdu).await.map_err(|_| FILE_OTHER)?;
        let fut = async {
            loop {
                let resp = self.recv_mms().await.map_err(|_| FILE_OTHER)?;
                let Some((t, _)) = ber::read(&resp) else { return Err(FILE_OTHER) };
                let mut r = t.children();
                let id = r.next().map(|x| x.uint()).unwrap_or(u64::MAX);
                if id != inv as u64 {
                    continue;
                }
                return match (t.cons, t.num) {
                    (true, 1) => r.next().map(|x| x.raw.to_vec()).ok_or(FILE_OTHER),
                    (true, 2) => {
                        // serviceError [2] { errorClass [0] { class [n] code } }
                        let class = r
                            .find(|x| x.ctx(true, 2))
                            .and_then(|se| se.children().next())
                            .and_then(|ec| ec.children().next());
                        match class {
                            Some(c) if c.num == 11 => Err(SvcErr::Class(ErrorClass::File(c.uint() as u8))),
                            _ => Err(FILE_OTHER),
                        }
                    }
                    _ => Err(FILE_OTHER),
                };
            }
        };
        tokio::time::timeout(Duration::from_secs(5), fut).await.unwrap_or(Err(FILE_OTHER))
    }

    /// Serves one ConfirmedRequest PDU (tag 0xa0); returns the PDUs to send.
    async fn confirmed(&mut self, pdu: &[u8]) -> Vec<Vec<u8>> {
        let parsed = ber::read(pdu).and_then(|(t, _)| {
            let mut r = t.children();
            let inv = r.next().filter(|x| x.is(ber::UNIV, false, 2))?.uint() as u32;
            let svc = r.find(|x| x.cls == ber::CTX)?;
            Some((inv, svc.cons, svc.num, svc.val.to_vec(), svc.raw.to_vec()))
        });
        let Some((inv, cons, num, val, _raw)) = parsed else {
            return vec![encode_reject_pdu(None, RejectReason::ConfirmedRequest(ConfirmedRequestRejectReason::Other)).to_vec()];
        };
        let res = match (cons, num) {
            (false, 0) => self.status(),
            (true, 1) => self.get_name_list(&val),
            (false, 2) => self.identify(),
            (true, 4) => self.read(&val),
            (true, 5) => self.write(&val),
            (true, 6) => self.gvaa(&val),
            (true, 11) => self.define_nvl(&val),
            (true, 12) => self.nvl_attrs(&val),
            (true, 13) => self.delete_nvl(&val),
            (true, 46) => self.obtain_file(&val).await,
            (true, 65) => self.read_journal(&val),
            (true, 72) => self.file_open(&val),
            (false, 73) => self.file_read(&val),
            (false, 74) => self.file_close(&val),
            (true, 75) => self.file_rename(&val),
            (true, 76) => self.file_delete(&val),
            (true, 77) => self.file_directory(&val),
            _ => {
                jl::log(&format!("rejecting unsupported confirmed service {num}"));
                Err(SvcErr::Reject(ConfirmedRequestRejectReason::UnrecognizedService))
            }
        };
        let mut out = std::mem::take(&mut self.reports);
        out.push(match res {
            Ok(body) => ber::cc(1, &ber::cat(&[&ber::tlv(ber::UNIV, false, 2, &ber::uint_bytes(inv as u64)), &body])),
            Err(SvcErr::Class(c)) => {
                let mut b = BytesMut::new();
                encode_confirmed_error_pdu(inv, &ServiceError::new(c), &mut b);
                b.to_vec()
            }
            Err(SvcErr::Reject(r)) => encode_reject_pdu(Some(inv), RejectReason::ConfirmedRequest(r)).to_vec(),
        });
        out
    }

    fn identify(&self) -> SvcResult {
        let m = self.model.lock().unwrap();
        Ok(ber::cc(
            2,
            &ber::cat(&[&ber::cp(0, m.vendor.as_bytes()), &ber::cp(1, m.model.as_bytes()), &ber::cp(2, m.rev.as_bytes())]),
        ))
    }

    fn status(&self) -> SvcResult {
        let m = self.model.lock().unwrap();
        Ok(ber::cc(0, &ber::cat(&[&ber::cp(0, &ber::int_bytes(m.logical)), &ber::cp(1, &ber::int_bytes(m.physical))])))
    }

    fn get_name_list(&self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let class = r.opt_ctx(true, 0).and_then(|t| t.children().next()).ok_or(INVALID)?.int();
        let scope = r.opt_ctx(true, 1).and_then(|t| t.children().next()).ok_or(INVALID)?;
        let after = r.opt_ctx(false, 2).map(|t| t.str());
        let m = self.model.lock().unwrap();
        let mut names: Vec<String> = match (class, scope.num) {
            (9, 0) => m.dom_order.clone(),
            (_, 1) => {
                let d = m.domains.get(&scope.str()).ok_or(access_err(DAE_NON_EXISTENT))?;
                match class {
                    0 => d.names.clone(),
                    2 => d.nvls.keys().cloned().collect(),
                    8 => d.journals.keys().cloned().collect(),
                    _ => return Err(access_err(DAE_ACCESS_UNSUPPORTED)),
                }
            }
            (2, 0) => m.vmd_nvls.keys().cloned().collect(),
            (2, 2) => self.aa.keys().cloned().collect(),
            _ => return Err(access_err(DAE_ACCESS_UNSUPPORTED)),
        };
        if let Some(a) = after {
            if let Some(i) = names.iter().position(|n| *n == a) {
                names.drain(..=i);
            }
        }
        let budget = self.max_pdu().saturating_sub(32);
        let (mut list, mut more) = (Vec::new(), false);
        for (i, n) in names.iter().enumerate() {
            let el = ber::vstr(n);
            if i > 0 && list.len() + el.len() > budget {
                more = true;
                break;
            }
            list.extend(el);
        }
        Ok(ber::cc(1, &ber::cat(&[&ber::cc(0, &list), &ber::cp(1, &ber::bool_bytes(more))])))
    }

    /// The members a VariableAccessSpecification TLV selects.
    fn access_spec(&self, m: &Model, t: &Tlv) -> Result<Vec<Member>, SvcErr> {
        if t.ctx(true, 0) {
            let mut out = Vec::new();
            for entry in t.children() {
                let mut er = entry.children();
                let vs = er.opt_ctx(true, 0).ok_or(INVALID)?;
                let name = vs.children().next().and_then(|n| Name::parse(&n)).ok_or(INVALID)?;
                let alt = er.opt_ctx(true, 5).map(|a| a.val.to_vec());
                let (dom, item) = match name {
                    Name::Dom(d, i) => (d, i),
                    Name::Vmd(i) | Name::Aa(i) => (String::new(), i),
                };
                out.push(Member { dom, item, alt });
            }
            return Ok(out);
        }
        if t.ctx(true, 1) {
            let name = t.children().next().and_then(|n| Name::parse(&n)).ok_or(INVALID)?;
            return Ok(self.lookup_nvl(m, &name)?.members.clone());
        }
        Err(INVALID)
    }

    fn lookup_nvl<'a>(&'a self, m: &'a Model, n: &Name) -> Result<&'a Nvl, SvcErr> {
        let found = match n {
            Name::Vmd(i) => m.vmd_nvls.get(i),
            Name::Aa(i) => self.aa.get(i),
            Name::Dom(d, i) => m.domains.get(d).ok_or(access_err(DAE_NON_EXISTENT))?.nvls.get(i),
        };
        found.ok_or(access_err(DAE_NON_EXISTENT))
    }

    fn read(&self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let with_spec = r.opt_ctx(false, 0).map(|t| t.bool()).unwrap_or(false);
        let vas = r.opt_ctx(true, 1).ok_or(INVALID)?;
        let choice = vas.children().next().ok_or(INVALID)?;
        let m = self.model.lock().unwrap();
        let members = self.access_spec(&m, &choice)?;
        let mut results = Vec::new();
        for mb in &members {
            results.extend(access_result(m.resolve(&mb.dom, &mb.item, mb.alt.as_deref()).map(|t| m.get(&t))));
        }
        let mut body = Vec::new();
        if with_spec {
            body.extend(ber::cc(0, choice.raw));
        }
        body.extend(ber::cc(1, &results));
        Ok(ber::cc(4, &body))
    }

    fn write(&mut self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let choice = r.next().ok_or(INVALID)?;
        let data_t = r.opt_ctx(true, 0).ok_or(INVALID)?;
        let mut data = Vec::new();
        let mut rest = data_t.val;
        while !rest.is_empty() {
            let (d, n) = MmsData::decode(rest).map_err(|_| INVALID)?;
            data.push(d);
            rest = &rest[n..];
        }
        let model = self.model.clone();
        let mut m = model.lock().unwrap();
        let members = self.access_spec(&m, &choice)?;
        if members.len() != data.len() {
            return Err(INVALID);
        }
        let mut out = Vec::new();
        let mut trigger = false;
        for (mb, v) in members.iter().zip(data) {
            match m.write_one(mb, v) {
                Ok(()) => {
                    out.extend(ber::cp(1, &[]));
                    if mb.item == REPORT_TRIGGER || mb.item.starts_with("report-trigger$") {
                        trigger = true;
                    }
                }
                Err(code) => out.extend(ber::cp(0, &[code])),
            }
        }
        if trigger {
            self.reports = build_reports(&m);
        }
        Ok(ber::cc(5, &out))
    }

    fn gvaa(&self, val: &[u8]) -> SvcResult {
        let t = Reader::new(val).opt_ctx(true, 0).ok_or(INVALID)?;
        let name = t.children().next().and_then(|n| Name::parse(&n)).ok_or(INVALID)?;
        let Name::Dom(d, i) = name else { return Err(access_err(DAE_NON_EXISTENT)) };
        let m = self.model.lock().unwrap();
        let tg = m.resolve(&d, &i, None).map_err(|_| access_err(DAE_NON_EXISTENT))?;
        let mut ts = BytesMut::new();
        tg.spec.encode(&mut ts).map_err(|_| SvcErr::Class(ErrorClass::Access(0)))?;
        Ok(ber::cc(6, &ber::cat(&[&ber::cp(0, &[0]), &ber::cc(2, &ts)])))
    }

    fn define_nvl(&mut self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let name = r.next().and_then(|n| Name::parse(&n)).ok_or(INVALID)?;
        let list = r.opt_ctx(true, 0).ok_or(INVALID)?;
        let model = self.model.clone();
        let mut m = model.lock().unwrap();
        let members = self.access_spec(&m, &list)?;
        for mb in &members {
            m.resolve(&mb.dom, &mb.item, mb.alt.as_deref()).map_err(|_| SvcErr::Class(ErrorClass::Definition(1)))?;
        }
        let nvl = Nvl { members, deletable: true };
        let map = match &name {
            Name::Vmd(_) => &mut m.vmd_nvls,
            Name::Aa(_) => &mut self.aa,
            Name::Dom(d, _) => &mut m.domains.get_mut(d).ok_or(access_err(DAE_NON_EXISTENT))?.nvls,
        };
        let item = match name {
            Name::Vmd(i) | Name::Aa(i) | Name::Dom(_, i) => i,
        };
        if map.contains_key(&item) {
            return Err(SvcErr::Class(ErrorClass::Definition(2)));
        }
        map.insert(item, nvl);
        Ok(ber::cp(11, &[]))
    }

    fn nvl_attrs(&self, val: &[u8]) -> SvcResult {
        let name = Reader::new(val).next().and_then(|n| Name::parse(&n)).ok_or(INVALID)?;
        let m = self.model.lock().unwrap();
        let l = self.lookup_nvl(&m, &name)?;
        let mut list = Vec::new();
        for mb in &l.members {
            let mut entry = ber::cc(0, &Name::Dom(mb.dom.clone(), mb.item.clone()).encode());
            if let Some(a) = &mb.alt {
                entry.extend(ber::cc(5, a));
            }
            list.extend(ber::seq(&entry));
        }
        Ok(ber::cc(12, &ber::cat(&[&ber::cp(0, &ber::bool_bytes(l.deletable)), &ber::cc(1, &list)])))
    }

    /// scopeOfDelete specific only, as libiec61850.
    fn delete_nvl(&mut self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        if let Some(s) = r.opt_ctx(false, 0) {
            if s.int() != 0 {
                return Err(access_err(DAE_ACCESS_UNSUPPORTED));
            }
        }
        let (mut matched, mut deleted) = (0u64, 0u64);
        let model = self.model.clone();
        let mut m = model.lock().unwrap();
        if let Some(list) = r.opt_ctx(true, 1) {
            for n in list.children() {
                let Some(name) = Name::parse(&n) else { return Err(INVALID) };
                let (map, item) = match name {
                    Name::Vmd(i) => (&mut m.vmd_nvls, i),
                    Name::Aa(i) => (&mut self.aa, i),
                    Name::Dom(d, i) => match m.domains.get_mut(&d) {
                        Some(dm) => (&mut dm.nvls, i),
                        None => continue,
                    },
                };
                if let Some(l) = map.get(&item) {
                    matched += 1;
                    if l.deletable {
                        map.remove(&item);
                        deleted += 1;
                    }
                }
            }
        }
        Ok(ber::cc(13, &ber::cat(&[&ber::cp(0, &ber::uint_bytes(matched)), &ber::cp(1, &ber::uint_bytes(deleted))])))
    }

    fn read_journal(&self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let jn = r.opt_ctx(true, 0).ok_or(INVALID)?;
        let name = jn.children().next().and_then(|n| Name::parse(&n)).ok_or(INVALID)?;
        let (mut start, mut end, mut limit, mut after) = (0u64, u64::MAX, None::<usize>, None::<u64>);
        for t in r {
            let mut inner = t.children();
            match (t.cons, t.num) {
                (true, 1) => {
                    if let Some(b) = inner.opt_ctx(false, 0) {
                        start = timefmt::binary_time_ms(b.val) as u64;
                    }
                }
                (true, 2) => {
                    if let Some(b) = inner.opt_ctx(false, 0) {
                        end = timefmt::binary_time_ms(b.val) as u64;
                    } else if let Some(n) = inner.opt_ctx(false, 1) {
                        limit = Some(n.int().max(0) as usize);
                    }
                }
                (true, 5) => {
                    let tb = inner.opt_ctx(false, 0).ok_or(INVALID)?;
                    let eb = inner.opt_ctx(false, 1).ok_or(INVALID)?;
                    start = timefmt::binary_time_ms(tb.val) as u64;
                    let mut id8 = [0u8; 8];
                    for (i, b) in eb.val.iter().take(8).enumerate() {
                        id8[i] = *b;
                    }
                    after = Some(u64::from_le_bytes(id8));
                }
                _ => {}
            }
        }
        let m = self.model.lock().unwrap();
        let entries = match &name {
            Name::Dom(d, i) => m.domains.get(d).and_then(|dm| dm.journals.get(i)),
            _ => None,
        }
        .ok_or(access_err(DAE_NON_EXISTENT))?;
        let mut sel = Vec::new();
        let mut found = false;
        for e in entries {
            match after {
                Some(a) => {
                    if found && e.time_ms >= start {
                        sel.push(e);
                    }
                    if e.id == a {
                        found = true;
                    }
                }
                None if e.time_ms >= start && e.time_ms <= end => sel.push(e),
                None => {}
            }
        }
        if let Some(l) = limit {
            sel.truncate(l);
        }
        let resp = ReadJournalResponse {
            entries: sel
                .iter()
                .map(|e| WireJournalEntry {
                    entry_id: e.id.to_le_bytes(),
                    occurence_time_ms: e.time_ms,
                    variables: vec![WireJournalVariable {
                        data_ref: e.tag.clone(),
                        value: e.value.clone(),
                        reason_code: reason_byte(e.reason),
                    }],
                })
                .collect(),
            more_follows: None,
        };
        let mut b = BytesMut::new();
        resp.encode(&mut b);
        Ok(b.to_vec())
    }

    // --- files ------------------------------------------------------------

    fn file_attrs(size: usize, mtime_ms: i64) -> Vec<u8> {
        ber::cc(
            1,
            &ber::cat(&[&ber::cp(0, &ber::uint_bytes(size as u64)), &ber::cp(1, ber::gen_time(mtime_ms).as_bytes())]),
        )
    }

    fn file_directory(&self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let spec = match r.opt_ctx(true, 0) {
            Some(t) => ber::parse_file_name(t.val).ok_or(INVALID)?,
            None => String::new(),
        };
        let after = match r.opt_ctx(true, 1) {
            Some(t) => Some(ber::parse_file_name(t.val).ok_or(INVALID)?),
            None => None,
        };
        let spec = spec.trim_start_matches('/').to_string();
        let m = self.model.lock().unwrap();
        let mut entries: Vec<(&String, &FileEnt)> = if spec.is_empty() {
            m.files.iter().collect()
        } else if let Some(f) = m.files.get_key_value(&spec) {
            vec![f]
        } else {
            let prefix = format!("{}/", spec.trim_end_matches('/'));
            let v: Vec<_> = m.files.iter().filter(|(k, _)| k.starts_with(&prefix)).collect();
            if v.is_empty() || spec.ends_with('\\') {
                return Err(FILE_NON_EXISTENT);
            }
            v
        };
        if let Some(a) = after {
            if let Some(i) = entries.iter().position(|(k, _)| **k == a) {
                entries.drain(..=i);
            }
        }
        let budget = self.max_pdu().saturating_sub(32);
        let (mut list, mut more) = (Vec::new(), false);
        for (i, (name, f)) in entries.iter().enumerate() {
            let el = ber::seq(&ber::cat(&[&ber::cc(0, &ber::file_name(name)), &Self::file_attrs(f.data.len(), f.mtime_ms)]));
            if i > 0 && list.len() + el.len() > budget {
                more = true;
                break;
            }
            list.extend(el);
        }
        let mut body = ber::cc(0, &ber::seq(&list));
        if more {
            body.extend(ber::cp(1, &ber::bool_bytes(true)));
        }
        Ok(ber::cc(77, &body))
    }

    fn file_open(&mut self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let name = r.opt_ctx(true, 0).and_then(|t| ber::parse_file_name(t.val)).ok_or(INVALID)?;
        let pos = r.opt_ctx(false, 1).map(|t| t.uint()).unwrap_or(0) as usize;
        let m = self.model.lock().unwrap();
        let f = m.files.get(name.trim_start_matches('/')).ok_or(FILE_NON_EXISTENT)?;
        if pos > f.data.len() {
            return Err(FILE_POSITION_INVALID);
        }
        let attrs = Self::file_attrs(f.data.len(), f.mtime_ms);
        let data = f.data.clone();
        drop(m);
        let id = self.next_frsm;
        self.next_frsm += 1;
        self.files.insert(id, OpenFile { data, pos });
        Ok(ber::cc(72, &ber::cat(&[&ber::cp(0, &ber::int_bytes(id)), &attrs])))
    }

    fn file_read(&mut self, val: &[u8]) -> SvcResult {
        let id = ber::parse_int(val);
        let chunk = self.max_pdu().saturating_sub(32).clamp(1, 65000);
        let f = self.files.get_mut(&id).ok_or(FILE_OTHER)?;
        let end = (f.pos + chunk).min(f.data.len());
        let data = f.data[f.pos..end].to_vec();
        f.pos = end;
        let more = f.pos < f.data.len();
        Ok(ber::cc(73, &ber::cat(&[&ber::cp(0, &data), &ber::cp(1, &ber::bool_bytes(more))])))
    }

    fn file_close(&mut self, val: &[u8]) -> SvcResult {
        self.files.remove(&ber::parse_int(val)).ok_or(FILE_OTHER)?;
        Ok(ber::cp(74, &[]))
    }

    fn file_delete(&mut self, val: &[u8]) -> SvcResult {
        let name = ber::parse_file_name(val).ok_or(INVALID)?;
        let mut m = self.model.lock().unwrap();
        m.files.remove(name.trim_start_matches('/')).ok_or(FILE_NON_EXISTENT)?;
        Ok(ber::cp(76, &[]))
    }

    fn file_rename(&mut self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        let from = r.opt_ctx(true, 0).and_then(|t| ber::parse_file_name(t.val)).ok_or(INVALID)?;
        let to = r.opt_ctx(true, 1).and_then(|t| ber::parse_file_name(t.val)).ok_or(INVALID)?;
        let mut m = self.model.lock().unwrap();
        let f = m.files.remove(from.trim_start_matches('/')).ok_or(FILE_NON_EXISTENT)?;
        m.files.insert(to.trim_start_matches('/').to_string(), f);
        Ok(ber::cp(75, &[]))
    }

    /// ObtainFile: FileOpen/FileRead/FileClose of sourceFile on the client.
    async fn obtain_file(&mut self, val: &[u8]) -> SvcResult {
        let mut r = Reader::new(val);
        r.opt_ctx(true, 0);
        let src = r.opt_ctx(true, 1).and_then(|t| ber::parse_file_name(t.val)).ok_or(INVALID)?;
        let dst = r.opt_ctx(true, 2).and_then(|t| ber::parse_file_name(t.val)).ok_or(INVALID)?;
        let open = ber::cc(72, &ber::cat(&[&ber::cc(0, &ber::file_name(&src)), &ber::cp(1, &[0])]));
        let resp = self.call(open).await?;
        let frsm = ber::read(&resp)
            .and_then(|(t, _)| t.children().next().map(|x| x.int()))
            .ok_or(FILE_OTHER)?;
        let mut data = Vec::new();
        let result = loop {
            let resp = match self.call(ber::cp(73, &ber::int_bytes(frsm))).await {
                Ok(r) => r,
                Err(e) => break Err(e),
            };
            let Some((t, _)) = ber::read(&resp) else { break Err(FILE_OTHER) };
            let mut rr = t.children();
            let Some(chunk) = rr.opt_ctx(false, 0) else { break Err(FILE_OTHER) };
            data.extend_from_slice(chunk.val);
            let more = rr.opt_ctx(false, 1).map(|b| b.bool()).unwrap_or(true);
            if !more {
                break Ok(());
            }
            if chunk.val.is_empty() {
                break Err(FILE_OTHER);
            }
        };
        let _ = self.call(ber::cp(74, &ber::int_bytes(frsm))).await;
        result?;
        let now = now_ms() / 1000 * 1000;
        self.model.lock().unwrap().files.insert(dst.trim_start_matches('/').to_string(), FileEnt { data, mtime_ms: now });
        Ok(ber::cp(46, &[]))
    }
}

/// The fixture's InformationReports as unconfirmed PDUs, in fixture order.
fn build_reports(m: &Model) -> Vec<Vec<u8>> {
    let mut out = Vec::new();
    for (i, r) in m.reports.iter().enumerate() {
        let mut names = Vec::new();
        let mut values = Vec::new();
        for v in &r.vars {
            let Some((d, it)) = v.split_once('/') else { continue };
            values.extend(access_result(m.resolve(d, it, None).map(|t| m.get(&t))));
            names.extend(ber::seq(&ber::cc(0, &Name::Dom(d.into(), it.into()).encode())));
        }
        let spec = match &r.vmd_list {
            Some(l) => ber::cc(1, &Name::Vmd(l.clone()).encode()),
            None => ber::cc(0, &names),
        };
        out.push(ber::cc(3, &ber::cc(0, &ber::cat(&[&spec, &ber::cc(0, &values)]))));
        jl::log(&format!(
            "sent informationReport {i} ({})",
            r.vmd_list.as_deref().unwrap_or("listOfVariable")
        ));
    }
    out
}

fn now_ms() -> i64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0)
}

// ---------------------------------------------------------------------------
// Connection loop (after iec61850-server lifecycle.rs run_connection)
// ---------------------------------------------------------------------------

async fn serve(stream: TcpStream, model: Arc<Mutex<Model>>) -> R<()> {
    let mut cotp = CotpConnection::accept(stream, CotpOptions::default()).await.map_err(e)?;
    let first = cotp.recv_data().await.map_err(e)?;
    let mut session = IsoSession::new();
    match session.parse_message(&first).map_err(e)? {
        SessionIndication::Connect => {}
        other => return Err(format!("first SPDU is {other:?}")),
    }
    let cp = session.user_data(&first).to_vec();
    let mut pres = IsoPresentation::new();
    let cpr = pres_parse_connect(&mut pres, &cp).map_err(e)?;
    let aarq = cpr.payload(&cp).to_vec();
    let mut acse = AcseConnection::new(None);
    let (ind, initiate) = acse.parse_message(&aarq).map_err(e)?;
    let initiate = bytes::Bytes::copy_from_slice(initiate);
    let refuse = |cotp: &mut CotpConnection<TcpStream>, session: &IsoSession, pres: &IsoPresentation| {
        let mut a = BytesMut::new();
        encode_associate_failed(&mut a);
        let mut c = BytesMut::new();
        encode_cpa(pres, &a, &mut c);
        let mut s = BytesMut::new();
        let _ = sess_encode_accept(session, &c, &mut s);
        let _ = cotp;
        s
    };
    if !matches!(ind, AcseIndication::Associate) {
        let s = refuse(&mut cotp, &session, &pres);
        let _ = cotp.send_data(&s).await;
        return Err(format!("acse {ind:?}"));
    }
    let mut mms = MmsServerConnection::new();
    match parse_message(&mms, &InitDispatcher, &initiate) {
        MessageOutcome::SendInitiateResponse { response_bytes, params } => {
            mms.set_negotiated(params);
            let mut aare = BytesMut::new();
            encode_aare(0, Some(&response_bytes), 3, &mut aare);
            let mut c = BytesMut::new();
            encode_cpa(&pres, &aare, &mut c);
            let mut s = BytesMut::new();
            sess_encode_accept(&session, &c, &mut s).map_err(e)?;
            cotp.send_data(&s).await.map_err(e)?;
        }
        other => {
            let s = refuse(&mut cotp, &session, &pres);
            let _ = cotp.send_data(&s).await;
            return Err(format!("initiate refused: {other:?}"));
        }
    }

    let mut a = Assoc {
        cotp,
        session,
        pres,
        mms,
        model,
        aa: BTreeMap::new(),
        files: HashMap::new(),
        next_frsm: 1,
        next_invoke: 1,
        reports: Vec::new(),
    };
    loop {
        let payload = if a.mms.is_concluded() {
            match tokio::time::timeout(Duration::from_millis(a.mms.conclude_timeout_ms()), a.cotp.recv_data()).await {
                Ok(r) => r.map_err(e)?,
                Err(_) => return Ok(()),
            }
        } else {
            a.cotp.recv_data().await.map_err(e)?
        };
        let mut sess = a.session.clone();
        match sess.parse_message(&payload).map_err(e)? {
            SessionIndication::Data => {}
            SessionIndication::Finish => {
                let mut dn = BytesMut::new();
                encode_disconnect(&[], &mut dn).map_err(e)?;
                a.cotp.send_data(&dn).await.map_err(e)?;
                return Ok(());
            }
            SessionIndication::Abort => return Err("aborted by the peer".into()),
            other => return Err(format!("session {other:?}")),
        }
        let ud = sess.user_data(&payload).to_vec();
        let mut pres = a.pres.clone();
        let pr = parse_user_data(&mut pres, &ud).map_err(e)?;
        let inner = pr.payload(&ud).to_vec();
        if pr.context_id == ACSE_CONTEXT_ID {
            let mut acse = AcseConnection::new(None);
            let (ind, _) = acse.parse_message(&inner).map_err(e)?;
            match ind {
                AcseIndication::ReleaseRequest => {
                    let mut rlre = [0u8; 2];
                    encode_rlre(&mut rlre);
                    let mut p = BytesMut::new();
                    encode_user_data_acse(&a.pres, &rlre, &mut p);
                    let mut s = BytesMut::new();
                    encode_data(&p, &mut s);
                    a.cotp.send_data(&s).await.map_err(e)?;
                }
                AcseIndication::Abort => return Err("aborted by the peer".into()),
                other => return Err(format!("acse {other:?}")),
            }
            continue;
        }
        if pr.context_id != MMS_CONTEXT_ID {
            return Err(format!("presentation context {}", pr.context_id));
        }
        if inner.first() == Some(&0xa0) && !a.mms.is_concluded() {
            for pdu in a.confirmed(&inner).await {
                a.send_mms(&pdu).await?;
            }
            continue;
        }
        match parse_message(&a.mms, &InitDispatcher, &inner) {
            MessageOutcome::SendBytes(b) => a.send_mms(&b).await?,
            MessageOutcome::SendConcludeResponse { response_bytes } => {
                a.send_mms(&response_bytes).await?;
                a.mms.mark_concluded();
                let mut f = BytesMut::new();
                encode_finish(&[], &mut f).map_err(e)?;
                a.cotp.send_data(&f).await.map_err(e)?;
            }
            MessageOutcome::Silent => {}
            other => return Err(format!("unexpected {other:?}")),
        }
    }
}

fn main() {
    jl::handle_meta_flags();
    let mut fixture = "/fixtures/mms/interop.json".to_string();
    let mut port: i64 = 1102;
    let args: Vec<String> = std::env::args().collect();
    for i in 1..args.len().saturating_sub(1) {
        match args[i].as_str() {
            "--fixture" => fixture = args[i + 1].clone(),
            "--port" => port = adapter::atoi(&args[i + 1]),
            "--filestore" => jl::log("--filestore: the file store is kept in memory"),
            _ => {}
        }
    }
    adapter::exit_on_signals();
    let start = now_ms() / 1000 * 1000;
    let model = match load_fixture(&fixture, start) {
        Ok(m) => m,
        Err(err) => {
            jl::log(&format!("error: {err}"));
            jl::fatal(3, &format!("error: fixture load failed: {fixture}"));
        }
    };
    jl::log(&format!(
        "loaded fixture {}: {} domain(s), {} file(s)",
        model.revision,
        model.domains.len(),
        model.files.len()
    ));
    for (dn, d) in &model.domains {
        for (vn, v) in &d.vars {
            jl::log(&format!("  registered {dn}/{vn} ({})", if v.writable { "rw" } else { "ro" }));
        }
    }
    let revision = model.revision.clone();
    let model = Arc::new(Mutex::new(model));
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build().expect("tokio runtime");
    rt.block_on(async move {
        let listener = match TcpListener::bind(("0.0.0.0", port as u16)).await {
            Ok(l) if port > 0 && port <= 65535 => l,
            Ok(_) => jl::fatal(1, &format!("error: invalid port {port}")),
            Err(err) => jl::fatal(1, &format!("error: server failed to start on port {port}: {err}")),
        };
        jl::ready(&format!("0.0.0.0:{port}"), &revision, "");
        jl::log(&format!("listening on port {port}"));
        loop {
            let (stream, peer) = match listener.accept().await {
                Ok(x) => x,
                Err(err) => {
                    jl::log(&format!("accept: {err}"));
                    continue;
                }
            };
            let _ = stream.set_nodelay(true);
            let model = model.clone();
            tokio::spawn(async move {
                jl::log(&format!("association from {peer}"));
                match serve(stream, model).await {
                    Ok(()) => jl::log(&format!("association from {peer} concluded")),
                    Err(err) => jl::log(&format!("association from {peer} ended: {err}")),
                }
            });
        }
    });
}
