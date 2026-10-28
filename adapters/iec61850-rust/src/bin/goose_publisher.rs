// SPDX-License-Identifier: MIT
//
// iec61850-rust-goose-publisher — GOOSE publisher on iec61850-goose.
//
//   --iface IF --gocb-ref REF --go-id ID --dataset REF --app-id 0xNNNN
//   --dst MAC --vlan-id N --vlan-prio N --conf-rev N --min-time ms
//   --max-time ms [--test] [--simulation] [--nds-com] --values JSON
//
// Frames are built by iec61850_goose::GoosePublisher (BER PDU and
// 802.1Q-tagged Ethernet frame) and sent on an iec61850_hal AF_PACKET
// socket. The retransmission schedule is this program's: MinTime, then
// doubling, capped at MaxTime (docs/goose.md); upstream's own T0..T3
// phases follow a different curve, so each frame gets an explicit
// timeAllowedToLive of twice the interval to the next frame.
//
// --test sets goosePdu field [7] (GoosePublisher::set_simulation, the
// ASN.1 name of that field). --simulation sets the Edition 2 S bit (MSB
// of Reserved1), for which upstream has no API: the adapter sets it in
// the encoded frame.
//
// Exit codes: 0 stop or signal, 1 argument or socket error.

use std::io::BufRead;
use std::sync::mpsc;
use std::time::{Duration, Instant, SystemTime};

use bytes::BytesMut;
use iec61850_goose::{CommParameters, GooseFrame, GooseFrameHeader, GoosePdu, GoosePublisher, VlanPriority, VlanTag};
use iec61850_hal::ethernet::linux::AfPacketSocket;
use iec61850_hal::ethernet::{EthernetConfig, EthernetSink};
use iec61850_model::MmsValue;
use serde_json::{json, Value};

use adapter::{jl, strtol, timefmt, typed};

const PROG: &str = "goose-publisher";

/// MinTime, 2*MinTime, ... (< MaxTime), MaxTime — as the C adapter.
fn build_schedule(min_ms: u32, max_ms: u32) -> Vec<u32> {
    let (mut min_ms, mut max_ms) = (min_ms, max_ms);
    if min_ms == 0 && max_ms == 0 {
        min_ms = 4;
        max_ms = 1000;
    }
    if min_ms == 0 {
        min_ms = (max_ms / 64).max(1);
    }
    if max_ms < min_ms {
        max_ms = min_ms;
    }
    let mut out = Vec::new();
    let mut d = min_ms;
    while d < max_ms && out.len() < 16 {
        out.push(d);
        d = d.saturating_mul(2);
    }
    out.push(max_ms);
    out
}

/// Encoder: upstream's GoosePublisher, or for an empty datSet (which
/// GoosePublisher::new refuses) the same PDU and frame codecs directly.
enum Encoder {
    Publisher(GoosePublisher),
    Raw { header: GooseFrameHeader, pdu: GoosePdu },
}

struct Pub {
    enc: Encoder,
    values: Vec<MmsValue>,
    st_num: u32,
    sq_num: u32,
    steps: Vec<u32>,
    step: usize,
    next: Instant,
    sock: AfPacketSocket,
    simulation: bool,
}

impl Pub {
    fn set_timestamp(&mut self, t: [u8; 8]) {
        match &mut self.enc {
            Encoder::Publisher(p) => p.set_timestamp(t),
            Encoder::Raw { pdu, .. } => pdu.t = t,
        }
    }

    fn encode(&mut self, tal: u32) -> Result<Vec<u8>, String> {
        match &mut self.enc {
            Encoder::Publisher(p) => {
                p.set_time_allowed_to_live(Some(tal));
                p.publish(&self.values).map(|b| b.to_vec()).map_err(|e| e.to_string())
            }
            Encoder::Raw { header, pdu } => {
                pdu.time_allowed_to_live = tal;
                pdu.st_num = self.st_num;
                pdu.sq_num = self.sq_num;
                pdu.num_dataset_entries = self.values.len() as u32;
                pdu.all_data = self.values.clone();
                let mut pb = BytesMut::new();
                pdu.encode_ber(&mut pb).map_err(|e| e.to_string())?;
                let mut fb = BytesMut::new();
                GooseFrame::new(header.clone(), pb.to_vec()).encode(&mut fb).map_err(|e| e.to_string())?;
                Ok(fb.to_vec())
            }
        }
    }

    fn send_frame(&mut self) {
        let idx = self.step.min(self.steps.len() - 1);
        let interval = self.steps[idx];
        let tal = 2 * interval;
        match self.encode(tal) {
            Ok(mut frame) => {
                if self.simulation {
                    // Reserved1 MSB: APDU = 12 + 4 (802.1Q) + 2 (EtherType); octet 4.
                    frame[12 + 4 + 2 + 4] |= 0x80;
                }
                match self.sock.send(&frame) {
                    Ok(_) => {
                        jl::emit(&json!({"event":"published","stNum":self.st_num,"sqNum":self.sq_num,"timeAllowedToLive":tal}));
                    }
                    Err(e) => typed::goose_error(&format!("send: {e}")),
                }
                self.sq_num = self.sq_num.wrapping_add(1);
                if self.sq_num == 0 {
                    self.sq_num = 1;
                }
            }
            Err(e) => typed::goose_error(&format!("publish: {e}")),
        }
        self.step += 1;
        self.next = Instant::now() + Duration::from_millis(interval as u64);
    }

    fn state_change(&mut self, values: Vec<MmsValue>) {
        self.values = values;
        if let Encoder::Publisher(p) = &mut self.enc {
            p.increase_st_num();
        }
        self.st_num = self.st_num.wrapping_add(1);
        if self.st_num == 0 {
            self.st_num = 1;
        }
        self.sq_num = 0;
        self.step = 0;
        self.set_timestamp(timefmt::utc_from_system(SystemTime::now()));
        self.send_frame();
    }

    /// One stdin line; false on {"cmd":"stop"}.
    fn handle_line(&mut self, line: &str) -> bool {
        let cmd: Value = match serde_json::from_str(line) {
            Ok(v) => v,
            Err(_) => {
                typed::goose_error("stdin: not JSON");
                return true;
            }
        };
        let c = cmd.get("cmd").and_then(Value::as_str).unwrap_or("");
        match c {
            "set" => match typed::list_from_json(cmd.get("values")) {
                Ok(v) => self.state_change(v),
                Err(e) => typed::goose_error(&format!("set: {e}")),
            },
            "stop" => return false,
            _ => {
                let short: String = c.chars().take(100).collect();
                typed::goose_error(&format!("stdin: unknown cmd \"{short}\""));
            }
        }
        true
    }
}

fn main() {
    jl::handle_meta_flags();
    let mut a = adapter::Args::new(PROG);
    let mut iface = "eth0".to_string();
    let mut gocb_ref: Option<String> = None;
    let mut go_id: Option<String> = None;
    let mut dataset = String::new();
    let mut values_json: Option<String> = None;
    let mut app_id_s = "0x0000".to_string();
    let mut dst_s = "01:0c:cd:01:00:00".to_string();
    let (mut vlan_id, mut vlan_prio, mut conf_rev, mut min_time, mut max_time) = (0i64, 4i64, 1i64, 10i64, 1000i64);
    let (mut test, mut simulation, mut nds_com) = (false, false, false);
    while let Some(f) = a.next_flag() {
        match f.as_str() {
            "--iface" => iface = a.value(&f),
            "--gocb-ref" => gocb_ref = Some(a.value(&f)),
            "--go-id" => go_id = Some(a.value(&f)),
            "--dataset" => dataset = a.value(&f),
            "--app-id" => app_id_s = a.value(&f),
            "--dst" => dst_s = a.value(&f),
            "--vlan-id" => vlan_id = strtol(&a.value(&f)),
            "--vlan-prio" => vlan_prio = strtol(&a.value(&f)),
            "--conf-rev" => conf_rev = strtol(&a.value(&f)),
            "--min-time" => min_time = strtol(&a.value(&f)),
            "--max-time" => max_time = strtol(&a.value(&f)),
            "--values" => values_json = Some(a.value(&f)),
            "--test" => test = true,
            "--simulation" => simulation = true,
            "--nds-com" => nds_com = true,
            _ => jl::fatal(1, &format!("{PROG}: unknown argument {f}")),
        }
    }
    let gocb_ref = gocb_ref.unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --gocb-ref is required")));
    let values_json = values_json.unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --values is required")));
    if !(0..=4095).contains(&vlan_id)
        || !(0..=7).contains(&vlan_prio)
        || conf_rev < 0
        || min_time < 0
        || max_time < 0
    {
        jl::fatal(1, &format!("{PROG}: --vlan-id 0..4095, --vlan-prio 0..7, times and conf-rev >= 0"));
    }
    let values = serde_json::from_str::<Value>(&values_json)
        .map_err(|_| "not JSON")
        .and_then(|v| typed::list_from_json(Some(&v)))
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: --values: {e}")));
    let app_id = typed::parse_appid(&app_id_s)
        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --app-id {app_id_s} is not a 16-bit number")));
    let dst = typed::parse_mac(&dst_s)
        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --dst {dst_s} is not a MAC address")));

    let src = typed::iface_mac(&iface);
    let sock = AfPacketSocket::open(&EthernetConfig::new(iface.clone()))
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: raw socket on {iface} failed (needs CAP_NET_RAW): {e}")));

    let vlan = VlanTag { priority: VlanPriority::new(vlan_prio as u8).unwrap(), vlan_id: vlan_id as u16 };
    let comm = CommParameters::new(app_id, dst).with_vlan(vlan).with_src_mac(src);
    let go_id = go_id.unwrap_or_else(|| gocb_ref.clone());
    let enc = if dataset.is_empty() {
        Encoder::Raw {
            header: GooseFrameHeader { dst_mac: dst, src_mac: src, vlan: Some(vlan), app_id, length: 8 },
            pdu: GoosePdu {
                gocb_ref: gocb_ref.clone(),
                time_allowed_to_live: 0,
                dat_set: String::new(),
                go_id: Some(go_id.clone()),
                t: [0; 8],
                st_num: 1,
                sq_num: 0,
                simulation: test,
                conf_rev: conf_rev as u32,
                nds_com,
                num_dataset_entries: 0,
                all_data: Vec::new(),
            },
        }
    } else {
        let mut p = GoosePublisher::new(comm, gocb_ref.clone(), Some(go_id), dataset.clone(), conf_rev as u32)
            .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: GoosePublisher: {e}")));
        p.set_simulation(test);
        p.set_needs_commission(nds_com);
        Encoder::Publisher(p)
    };

    let mut p = Pub {
        enc,
        values,
        st_num: 1,
        sq_num: 0,
        steps: build_schedule(min_time as u32, max_time as u32),
        step: 0,
        next: Instant::now(),
        sock,
        simulation,
    };
    // State 1 starts now.
    p.set_timestamp(timefmt::utc_from_system(SystemTime::now()));

    adapter::exit_on_signals();
    typed::goose_ready("publisher", &iface, &src, Some(&gocb_ref));
    p.send_frame();

    let (tx, rx) = mpsc::channel::<String>();
    std::thread::spawn(move || {
        let stdin = std::io::stdin();
        for line in stdin.lock().lines() {
            match line {
                Ok(l) => {
                    if tx.send(l).is_err() {
                        return;
                    }
                }
                Err(_) => break,
            }
        }
        jl::log(&format!("{PROG}: stdin closed; publishing until SIGTERM"));
    });

    let mut stdin_open = true;
    loop {
        let now = Instant::now();
        if now >= p.next {
            p.send_frame();
            continue;
        }
        let wait = p.next - now;
        if !stdin_open {
            std::thread::sleep(wait);
            continue;
        }
        match rx.recv_timeout(wait) {
            Ok(line) => {
                let line = line.trim();
                if !line.is_empty() && !p.handle_line(line) {
                    std::process::exit(0);
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
            Err(mpsc::RecvTimeoutError::Disconnected) => stdin_open = false,
        }
    }
}
