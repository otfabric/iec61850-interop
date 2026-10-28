// SPDX-License-Identifier: MIT
//
// Typed value JSON of the GOOSE contract ({"type":T,"value":V}) to and from
// iec61850_model::MmsValue, with the validation messages of the C adapter.

use iec61850_model::MmsValue;
use serde_json::{json, Value};

use crate::{cfmt, timefmt};

/// Bits of a BitString, MSB first.
pub fn bits_string(padding: u8, data: &[u8]) -> String {
    let n = (data.len() * 8).saturating_sub(padding as usize);
    (0..n).map(|i| if data[i / 8] & (0x80 >> (i % 8)) != 0 { '1' } else { '0' }).collect()
}

/// BitString of MSB-first binary digits; None if not binary.
pub fn bits_value(s: &str) -> Option<MmsValue> {
    let n = s.len();
    let mut data = vec![0u8; n.div_ceil(8)];
    for (i, c) in s.chars().enumerate() {
        match c {
            '1' => data[i / 8] |= 0x80 >> (i % 8),
            '0' => {}
            _ => return None,
        }
    }
    let padding = ((8 - n % 8) % 8) as u8;
    Some(MmsValue::BitString { padding, data })
}

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}

pub fn unhex(s: &str) -> Option<Vec<u8>> {
    if s.len() % 2 != 0 {
        return None;
    }
    (0..s.len() / 2).map(|i| u8::from_str_radix(s.get(2 * i..2 * i + 2)?, 16).ok()).collect()
}

/// One typed JSON value to an MmsValue.
pub fn from_json(typed: &Value) -> Result<MmsValue, &'static str> {
    let (t, v) = match (typed.get("type"), typed.get("value")) {
        (Some(Value::String(t)), Some(v)) if typed.is_object() => (t.as_str(), v),
        _ => return Err("value is not {\"type\":..,\"value\":..}"),
    };
    match t {
        "boolean" => v.as_bool().map(MmsValue::Boolean).ok_or("boolean: not a JSON boolean"),
        "integer" => v.as_i64().map(MmsValue::Integer).ok_or("integer: not a JSON integer"),
        "unsigned" => match v.as_i64() {
            Some(n) if (0..=0xffff_ffff).contains(&n) => Ok(MmsValue::Unsigned(n as u64)),
            _ => Err("unsigned: not a 32-bit unsigned integer"),
        },
        "float32" | "float64" => {
            let d = v.as_f64().ok_or("float: not a JSON number")?;
            Ok(if t == "float32" { MmsValue::Float32(d as f32) } else { MmsValue::Float64(d) })
        }
        "visible-string" | "mms-string" => {
            let s = v.as_str().ok_or("string: not a JSON string")?.to_string();
            Ok(if t == "visible-string" { MmsValue::VisibleString(s) } else { MmsValue::MmsString(s) })
        }
        "octet-string" => v.as_str().and_then(unhex).map(MmsValue::OctetString).ok_or("octet-string: not hex"),
        "bit-string" => {
            let s = v.as_str().ok_or("bit-string: not a string")?;
            bits_value(s).ok_or("bit-string: not binary digits")
        }
        "utc-time" | "binary-time" => {
            let ms = v.as_str().and_then(timefmt::parse_ms).ok_or("time: not YYYY-MM-DDThh:mm:ss[.fff]Z")?;
            Ok(if t == "utc-time" {
                MmsValue::UtcTime(timefmt::utc_from_ms(ms))
            } else {
                MmsValue::BinaryTime(timefmt::binary_time_from_ms(ms))
            })
        }
        "array" | "structure" => {
            let items = v.as_array().ok_or("array/structure: not a JSON array")?;
            let vals = items.iter().map(from_json).collect::<Result<Vec<_>, _>>()?;
            Ok(if t == "array" { MmsValue::Array(vals) } else { MmsValue::Structure(vals) })
        }
        _ => Err("unsupported type"),
    }
}

/// A JSON array of typed values.
pub fn list_from_json(v: Option<&Value>) -> Result<Vec<MmsValue>, &'static str> {
    match v {
        Some(Value::Array(a)) => a.iter().map(from_json).collect(),
        _ => Err("values is not a JSON array"),
    }
}

fn typed(t: &str, v: Value) -> Value {
    json!({"type": t, "value": v})
}

/// An MmsValue to typed JSON.
pub fn to_json(v: &MmsValue) -> Value {
    match v {
        MmsValue::Boolean(b) => typed("boolean", json!(b)),
        MmsValue::Integer(i) => typed("integer", json!(i)),
        MmsValue::Unsigned(u) => typed("unsigned", json!(u)),
        MmsValue::Float32(f) => typed("float32", cfmt::f32_json(*f)),
        MmsValue::Float64(d) => typed("float64", cfmt::raw_number(&cfmt::g(*d, 17), *d)),
        MmsValue::VisibleString(s) => typed("visible-string", json!(s)),
        MmsValue::MmsString(s) => typed("mms-string", json!(s)),
        MmsValue::OctetString(b) => typed("octet-string", json!(hex(b))),
        MmsValue::BitString { padding, data } => typed("bit-string", json!(bits_string(*padding, data))),
        MmsValue::UtcTime(raw) => typed("utc-time", json!(timefmt::format_ms(timefmt::utc_ms_rounded(raw)))),
        MmsValue::BinaryTime(b) => typed("binary-time", json!(timefmt::format_ms(timefmt::binary_time_ms(b)))),
        MmsValue::Array(items) => typed("array", Value::Array(items.iter().map(to_json).collect())),
        MmsValue::Structure(items) => typed("structure", Value::Array(items.iter().map(to_json).collect())),
    }
}

/// "0x1001" or decimal (strtoul base 0); None if not a 16-bit number.
pub fn parse_appid(s: &str) -> Option<u16> {
    let n = if let Some(h) = s.strip_prefix("0x").or_else(|| s.strip_prefix("0X")) {
        u64::from_str_radix(h, 16).ok()?
    } else if s.len() > 1 && s.starts_with('0') {
        u64::from_str_radix(&s[1..], 8).ok()?
    } else {
        s.parse::<u64>().ok()?
    };
    u16::try_from(n).ok()
}

/// "01:0c:cd:01:00:01" (any single-character separator).
pub fn parse_mac(s: &str) -> Option<[u8; 6]> {
    let b = s.as_bytes();
    if b.len() != 17 {
        return None;
    }
    let mut out = [0u8; 6];
    for (i, o) in out.iter_mut().enumerate() {
        *o = u8::from_str_radix(s.get(3 * i..3 * i + 2)?, 16).ok()?;
    }
    Some(out)
}

pub fn format_mac(m: &[u8; 6]) -> String {
    format!("{:02x}:{:02x}:{:02x}:{:02x}:{:02x}:{:02x}", m[0], m[1], m[2], m[3], m[4], m[5])
}

/// MAC address of a network interface (sysfs), zeros if unknown.
pub fn iface_mac(iface: &str) -> [u8; 6] {
    std::fs::read_to_string(format!("/sys/class/net/{iface}/address"))
        .ok()
        .and_then(|s| parse_mac(s.trim()))
        .unwrap_or([0; 6])
}

/// The GOOSE ready event.
pub fn goose_ready(role: &str, iface: &str, src: &[u8; 6], gocb_ref: Option<&str>) {
    let mut o = serde_json::Map::new();
    o.insert("event".into(), json!("ready"));
    o.insert("address".into(), json!(iface));
    o.insert("fixture".into(), json!("goose-v1"));
    o.insert("adapter".into(), json!(crate::ADAPTER));
    o.insert("version".into(), json!(crate::jl::version()));
    o.insert("role".into(), json!(role));
    o.insert("iface".into(), json!(iface));
    o.insert("src".into(), json!(format_mac(src)));
    if let Some(r) = gocb_ref.filter(|r| !r.is_empty()) {
        o.insert("gocbRef".into(), json!(r));
    }
    crate::jl::emit(&Value::Object(o));
}

pub fn goose_error(msg: &str) {
    crate::jl::emit(&json!({"event":"error","error":msg}));
}
