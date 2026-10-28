// SPDX-License-Identifier: MIT
//
// UtcTime / BinaryTime helpers and RFC 3339 formatting without a date
// library (days-from-civil arithmetic).

/// (year, month, day) of a day count since 1970-01-01.
pub fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = (z - era * 146097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// Days since 1970-01-01 of a civil date.
pub fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = (y - era * 400) as u64;
    let m = m as u64;
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d as u64 - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe as i64 - 719468
}

/// "YYYY-MM-DDThh:mm:ss" of seconds since the epoch.
pub fn base(secs: i64) -> String {
    let days = secs.div_euclid(86400);
    let sod = secs.rem_euclid(86400);
    let (y, m, d) = civil_from_days(days);
    format!(
        "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}",
        y,
        m,
        d,
        sod / 3600,
        (sod / 60) % 60,
        sod % 60
    )
}

/// "YYYY-MM-DDThh:mm:ss.mmmZ" of ms since the epoch.
pub fn format_ms(ms: i64) -> String {
    format!("{}.{:03}Z", base(ms.div_euclid(1000)), ms.rem_euclid(1000))
}

/// Parses "YYYY-MM-DDThh:mm:ss[.f…]Z" to ms since the epoch (fraction
/// rounded to ms, as the C adapters do).
pub fn parse_ms(s: &str) -> Option<i64> {
    let b = s.as_bytes();
    if b.len() < 20 {
        return None;
    }
    let num = |r: std::ops::Range<usize>| -> Option<i64> {
        let t = s.get(r)?;
        if !t.bytes().all(|c| c.is_ascii_digit()) {
            return None;
        }
        t.parse().ok()
    };
    if b[4] != b'-' || b[7] != b'-' || b[10] != b'T' || b[13] != b':' || b[16] != b':' {
        return None;
    }
    let (y, mo, d, h, mi, se) = (num(0..4)?, num(5..7)?, num(8..10)?, num(11..13)?, num(14..16)?, num(17..19)?);
    let mut p = 19;
    let mut ms = 0i64;
    if b[p] == b'.' {
        p += 1;
        let mut digits = 0;
        let mut ns: i64 = 0;
        while p < b.len() && b[p].is_ascii_digit() {
            if digits < 9 {
                ns = ns * 10 + (b[p] - b'0') as i64;
                digits += 1;
            }
            p += 1;
        }
        while digits < 9 {
            ns *= 10;
            digits += 1;
        }
        ms = (ns + 500_000) / 1_000_000;
    }
    if p + 1 != b.len() || b[p] != b'Z' {
        return None;
    }
    let days = days_from_civil(y, mo as u32, d as u32);
    Some(((days * 86400 + h * 3600 + mi * 60 + se) * 1000) + ms)
}

/// UtcTime octets of ms since the epoch; quality byte as libiec61850
/// (MmsValue_setUtcTimeMs leaves it 0x0a: 10 bits of accuracy).
pub fn utc_from_ms(ms: i64) -> [u8; 8] {
    let secs = ms.div_euclid(1000) as u32;
    let frac = ((ms.rem_euclid(1000) as u64) << 24) / 1000;
    let s = secs.to_be_bytes();
    [s[0], s[1], s[2], s[3], (frac >> 16) as u8, (frac >> 8) as u8, frac as u8, 0x0a]
}

/// UtcTime octets of a system time, full fraction precision.
pub fn utc_from_system(t: std::time::SystemTime) -> [u8; 8] {
    let d = t.duration_since(std::time::UNIX_EPOCH).unwrap_or_default();
    let secs = d.as_secs() as u32;
    let frac = ((d.subsec_nanos() as u64) << 24) / 1_000_000_000;
    let s = secs.to_be_bytes();
    [s[0], s[1], s[2], s[3], (frac >> 16) as u8, (frac >> 8) as u8, frac as u8, 0x0a]
}

fn utc_parts(raw: &[u8; 8]) -> (i64, u64) {
    let secs = u32::from_be_bytes([raw[0], raw[1], raw[2], raw[3]]) as i64;
    let frac = ((raw[4] as u64) << 16) | ((raw[5] as u64) << 8) | raw[6] as u64;
    (secs, (frac * 1_000_000_000) >> 24)
}

/// ms since the epoch of UtcTime octets, rounded to the nearest ms.
pub fn utc_ms_rounded(raw: &[u8; 8]) -> i64 {
    let (secs, ns) = utc_parts(raw);
    secs * 1000 + ((ns + 500_000) / 1_000_000) as i64
}

/// ms since the epoch of UtcTime octets, truncated (libiec61850
/// MmsValue_getUtcTimeInMs).
pub fn utc_ms_trunc(raw: &[u8; 8]) -> i64 {
    let secs = u32::from_be_bytes([raw[0], raw[1], raw[2], raw[3]]) as i64;
    let frac = ((raw[4] as u64) << 16) | ((raw[5] as u64) << 8) | raw[6] as u64;
    secs * 1000 + ((frac * 1000) >> 24) as i64
}

/// RFC 3339 with up to nine fraction digits, trailing zeros dropped.
pub fn format_utc_ns(raw: &[u8; 8]) -> String {
    let (secs, ns) = utc_parts(raw);
    if ns == 0 {
        return format!("{}Z", base(secs));
    }
    let f = format!("{:09}", ns);
    format!("{}.{}Z", base(secs), f.trim_end_matches('0'))
}

/// Seconds between 1970-01-01 and 1984-01-01, the BinaryTime epoch.
pub const BINARY_TIME_EPOCH_S: i64 = 441_763_200;

/// BinaryTime (6 octets) of ms since the epoch.
pub fn binary_time_from_ms(ms: i64) -> Vec<u8> {
    let rel = ms - BINARY_TIME_EPOCH_S * 1000;
    let days = rel.div_euclid(86_400_000);
    let msod = rel.rem_euclid(86_400_000) as u32;
    let mut v = msod.to_be_bytes().to_vec();
    v.extend_from_slice(&(days as u16).to_be_bytes());
    v
}

/// ms since the epoch of BinaryTime octets (4 octets: day 0).
pub fn binary_time_ms(b: &[u8]) -> i64 {
    if b.len() < 4 {
        return 0;
    }
    let msod = u32::from_be_bytes([b[0], b[1], b[2], b[3]]) as i64;
    let days = if b.len() >= 6 { u16::from_be_bytes([b[4], b[5]]) as i64 } else { 0 };
    BINARY_TIME_EPOCH_S * 1000 + days * 86_400_000 + msod
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn roundtrip() {
        let ms = parse_ms("2024-01-01T00:00:01.500Z").unwrap();
        assert_eq!(format_ms(ms), "2024-01-01T00:00:01.500Z");
        assert_eq!(utc_ms_rounded(&utc_from_ms(ms)), ms);
        assert_eq!(format_ms(binary_time_ms(&binary_time_from_ms(ms))), "2024-01-01T00:00:01.500Z");
    }
}
