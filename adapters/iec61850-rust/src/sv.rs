// SPDX-License-Identifier: MIT
//
// Sampled Values helpers shared by the SV publisher and subscriber: the
// seqData fixed-length member encoding of IEC 61850-9-2 (docs/sv.md), the
// quality word orientation, smpMod names and the sv.json fixture.

use std::collections::HashMap;

use iec61850_model::MmsValue;
use iec61850_sv::SmpMod;
use serde_json::{json, Value};

use crate::{cfmt, timefmt, typed};

/// Fixture revision of fixtures/sv/.
pub const FIXTURE: &str = "sv-v1";

/// Number of quality bits reported (bit-string length).
pub const QUALITY_BITS: usize = 13;

/// Member types of the fixed-length seqData encoding.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Member {
    Integer,
    Unsigned,
    Float32,
    Float64,
    Boolean,
    Quality,
    UtcTime,
}

impl Member {
    pub fn parse(t: &str) -> Option<Member> {
        Some(match t {
            "integer" => Member::Integer,
            "unsigned" => Member::Unsigned,
            "float32" => Member::Float32,
            "float64" => Member::Float64,
            "boolean" => Member::Boolean,
            "bit-string" => Member::Quality,
            "utc-time" => Member::UtcTime,
            _ => return None,
        })
    }

    pub fn size(self) -> usize {
        match self {
            Member::Integer | Member::Unsigned | Member::Float32 | Member::Quality => 4,
            Member::Float64 | Member::UtcTime => 8,
            Member::Boolean => 1,
        }
    }
}

/// Quality bit-string (position i = bit i from the LSB) to the 32-bit word.
pub fn quality_word(bits: &str) -> Option<u32> {
    if bits.len() > 32 {
        return None;
    }
    let mut w = 0u32;
    for (i, c) in bits.chars().enumerate() {
        match c {
            '1' => w |= 1 << i,
            '0' => {}
            _ => return None,
        }
    }
    Some(w)
}

/// The 13-character bit-string of a quality word (bits 13..31 dropped).
pub fn quality_bits(w: u32) -> String {
    (0..QUALITY_BITS).map(|i| if w & (1 << i) != 0 { '1' } else { '0' }).collect()
}

/// Appends one typed value in the fixed-length encoding.
pub fn encode_value(v: &MmsValue, out: &mut Vec<u8>) -> Result<(), String> {
    match v {
        MmsValue::Integer(i) => {
            let i = i32::try_from(*i).map_err(|_| format!("integer {i} is not INT32"))?;
            out.extend_from_slice(&i.to_be_bytes());
        }
        MmsValue::Unsigned(u) => {
            let u = u32::try_from(*u).map_err(|_| format!("unsigned {u} is not INT32U"))?;
            out.extend_from_slice(&u.to_be_bytes());
        }
        MmsValue::Float32(f) => out.extend_from_slice(&f.to_bits().to_be_bytes()),
        MmsValue::Float64(d) => out.extend_from_slice(&d.to_bits().to_be_bytes()),
        MmsValue::Boolean(b) => out.push(*b as u8),
        MmsValue::BitString { padding, data } => {
            let w = quality_word(&typed::bits_string(*padding, data))
                .ok_or_else(|| "bit-string: more than 32 bits".to_string())?;
            out.extend_from_slice(&w.to_be_bytes());
        }
        MmsValue::UtcTime(raw) => out.extend_from_slice(raw),
        _ => return Err("type not supported in seqData (integer, unsigned, float32, float64, boolean, bit-string, utc-time)".into()),
    }
    Ok(())
}

/// seqData of a list of typed values.
pub fn encode_seq(values: &[MmsValue]) -> Result<Vec<u8>, String> {
    let mut out = Vec::with_capacity(values.len() * 4);
    for (i, v) in values.iter().enumerate() {
        encode_value(v, &mut out).map_err(|e| format!("value {i}: {e}"))?;
    }
    Ok(out)
}

/// Typed JSON values of seqData decoded with a member list; None when the
/// length does not match.
pub fn decode_seq(members: &[Member], data: &[u8]) -> Option<Vec<Value>> {
    let need: usize = members.iter().map(|m| m.size()).sum();
    if need != data.len() {
        return None;
    }
    let mut out = Vec::with_capacity(members.len());
    let mut p = 0;
    for m in members {
        let b = &data[p..p + m.size()];
        p += m.size();
        let w4 = || u32::from_be_bytes([b[0], b[1], b[2], b[3]]);
        out.push(match m {
            Member::Integer => json!({"type":"integer","value":w4() as i32}),
            Member::Unsigned => json!({"type":"unsigned","value":w4()}),
            Member::Float32 => json!({"type":"float32","value":cfmt::f32_json(f32::from_bits(w4()))}),
            Member::Float64 => {
                let d = f64::from_bits(u64::from_be_bytes(b.try_into().unwrap()));
                json!({"type":"float64","value":cfmt::raw_number(&cfmt::g(d, 17), d)})
            }
            Member::Boolean => json!({"type":"boolean","value":b[0] != 0}),
            Member::Quality => json!({"type":"bit-string","value":quality_bits(w4())}),
            Member::UtcTime => {
                let raw: [u8; 8] = b.try_into().unwrap();
                json!({"type":"utc-time","value":timefmt::format_ms(timefmt::utc_ms_rounded(&raw))})
            }
        });
    }
    Some(out)
}

pub fn smp_mod_name(m: SmpMod) -> Value {
    match m {
        SmpMod::PerNominalPeriod => json!("samples-per-period"),
        SmpMod::SamplesPerSecond => json!("samples-per-second"),
        SmpMod::SecondsPerSample => json!("seconds-per-sample"),
        SmpMod::Unknown(b) => json!(b),
    }
}

pub fn parse_smp_mod(s: &str) -> Option<SmpMod> {
    Some(match s {
        "samples-per-period" => SmpMod::PerNominalPeriod,
        "samples-per-second" => SmpMod::SamplesPerSecond,
        "seconds-per-sample" => SmpMod::SecondsPerSample,
        _ => return None,
    })
}

/// A stream of sv.json, as the subscriber needs it.
pub struct FixtureStream {
    pub members: Vec<Member>,
    pub wrap: u32,
}

/// svID -> stream of an sv.json file.
pub fn load_fixture(path: &str) -> Result<HashMap<String, FixtureStream>, String> {
    let text = std::fs::read_to_string(path).map_err(|e| format!("{path}: {e}"))?;
    let v: Value = serde_json::from_str(&text).map_err(|e| format!("{path}: {e}"))?;
    let mut out = HashMap::new();
    let streams = v.get("streams").and_then(Value::as_object).ok_or_else(|| format!("{path}: no streams"))?;
    for (name, s) in streams {
        let sv_id = s.get("svID").and_then(Value::as_str).ok_or_else(|| format!("{path}: {name}: no svID"))?;
        let members = s
            .get("members")
            .and_then(Value::as_array)
            .ok_or_else(|| format!("{path}: {name}: no members"))?
            .iter()
            .map(|m| {
                let t = m.get("type").and_then(Value::as_str).unwrap_or("");
                Member::parse(t).ok_or_else(|| format!("{path}: {name}: member type {t:?} not supported"))
            })
            .collect::<Result<Vec<_>, _>>()?;
        let wrap = s.get("smpCntWrap").and_then(Value::as_u64).unwrap_or(65536).clamp(1, 65536) as u32;
        out.insert(sv_id.to_string(), FixtureStream { members, wrap });
    }
    Ok(out)
}

/// The SV ready event.
pub fn ready(role: &str, iface: &str, src: &[u8; 6], sv_id: Option<&str>) {
    let mut o = serde_json::Map::new();
    o.insert("event".into(), json!("ready"));
    o.insert("address".into(), json!(iface));
    o.insert("fixture".into(), json!(FIXTURE));
    o.insert("adapter".into(), json!(crate::ADAPTER));
    o.insert("version".into(), json!(crate::jl::version()));
    o.insert("role".into(), json!(role));
    o.insert("iface".into(), json!(iface));
    o.insert("src".into(), json!(typed::format_mac(src)));
    if let Some(id) = sv_id {
        o.insert("svID".into(), json!(id));
    }
    crate::jl::emit(&Value::Object(o));
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn quality() {
        assert_eq!(quality_word("0100000000000"), Some(2));
        assert_eq!(quality_word("1100000000000"), Some(3));
        assert_eq!(quality_word("0000000000010"), Some(0x800));
        assert_eq!(quality_bits(0x800), "0000000000010");
        assert_eq!(quality_bits(0xffff_e002), "0100000000000");
    }
}
