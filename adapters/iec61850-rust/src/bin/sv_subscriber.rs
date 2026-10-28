// SPDX-License-Identifier: MIT
//
// iec61850-rust-sv-subscriber — Sampled Values (IEC 61850-9-2) subscriber
// on iec61850-sv.
//
//   --iface IF [--app-id 0xNNNN] [--sv-id ID] [--dst MAC] [--sample-every N]
//   [--fixture PATH]
//
// Frames are read from an iec61850_hal AF_PACKET socket; the savPdu is
// decoded with iec61850_sv::decode_sav_pdu. The adapter reads the Ethernet
// header itself (src/dst, APPID, the Edition 2 S bit of Reserved1) so the
// APPID and destination filters run before any decoding. seqData is decoded
// with the member list of the sv.json stream whose svID matches.
//
// Every accepted ASDU is counted (received/lost/duplicates/outOfOrder per
// APPID and svID, docs/sv.md); only those whose smpCnt is a multiple of
// --sample-every are printed, so the loop keeps up with 4000 frames/s.
//
// Exit codes: 0 on SIGTERM/SIGINT, 1 argument or socket error.

use std::collections::HashMap;
use std::time::{Duration, Instant};

use iec61850_hal::ethernet::linux::AfPacketSocket;
use iec61850_hal::ethernet::{EthernetConfig, EthernetError, EthernetSource};
use iec61850_sv::{decode_sav_pdu, SV_ETHER_TYPE};
use serde_json::{json, Value};

use adapter::{jl, strtol, sv, timefmt, typed};

const PROG: &str = "sv-subscriber";
const DEFAULT_FIXTURE: &str = "/fixtures/sv/sv.json";

/// Sequence state of one stream (APPID, svID).
struct Stream {
    app_id: u16,
    sv_id: String,
    prev: Option<u16>,
    max_seen: u16,
    wrap: Option<u32>,
}

#[derive(Default)]
struct Stats {
    received: u64,
    lost: u64,
    duplicates: u64,
    out_of_order: u64,
}

impl Stream {
    fn count(&mut self, smp_cnt: u16, st: &mut Stats) {
        st.received += 1;
        self.max_seen = self.max_seen.max(smp_cnt);
        let prev = match self.prev {
            None => {
                self.prev = Some(smp_cnt);
                return;
            }
            Some(p) => p as i64,
        };
        let cur = smp_cnt as i64;
        let wrap = self.wrap.map(|w| w as i64).unwrap_or(self.max_seen as i64 + 1);
        let d = cur - prev;
        if d == 1 {
            self.prev = Some(smp_cnt);
        } else if d == 0 {
            st.duplicates += 1;
        } else if d > 1 {
            st.lost += (d - 1) as u64;
            self.prev = Some(smp_cnt);
        } else if prev - cur > wrap / 2 {
            st.lost += ((wrap - 1 - prev) + cur).max(0) as u64;
            self.prev = Some(smp_cnt);
        } else {
            st.out_of_order += 1;
        }
    }
}

fn main() {
    jl::handle_meta_flags();
    let mut a = adapter::Args::new(PROG);
    let mut iface = "eth0".to_string();
    let mut app_id: Option<u16> = None;
    let mut sv_filter: Option<String> = None;
    let mut dst: Option<[u8; 6]> = None;
    let mut every = 1i64;
    let mut fixture: Option<String> = None;
    while let Some(f) = a.next_flag() {
        match f.as_str() {
            "--iface" => iface = a.value(&f),
            "--app-id" => {
                let v = a.value(&f);
                app_id = Some(typed::parse_appid(&v).unwrap_or_else(|| {
                    jl::fatal(1, &format!("{PROG}: --app-id {v} is not a 16-bit number"))
                }));
            }
            "--sv-id" => sv_filter = Some(a.value(&f)),
            "--dst" => {
                let v = a.value(&f);
                dst = Some(
                    typed::parse_mac(&v)
                        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --dst {v} is not a MAC address"))),
                );
            }
            "--sample-every" => every = strtol(&a.value(&f)),
            "--fixture" => fixture = Some(a.value(&f)),
            _ => jl::fatal(1, &format!("{PROG}: unknown argument {f}")),
        }
    }
    if every < 1 {
        jl::fatal(1, &format!("{PROG}: --sample-every must be >= 1"));
    }
    let every = every as u64;
    // An explicit --fixture must load; a missing default only disables decoding.
    let streams_fx = match &fixture {
        Some(p) => sv::load_fixture(p).unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: --fixture {e}"))),
        None => sv::load_fixture(DEFAULT_FIXTURE).unwrap_or_else(|e| {
            jl::log(&format!("{PROG}: {e}; seqData is not decoded"));
            HashMap::new()
        }),
    };

    let mut sock = AfPacketSocket::open(&EthernetConfig::new(iface.clone()).with_recv_timeout(Duration::from_millis(10)))
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: raw socket on {iface} failed (needs CAP_NET_RAW): {e}")));

    // The ETH_P_ALL socket queues frames of every interface until it is
    // bound to IF; drop whatever arrived before.
    let mut buf = vec![0u8; 2048];
    while sock.recv(&mut buf, None).is_ok() {}

    adapter::exit_on_signals();
    let own = typed::iface_mac(&iface);
    sv::ready("subscriber", &iface, &own, None);

    let mut streams: Vec<Stream> = Vec::new();
    let mut stats = Stats::default();
    let mut next_stats = Instant::now() + Duration::from_secs(1);
    loop {
        let now = Instant::now();
        while now >= next_stats {
            jl::emit(&json!({"event":"stats","received":stats.received,"lost":stats.lost,
                             "duplicates":stats.duplicates,"outOfOrder":stats.out_of_order}));
            next_stats += Duration::from_secs(1);
        }
        let n = match sock.recv(&mut buf, None) {
            Ok(n) => n,
            Err(EthernetError::Timeout) => continue,
            Err(e) => {
                jl::log(&format!("{PROG}: recv: {e}"));
                std::thread::sleep(Duration::from_millis(10));
                continue;
            }
        };
        let frame = &buf[..n];
        if n < 14 {
            continue;
        }
        let mut off = 12;
        if frame[12] == 0x81 && frame[13] == 0x00 {
            off += 4;
        }
        if n < off + 10 || u16::from_be_bytes([frame[off], frame[off + 1]]) != SV_ETHER_TYPE {
            continue;
        }
        let apdu = off + 2;
        let frame_app_id = u16::from_be_bytes([frame[apdu], frame[apdu + 1]]);
        if app_id.is_some_and(|x| x != frame_app_id) {
            continue;
        }
        if dst.is_some_and(|d| frame[..6] != d) {
            continue;
        }
        let length = u16::from_be_bytes([frame[apdu + 2], frame[apdu + 3]]) as usize;
        let sim = frame[apdu + 4] & 0x80 != 0;
        let pdu_end = (apdu + length.max(8)).min(n);
        let pdu = match decode_sav_pdu(&frame[apdu + 8..pdu_end]) {
            Ok(p) => p,
            Err(e) => {
                jl::log(&format!("{PROG}: undecodable SV frame: {e}"));
                continue;
            }
        };
        let no_asdu = pdu.asdus.len();
        for (idx, asdu) in pdu.asdus.iter().enumerate() {
            if sv_filter.as_deref().is_some_and(|f| f != asdu.sv_id) {
                continue;
            }
            let fx = streams_fx.get(&asdu.sv_id);
            let si = match streams.iter().position(|s| s.app_id == frame_app_id && s.sv_id == asdu.sv_id) {
                Some(i) => i,
                None => {
                    streams.push(Stream {
                        app_id: frame_app_id,
                        sv_id: asdu.sv_id.clone(),
                        prev: None,
                        max_seen: 0,
                        wrap: fx.map(|f| f.wrap),
                    });
                    streams.len() - 1
                }
            };
            streams[si].count(asdu.smp_cnt, &mut stats);
            if asdu.smp_cnt as u64 % every != 0 {
                continue;
            }
            let values = fx.and_then(|f| sv::decode_seq(&f.members, &asdu.sample));
            let mut ev = json!({
                "event": "sv",
                "appID": format!("0x{:04x}", frame_app_id),
                "dst": typed::format_mac(frame[..6].try_into().unwrap()),
                "src": typed::format_mac(frame[6..12].try_into().unwrap()),
                "svID": asdu.sv_id,
                "datSet": asdu.dat_set,
                "smpCnt": asdu.smp_cnt,
                "confRev": asdu.conf_rev,
                "refrTm": asdu.refr_tm.as_ref().map(timefmt::format_utc_ns),
                "smpSynch": asdu.smp_synch.to_byte(),
                "smpRate": asdu.smp_rate,
                "smpMod": asdu.smp_mod.map(sv::smp_mod_name),
                "simulation": sim,
                "asduIndex": idx,
                "noASDU": no_asdu,
                "values": values,
            });
            if ev["values"].is_null() {
                ev.as_object_mut().unwrap().insert("seqData".into(), Value::String(typed::hex(&asdu.sample)));
            }
            jl::emit(&ev);
        }
    }
}
