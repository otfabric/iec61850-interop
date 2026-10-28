// SPDX-License-Identifier: MIT
//
// iec61850-rust-goose-subscriber — GOOSE subscriber on iec61850-goose.
//
//   --iface IF [--app-id 0xNNNN] [--gocb-ref REF] [--dst MAC]
//
// Frames are read from an iec61850_hal AF_PACKET socket and decoded with
// iec61850_goose::GooseFrame::decode and GoosePdu::decode_ber. The adapter
// keeps the Ethernet header for src/dst and the Edition 2 S bit of
// Reserved1 ("simulation"); goosePdu field [7] is reported as "test".
//
// Supervision per gocbRef with the timeAllowedToLive of its last frame:
// {"event":"supervision","gocbRef":REF,"state":"timeout"} when it passes,
// "state":"ok" before the next frame of that gocbRef.
//
// Exit codes: 0 on SIGTERM/SIGINT, 1 argument or socket error.

use std::collections::HashMap;
use std::time::{Duration, Instant};

use iec61850_goose::{GooseFrame, GoosePdu};
use iec61850_hal::ethernet::linux::AfPacketSocket;
use iec61850_hal::ethernet::{EthernetConfig, EthernetError, EthernetSource};
use serde_json::{json, Value};

use adapter::{jl, timefmt, typed};

const PROG: &str = "goose-subscriber";

struct Stream {
    deadline: Option<Instant>,
    timed_out: bool,
}

fn supervision(r: &str, state: &str) {
    jl::emit(&json!({"event":"supervision","gocbRef":r,"state":state}));
}

fn main() {
    jl::handle_meta_flags();
    let mut a = adapter::Args::new(PROG);
    let mut iface = "eth0".to_string();
    let mut app_id: Option<u16> = None;
    let mut gocb_filter: Option<String> = None;
    let mut dst: Option<[u8; 6]> = None;
    while let Some(f) = a.next_flag() {
        match f.as_str() {
            "--iface" => iface = a.value(&f),
            "--app-id" => {
                let v = a.value(&f);
                app_id = Some(typed::parse_appid(&v).unwrap_or_else(|| {
                    jl::fatal(1, &format!("{PROG}: --app-id {v} is not a 16-bit number"))
                }));
            }
            "--gocb-ref" => gocb_filter = Some(a.value(&f)),
            "--dst" => {
                let v = a.value(&f);
                dst = Some(
                    typed::parse_mac(&v)
                        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --dst {v} is not a MAC address"))),
                );
            }
            _ => jl::fatal(1, &format!("{PROG}: unknown argument {f}")),
        }
    }

    let mut sock = AfPacketSocket::open(&EthernetConfig::new(iface.clone()).with_recv_timeout(Duration::from_millis(10)))
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: raw socket on {iface} failed (needs CAP_NET_RAW): {e}")));

    // The ETH_P_ALL socket queues frames of every interface until it is
    // bound to IF; drop whatever arrived before.
    let mut buf = vec![0u8; 2048];
    while sock.recv(&mut buf, None).is_ok() {}

    adapter::exit_on_signals();
    let own = typed::iface_mac(&iface);
    typed::goose_ready("subscriber", &iface, &own, gocb_filter.as_deref());

    let mut streams: HashMap<String, Stream> = HashMap::new();
    let mut order: Vec<String> = Vec::new();
    loop {
        let now = Instant::now();
        for r in &order {
            let s = streams.get_mut(r).unwrap();
            if let Some(d) = s.deadline {
                if !s.timed_out && now >= d {
                    s.timed_out = true;
                    supervision(r, "timeout");
                }
            }
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
        if n < off + 10 || frame[off] != 0x88 || frame[off + 1] != 0xb8 {
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
        let sim = frame[apdu + 4] & 0x80 != 0;
        let decoded = GooseFrame::decode(frame).and_then(|f| GoosePdu::decode_ber(&f.pdu_bytes).map(|p| (f, p)));
        let (gf, pdu) = match decoded {
            Ok(x) => x,
            Err(e) => {
                jl::log(&format!("{PROG}: undecodable GOOSE frame: {e}"));
                continue;
            }
        };
        if gocb_filter.as_deref().is_some_and(|g| g != pdu.gocb_ref) {
            continue;
        }
        let tal = pdu.time_allowed_to_live;
        if !streams.contains_key(&pdu.gocb_ref) {
            streams.insert(pdu.gocb_ref.clone(), Stream { deadline: None, timed_out: false });
            order.push(pdu.gocb_ref.clone());
        }
        let s = streams.get_mut(&pdu.gocb_ref).unwrap();
        if s.timed_out {
            s.timed_out = false;
            supervision(&pdu.gocb_ref, "ok");
        }
        s.deadline = if tal > 0 { Some(Instant::now() + Duration::from_millis(tal as u64)) } else { None };

        let values: Vec<Value> = pdu.all_data.iter().map(typed::to_json).collect();
        let ev = json!({
            "event": "goose",
            "gocbRef": pdu.gocb_ref,
            "datSet": pdu.dat_set,
            "goID": pdu.go_id.clone().unwrap_or_default(),
            "appID": format!("0x{:04x}", gf.header.app_id),
            "dst": typed::format_mac(&gf.header.dst_mac),
            "src": typed::format_mac(&gf.header.src_mac),
            "stNum": pdu.st_num,
            "sqNum": pdu.sq_num,
            "timeAllowedToLive": tal,
            "confRev": pdu.conf_rev,
            "test": pdu.simulation,
            "simulation": sim,
            "ndsCom": pdu.nds_com,
            "numDatSetEntries": pdu.all_data.len(),
            "t": timefmt::format_utc_ns(&pdu.t),
            "values": values,
        });
        jl::emit(&ev);
    }
}
