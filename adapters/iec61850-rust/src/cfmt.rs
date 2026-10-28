// SPDX-License-Identifier: MIT
//
// printf("%.*g") as glibc writes it, so float output matches the
// libiec61850 adapter byte for byte.

/// `%.{prec}g` of `v`.
pub fn g(v: f64, prec: usize) -> String {
    let p = if prec == 0 { 1 } else { prec };
    if v.is_nan() {
        return if v.is_sign_negative() { "-nan".into() } else { "nan".into() };
    }
    if v.is_infinite() {
        return if v < 0.0 { "-inf".into() } else { "inf".into() };
    }
    if v == 0.0 {
        return if v.is_sign_negative() { "-0".into() } else { "0".into() };
    }
    // Exponent after rounding to p significant digits.
    let e_form = format!("{:.*e}", p - 1, v);
    let (mant, exp) = e_form.split_once('e').unwrap();
    let x: i32 = exp.parse().unwrap();
    if x < -4 || x >= p as i32 {
        let mant = strip_zeros(mant);
        let sign = if x < 0 { '-' } else { '+' };
        format!("{}e{}{:02}", mant, sign, x.abs())
    } else {
        let decimals = (p as i32 - 1 - x).max(0) as usize;
        strip_zeros(&format!("{:.*}", decimals, v))
    }
}

/// `%g` (precision 6).
pub fn g6(v: f64) -> String {
    g(v, 6)
}

fn strip_zeros(s: &str) -> String {
    if s.contains('.') {
        let t = s.trim_end_matches('0');
        t.trim_end_matches('.').to_string()
    } else {
        s.to_string()
    }
}

/// Shortest decimal that reads back as the same f32 (`%.*g`, 1..9 digits,
/// as the C GOOSE adapter does), as a JSON number.
pub fn f32_json(f: f32) -> serde_json::Value {
    let mut s = String::new();
    for prec in 1..=9 {
        s = g(f as f64, prec);
        if s.parse::<f64>().map(|d| d as f32 == f).unwrap_or(false) {
            break;
        }
    }
    raw_number(&s, f as f64)
}

/// A JSON number with the given text, falling back to `fallback`.
pub fn raw_number(text: &str, fallback: f64) -> serde_json::Value {
    if let Ok(n) = serde_json::from_str::<serde_json::Number>(text) {
        return serde_json::Value::Number(n);
    }
    serde_json::Number::from_f64(fallback)
        .map(serde_json::Value::Number)
        .unwrap_or(serde_json::Value::Null)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn cases() {
        assert_eq!(g6(21.5), "21.5");
        assert_eq!(g6(1234.5), "1234.5");
        assert_eq!(g6(0.0001), "0.0001");
        assert_eq!(g6(0.00001), "1e-05");
        assert_eq!(g6(1e6), "1e+06");
        assert_eq!(g6(123456.0), "123456");
        assert_eq!(g(0.1f32 as f64, 9), "0.100000001");
        assert_eq!(g6(-0.25), "-0.25");
    }
}
