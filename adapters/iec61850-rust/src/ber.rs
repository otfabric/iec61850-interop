// SPDX-License-Identifier: MIT
//
// Minimal BER reader and writer for the MMS services iec61850-rust has no
// codec for (Identify, Status, the file services, alternate access with
// index ranges, ...). Values, type specifications and the services it
// does cover are encoded with the iec61850_mms codecs.

/// Tag classes.
pub const UNIV: u8 = 0;
pub const CTX: u8 = 2;

/// One decoded TLV.
#[derive(Debug, Clone, Copy)]
pub struct Tlv<'a> {
    pub cls: u8,
    pub cons: bool,
    pub num: u32,
    /// Content octets.
    pub val: &'a [u8],
    /// The whole TLV, header included.
    pub raw: &'a [u8],
}

impl<'a> Tlv<'a> {
    pub fn is(&self, cls: u8, cons: bool, num: u32) -> bool {
        self.cls == cls && self.cons == cons && self.num == num
    }
    pub fn ctx(&self, cons: bool, num: u32) -> bool {
        self.is(CTX, cons, num)
    }
    pub fn str(&self) -> String {
        String::from_utf8_lossy(self.val).into_owned()
    }
    pub fn uint(&self) -> u64 {
        parse_uint(self.val)
    }
    pub fn int(&self) -> i64 {
        parse_int(self.val)
    }
    pub fn bool(&self) -> bool {
        self.val.first().is_some_and(|b| *b != 0)
    }
    pub fn children(&self) -> Reader<'a> {
        Reader::new(self.val)
    }
}

/// Reads one TLV at the start of `data`; returns it and its total size.
pub fn read(data: &[u8]) -> Option<(Tlv<'_>, usize)> {
    let b0 = *data.first()?;
    let cls = b0 >> 6;
    let cons = b0 & 0x20 != 0;
    let mut pos = 1;
    let mut num = (b0 & 0x1f) as u32;
    if num == 0x1f {
        num = 0;
        loop {
            let b = *data.get(pos)?;
            pos += 1;
            num = (num << 7) | (b & 0x7f) as u32;
            if b & 0x80 == 0 {
                break;
            }
            if pos > 5 {
                return None;
            }
        }
    }
    let l0 = *data.get(pos)?;
    pos += 1;
    let len = if l0 < 0x80 {
        l0 as usize
    } else {
        let n = (l0 & 0x7f) as usize;
        if n == 0 || n > 4 {
            return None;
        }
        let mut l = 0usize;
        for _ in 0..n {
            l = (l << 8) | *data.get(pos)? as usize;
            pos += 1;
        }
        l
    };
    let end = pos.checked_add(len)?;
    if end > data.len() {
        return None;
    }
    Some((Tlv { cls, cons, num, val: &data[pos..end], raw: &data[..end] }, end))
}

/// Sequential reader over concatenated TLVs.
#[derive(Debug, Clone)]
pub struct Reader<'a> {
    data: &'a [u8],
    pos: usize,
    pub bad: bool,
}

impl<'a> Reader<'a> {
    pub fn new(data: &'a [u8]) -> Self {
        Reader { data, pos: 0, bad: false }
    }
    pub fn more(&self) -> bool {
        !self.bad && self.pos < self.data.len()
    }
    pub fn peek(&self) -> Option<Tlv<'a>> {
        if self.bad {
            return None;
        }
        read(&self.data[self.pos..]).map(|(t, _)| t)
    }
    /// The next TLV when it has this context tag.
    pub fn opt_ctx(&mut self, cons: bool, num: u32) -> Option<Tlv<'a>> {
        match self.peek() {
            Some(t) if t.ctx(cons, num) => self.next(),
            _ => None,
        }
    }
}

impl<'a> Iterator for Reader<'a> {
    type Item = Tlv<'a>;
    fn next(&mut self) -> Option<Tlv<'a>> {
        if !self.more() {
            return None;
        }
        match read(&self.data[self.pos..]) {
            Some((t, n)) => {
                self.pos += n;
                Some(t)
            }
            None => {
                self.bad = true;
                None
            }
        }
    }
}

pub fn parse_uint(b: &[u8]) -> u64 {
    b.iter().fold(0u64, |a, x| (a << 8) | *x as u64)
}

pub fn parse_int(b: &[u8]) -> i64 {
    if b.is_empty() {
        return 0;
    }
    let mut v: i64 = if b[0] & 0x80 != 0 { -1 } else { 0 };
    for x in b {
        v = (v << 8) | *x as i64;
    }
    v
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

pub fn tag(cls: u8, cons: bool, num: u32) -> Vec<u8> {
    let first = (cls << 6) | if cons { 0x20 } else { 0 };
    if num < 31 {
        return vec![first | num as u8];
    }
    let mut out = vec![first | 0x1f];
    let mut groups = vec![(num & 0x7f) as u8];
    let mut n = num >> 7;
    while n > 0 {
        groups.push(0x80 | (n & 0x7f) as u8);
        n >>= 7;
    }
    groups.reverse();
    out.extend(groups);
    out
}

pub fn put_len(n: usize, out: &mut Vec<u8>) {
    if n < 0x80 {
        out.push(n as u8);
    } else {
        let bytes = (n as u32).to_be_bytes();
        let skip = bytes.iter().take_while(|b| **b == 0).count();
        out.push(0x80 | (4 - skip) as u8);
        out.extend_from_slice(&bytes[skip..]);
    }
}

pub fn tlv(cls: u8, cons: bool, num: u32, content: &[u8]) -> Vec<u8> {
    let mut out = tag(cls, cons, num);
    put_len(content.len(), &mut out);
    out.extend_from_slice(content);
    out
}

/// Context-specific constructed TLV.
pub fn cc(num: u32, content: &[u8]) -> Vec<u8> {
    tlv(CTX, true, num, content)
}

/// Context-specific primitive TLV.
pub fn cp(num: u32, content: &[u8]) -> Vec<u8> {
    tlv(CTX, false, num, content)
}

pub fn seq(content: &[u8]) -> Vec<u8> {
    tlv(UNIV, true, 16, content)
}

pub fn vstr(s: &str) -> Vec<u8> {
    tlv(UNIV, false, 26, s.as_bytes())
}

/// Minimal two's-complement content of an INTEGER.
pub fn int_bytes(v: i64) -> Vec<u8> {
    let b = v.to_be_bytes();
    let mut i = 0;
    while i < 7 && ((b[i] == 0 && b[i + 1] & 0x80 == 0) || (b[i] == 0xff && b[i + 1] & 0x80 != 0)) {
        i += 1;
    }
    b[i..].to_vec()
}

/// Minimal content of a non-negative INTEGER (leading 0 when the top bit is set).
pub fn uint_bytes(v: u64) -> Vec<u8> {
    let b = v.to_be_bytes();
    let skip = b.iter().take_while(|x| **x == 0).count().min(7);
    let mut out = Vec::new();
    if b[skip] & 0x80 != 0 {
        out.push(0);
    }
    out.extend_from_slice(&b[skip..]);
    out
}

pub fn bool_bytes(v: bool) -> Vec<u8> {
    vec![if v { 0xff } else { 0x00 }]
}

pub fn cat(parts: &[&[u8]]) -> Vec<u8> {
    parts.concat()
}

// ---------------------------------------------------------------------------
// MMS names
// ---------------------------------------------------------------------------

/// An MMS ObjectName.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Name {
    Vmd(String),
    Dom(String, String),
    Aa(String),
}

impl Name {
    pub fn parse(t: &Tlv) -> Option<Name> {
        if t.ctx(false, 0) {
            return Some(Name::Vmd(t.str()));
        }
        if t.ctx(false, 2) {
            return Some(Name::Aa(t.str()));
        }
        if t.ctx(true, 1) {
            let mut r = t.children();
            let d = r.next()?;
            let i = r.next()?;
            return Some(Name::Dom(d.str(), i.str()));
        }
        None
    }

    pub fn encode(&self) -> Vec<u8> {
        match self {
            Name::Vmd(s) => cp(0, s.as_bytes()),
            Name::Aa(s) => cp(2, s.as_bytes()),
            Name::Dom(d, i) => cc(1, &cat(&[&vstr(d), &vstr(i)])),
        }
    }
}

/// FileName ::= SEQUENCE OF GraphicString, given as the content of its
/// (context) tag; the parts are concatenated.
pub fn parse_file_name(content: &[u8]) -> Option<String> {
    let mut s = String::new();
    let mut r = Reader::new(content);
    for t in r.by_ref() {
        s.push_str(&t.str());
    }
    if r.bad {
        return None;
    }
    Some(s)
}

/// Content of a FileName with one GraphicString.
pub fn file_name(s: &str) -> Vec<u8> {
    tlv(UNIV, false, 25, s.as_bytes())
}

/// GeneralizedTime "YYYYMMDDhhmmss.fffZ" of epoch milliseconds.
pub fn gen_time(ms: i64) -> String {
    let secs = ms.div_euclid(1000);
    let (y, mo, d) = crate::timefmt::civil_from_days(secs.div_euclid(86_400));
    let sod = secs.rem_euclid(86_400);
    format!(
        "{:04}{:02}{:02}{:02}{:02}{:02}.{:03}Z",
        y,
        mo,
        d,
        sod / 3600,
        sod % 3600 / 60,
        sod % 60,
        ms.rem_euclid(1000)
    )
}

/// Epoch milliseconds of a GeneralizedTime (UTC forms; a missing Z is UTC).
pub fn parse_gen_time(s: &str) -> Option<i64> {
    let b = s.as_bytes();
    if b.len() < 14 || !b[..14].iter().all(|c| c.is_ascii_digit()) {
        return None;
    }
    let n = |a: usize, l: usize| s[a..a + l].parse::<i64>().ok();
    let days = crate::timefmt::days_from_civil(n(0, 4)?, n(4, 2)? as u32, n(6, 2)? as u32);
    let mut ms = (days * 86_400 + n(8, 2)? * 3600 + n(10, 2)? * 60 + n(12, 2)?) * 1000;
    let rest = &s[14..];
    if let Some(frac) = rest.strip_prefix('.').or_else(|| rest.strip_prefix(',')) {
        let digits: String = frac.chars().take_while(|c| c.is_ascii_digit()).collect();
        let mut f = digits.clone();
        f.truncate(3);
        while f.len() < 3 {
            f.push('0');
        }
        ms += f.parse::<i64>().unwrap_or(0);
    }
    Some(ms)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ints() {
        assert_eq!(int_bytes(0), vec![0]);
        assert_eq!(int_bytes(-1), vec![0xff]);
        assert_eq!(int_bytes(128), vec![0, 0x80]);
        assert_eq!(int_bytes(-129), vec![0xff, 0x7f]);
        assert_eq!(uint_bytes(456), vec![0x01, 0xc8]);
        assert_eq!(uint_bytes(200), vec![0, 200]);
        assert_eq!(parse_int(&[0xff, 0x85]), -123);
    }

    #[test]
    fn tags() {
        assert_eq!(tag(CTX, true, 77), vec![0xbf, 0x4d]);
        assert_eq!(tag(CTX, false, 73), vec![0x9f, 0x49]);
        let enc = cc(77, &[1, 2]);
        let (t, n) = read(&enc).unwrap();
        assert!(t.ctx(true, 77));
        assert_eq!(n, 5);
        let big = vec![0u8; 300];
        let enc = cp(0, &big);
        let (t, _) = read(&enc).unwrap();
        assert_eq!(t.val.len(), 300);
    }

    #[test]
    fn times() {
        assert_eq!(gen_time(1704164645000), "20240102030405.000Z");
        assert_eq!(parse_gen_time("20240102030405.000Z"), Some(1704164645000));
        assert_eq!(parse_gen_time("20240102030405Z"), Some(1704164645000));
    }
}
