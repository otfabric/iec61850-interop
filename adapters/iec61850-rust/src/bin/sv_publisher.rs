// SPDX-License-Identifier: MIT
//
// iec61850-rust-sv-publisher — Sampled Values (IEC 61850-9-2) publisher on
// iec61850-sv.
//
//   --iface IF --app-id 0xNNNN --dst MAC --vlan-id N --vlan-prio N --sv-id ID
//   --dataset REF --conf-rev N --smp-rate N
//   --smp-mod samples-per-period|samples-per-second|seconds-per-sample
//   --no-asdu N [--smp-synch 0|1|2] [--simulation] [--profile 9-2LE|generic]
//   [--frequency HZ] [--report-every N] --values JSON
//
// The savPdu and its ASDUs are encoded by iec61850_sv::encode_sav_pdu and
// framed (802.1Q tag always present) by iec61850_sv::SvFrame; the 9-2LE
// seqData is built by iec61850_sv::NineTwoLE, the generic one with the
// fixed-length member encoding of docs/sv.md (adapter::sv). Frames go out
// on an iec61850_hal AF_PACKET socket.
//
// Pacing is this program's: frame k is due at start + k * noASDU / rate,
// an absolute deadline reached by sleeping (timer slack lowered to 1 ns)
// until shortly before it and spinning the rest. A late frame is sent at
// once and the schedule is not shifted. The thread is pinned to the CPU it
// starts on. upstream's publish_thread needs SCHED_FIFO and owns the loop,
// so it is not used.
//
// --simulation sets the Edition 2 S bit (MSB of Reserved1), for which
// upstream has no API: the adapter sets it in the encoded frame.
//
// Exit codes: 0 stop or signal, 1 argument or socket error.

use std::io::BufRead;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::time::{Duration, Instant, SystemTime};

use bytes::BytesMut;
use iec61850_hal::ethernet::linux::AfPacketSocket;
use iec61850_hal::ethernet::{EthernetConfig, EthernetSink};
use iec61850_model::{MmsValue, Quality};
use iec61850_sv::{
    encode_sav_pdu, Asdu, ChannelSample, NineTwoLE, SavPdu, SmpMod, SmpSynch, SvFrame, SvFrameHeader, VlanPriority,
    VlanTag, CHANNEL_COUNT, MAX_ASDU_PER_FRAME, SV_APP_HEADER_SIZE,
};
use serde_json::{json, Value};

use adapter::{jl, strtol, sv, timefmt, typed};

const PROG: &str = "sv-publisher";

/// Offset of Reserved1 in a VLAN-tagged frame: MACs, 802.1Q tag,
/// EtherType, APPID, Length.
const RESERVED1: usize = 12 + 4 + 2 + 2 + 2;

static STOP: AtomicBool = AtomicBool::new(false);

fn install_signals() {
    extern "C" fn on_signal(_: libc::c_int) {
        STOP.store(true, Ordering::SeqCst);
    }
    unsafe {
        libc::signal(libc::SIGTERM, on_signal as extern "C" fn(libc::c_int) as libc::sighandler_t);
        libc::signal(libc::SIGINT, on_signal as extern "C" fn(libc::c_int) as libc::sighandler_t);
        libc::signal(libc::SIGPIPE, libc::SIG_IGN);
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Profile {
    NineTwoLE,
    Generic,
}

/// seqData of the typed values for the profile.
fn seq_data(profile: Profile, values: &[MmsValue]) -> Result<Vec<u8>, String> {
    match profile {
        Profile::Generic => sv::encode_seq(values),
        Profile::NineTwoLE => {
            if values.len() != 2 * CHANNEL_COUNT {
                return Err(format!(
                    "9-2LE needs {} values (eight integer/bit-string pairs), got {}",
                    2 * CHANNEL_COUNT,
                    values.len()
                ));
            }
            let mut channels = [ChannelSample::new(0, Quality(0)); CHANNEL_COUNT];
            for (i, ch) in channels.iter_mut().enumerate() {
                let v = match &values[2 * i] {
                    MmsValue::Integer(v) => {
                        i32::try_from(*v).map_err(|_| format!("value {}: integer {v} is not INT32", 2 * i))?
                    }
                    _ => return Err(format!("value {}: 9-2LE channel is not an integer", 2 * i)),
                };
                let q = match &values[2 * i + 1] {
                    MmsValue::BitString { padding, data } => sv::quality_word(&typed::bits_string(*padding, data))
                        .and_then(|w| u16::try_from(w).ok())
                        .ok_or_else(|| format!("value {}: quality has bits beyond 15 set", 2 * i + 1))?,
                    _ => return Err(format!("value {}: 9-2LE quality is not a bit-string", 2 * i + 1)),
                };
                *ch = ChannelSample::new(v, Quality(q));
            }
            Ok(NineTwoLE { channels }.to_sample().to_vec())
        }
    }
}

struct Pub {
    sock: AfPacketSocket,
    header: SvFrameHeader,
    profile: Profile,
    sv_id: String,
    dataset: Option<String>,
    conf_rev: u32,
    smp_synch: SmpSynch,
    smp_rate: u16,
    smp_mod: SmpMod,
    no_asdu: u64,
    wrap: u64,
    sample_ns: f64,
    simulation: bool,
    seq: Vec<u8>,
    frames: u64,
    samples: u64,
    report_every: u64,
    t0_sys: SystemTime,
    pdu_buf: BytesMut,
    frame_buf: BytesMut,
    last_send_error: Option<Instant>,
}

impl Pub {
    fn encode(&mut self) -> Result<(), String> {
        let mut asdus = Vec::with_capacity(self.no_asdu as usize);
        for j in 0..self.no_asdu {
            let s = self.samples + j;
            let generic = self.profile == Profile::Generic;
            let refr_tm = generic.then(|| {
                timefmt::utc_from_system(self.t0_sys + Duration::from_nanos((s as f64 * self.sample_ns) as u64))
            });
            asdus.push(Asdu {
                sv_id: self.sv_id.clone(),
                dat_set: if generic { self.dataset.clone() } else { None },
                smp_cnt: (s % self.wrap) as u16,
                conf_rev: self.conf_rev,
                refr_tm,
                smp_synch: self.smp_synch,
                smp_rate: generic.then_some(self.smp_rate),
                sample: self.seq.clone(),
                smp_mod: generic.then_some(self.smp_mod),
                gm_identity: None,
            });
        }
        self.pdu_buf.clear();
        encode_sav_pdu(&SavPdu { asdus }, &mut self.pdu_buf).map_err(|e| e.to_string())?;
        self.header.length = (SV_APP_HEADER_SIZE + self.pdu_buf.len()) as u16;
        let frame = SvFrame { header: self.header.clone(), pdu_bytes: self.pdu_buf.to_vec() };
        self.frame_buf.clear();
        frame.encode(&mut self.frame_buf).map_err(|e| e.to_string())?;
        if self.simulation {
            self.frame_buf[RESERVED1] |= 0x80;
        }
        Ok(())
    }

    /// Builds and sends the next frame; `reason` forces a published event.
    fn send_frame(&mut self, reason: Option<&str>) {
        match self.encode() {
            Ok(()) => {
                if let Err(e) = self.sock.send(&self.frame_buf) {
                    // At up to 4000 frames/s a persistent failure is reported
                    // at most once per second.
                    let now = Instant::now();
                    if self.last_send_error.is_none_or(|t| now - t >= Duration::from_secs(1)) {
                        self.last_send_error = Some(now);
                        typed::goose_error(&format!("send: {e}"));
                    }
                }
            }
            Err(e) => typed::goose_error(&format!("encode: {e}")),
        }
        let before = self.samples;
        self.frames += 1;
        self.samples += self.no_asdu;
        let reason = reason.or_else(|| {
            (self.report_every > 0 && before / self.report_every != self.samples / self.report_every)
                .then_some("periodic")
        });
        if let Some(r) = reason {
            let last = (self.samples - 1) % self.wrap;
            jl::emit(&json!({"event":"published","reason":r,"frames":self.frames,"samples":self.samples,"smpCnt":last}));
        }
    }

    /// One stdin line; false on {"cmd":"stop"}. Sets `pending` on a set.
    fn handle_line(&mut self, line: &str, pending: &mut bool) -> bool {
        let cmd: Value = match serde_json::from_str(line) {
            Ok(v) => v,
            Err(_) => {
                typed::goose_error("stdin: not JSON");
                return true;
            }
        };
        let c = cmd.get("cmd").and_then(Value::as_str).unwrap_or("");
        match c {
            "set" => match typed::list_from_json(cmd.get("values"))
                .map_err(|e| e.to_string())
                .and_then(|v| seq_data(self.profile, &v))
            {
                Ok(seq) => {
                    self.seq = seq;
                    *pending = true;
                }
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

    fn stop(&self) -> ! {
        jl::emit(&json!({"event":"stopped","frames":self.frames,"samples":self.samples}));
        std::process::exit(0)
    }
}

fn main() {
    jl::handle_meta_flags();
    let mut a = adapter::Args::new(PROG);
    let mut iface = "eth0".to_string();
    let mut app_id_s: Option<String> = None;
    let mut dst_s: Option<String> = None;
    let mut sv_id: Option<String> = None;
    let mut dataset = String::new();
    let mut values_json: Option<String> = None;
    let (mut vlan_id, mut vlan_prio, mut conf_rev, mut smp_rate, mut no_asdu, mut smp_synch) =
        (0i64, 4i64, 1i64, 80i64, 1i64, 0i64);
    let mut smp_mod_s = "samples-per-period".to_string();
    let mut profile_s = "9-2LE".to_string();
    let mut frequency_s = "50".to_string();
    let mut report_every = 1000i64;
    let mut simulation = false;
    while let Some(f) = a.next_flag() {
        match f.as_str() {
            "--iface" => iface = a.value(&f),
            "--app-id" => app_id_s = Some(a.value(&f)),
            "--dst" => dst_s = Some(a.value(&f)),
            "--vlan-id" => vlan_id = strtol(&a.value(&f)),
            "--vlan-prio" => vlan_prio = strtol(&a.value(&f)),
            "--sv-id" => sv_id = Some(a.value(&f)),
            "--dataset" => dataset = a.value(&f),
            "--conf-rev" => conf_rev = strtol(&a.value(&f)),
            "--smp-rate" => smp_rate = strtol(&a.value(&f)),
            "--smp-mod" => smp_mod_s = a.value(&f),
            "--no-asdu" => no_asdu = strtol(&a.value(&f)),
            "--smp-synch" => smp_synch = strtol(&a.value(&f)),
            "--profile" => profile_s = a.value(&f),
            "--frequency" => frequency_s = a.value(&f),
            "--report-every" => report_every = strtol(&a.value(&f)),
            "--values" => values_json = Some(a.value(&f)),
            "--simulation" => simulation = true,
            _ => jl::fatal(1, &format!("{PROG}: unknown argument {f}")),
        }
    }
    let need = |o: Option<String>, flag: &str| o.unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: {flag} is required")));
    let app_id_s = need(app_id_s, "--app-id");
    let dst_s = need(dst_s, "--dst");
    let sv_id = need(sv_id, "--sv-id");
    let values_json = need(values_json, "--values");
    if !(0..=4095).contains(&vlan_id)
        || !(0..=7).contains(&vlan_prio)
        || !(0..=u32::MAX as i64).contains(&conf_rev)
        || !(1..=65535).contains(&smp_rate)
        || !(1..=MAX_ASDU_PER_FRAME as i64).contains(&no_asdu)
        || !(0..=255).contains(&smp_synch)
        || report_every < 0
    {
        jl::fatal(
            1,
            &format!(
                "{PROG}: --vlan-id 0..4095, --vlan-prio 0..7, --conf-rev 0..2^32-1, --smp-rate 1..65535, \
                 --no-asdu 1..{MAX_ASDU_PER_FRAME}, --smp-synch 0..255, --report-every >= 0"
            ),
        );
    }
    let profile = match profile_s.as_str() {
        "9-2LE" => Profile::NineTwoLE,
        "generic" => Profile::Generic,
        _ => jl::fatal(1, &format!("{PROG}: --profile {profile_s}: 9-2LE or generic")),
    };
    let smp_mod = sv::parse_smp_mod(&smp_mod_s).unwrap_or_else(|| {
        jl::fatal(1, &format!("{PROG}: --smp-mod {smp_mod_s}: samples-per-period, samples-per-second or seconds-per-sample"))
    });
    let frequency: f64 = frequency_s
        .parse()
        .ok()
        .filter(|f: &f64| f.is_finite() && *f > 0.0)
        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --frequency {frequency_s} is not a positive number")));
    let app_id = typed::parse_appid(&app_id_s)
        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --app-id {app_id_s} is not a 16-bit number")));
    let dst = typed::parse_mac(&dst_s)
        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: --dst {dst_s} is not a MAC address")));
    let values = serde_json::from_str::<Value>(&values_json)
        .map_err(|_| "not JSON".to_string())
        .and_then(|v| typed::list_from_json(Some(&v)).map_err(|e| e.to_string()))
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: --values: {e}")));
    let seq = seq_data(profile, &values).unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: --values: {e}")));

    // Sample rate (samples/s) and smpCnt wrap.
    let (rate, wrap) = match smp_mod {
        SmpMod::SecondsPerSample => (1.0 / smp_rate as f64, 65536u64),
        SmpMod::SamplesPerSecond => (smp_rate as f64, smp_rate as u64),
        _ => {
            let r = smp_rate as f64 * frequency;
            (r, (r.round() as u64).clamp(1, 65536))
        }
    };
    let sample_ns = 1e9 / rate;
    let frame_ns = sample_ns * no_asdu as f64;

    let src = typed::iface_mac(&iface);
    let sock = AfPacketSocket::open(&EthernetConfig::new(iface.clone()))
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: raw socket on {iface} failed (needs CAP_NET_RAW): {e}")));

    let mut p = Pub {
        sock,
        header: SvFrameHeader {
            dst_mac: dst,
            src_mac: src,
            vlan: Some(VlanTag { priority: VlanPriority::new(vlan_prio as u8).unwrap(), vlan_id: vlan_id as u16 }),
            app_id,
            length: 0,
        },
        profile,
        sv_id: sv_id.clone(),
        dataset: (!dataset.is_empty()).then_some(dataset),
        conf_rev: conf_rev as u32,
        smp_synch: SmpSynch::from_byte(smp_synch as u8),
        smp_rate: smp_rate as u16,
        smp_mod,
        no_asdu: no_asdu as u64,
        wrap,
        sample_ns,
        simulation,
        seq,
        frames: 0,
        samples: 0,
        report_every: report_every as u64,
        t0_sys: SystemTime::now(),
        pdu_buf: BytesMut::with_capacity(1536),
        frame_buf: BytesMut::with_capacity(1536),
        last_send_error: None,
    };

    // Sleep precisely: the default 50 us timer slack is a fifth of the
    // 250 us frame interval of 9-2LE. Stay on one CPU: a veth transmits
    // into the backlog of the sending CPU, so a thread that migrates
    // between two frames can reorder them on the wire.
    unsafe {
        libc::prctl(libc::PR_SET_TIMERSLACK, 1 as libc::c_ulong);
        let cpu = libc::sched_getcpu();
        if cpu >= 0 {
            let mut set: libc::cpu_set_t = std::mem::zeroed();
            libc::CPU_SET(cpu as usize, &mut set);
            libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set);
        }
    }
    install_signals();
    sv::ready("publisher", &iface, &src, Some(&sv_id));

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

    let start = Instant::now();
    p.t0_sys = SystemTime::now();
    p.send_frame(Some("start"));
    let mut stdin_open = true;
    let mut pending_set = false;
    const SPIN: Duration = Duration::from_micros(60);
    const MAX_SLEEP: Duration = Duration::from_millis(20);
    loop {
        let deadline = start + Duration::from_nanos((p.frames as f64 * frame_ns) as u64);
        loop {
            if STOP.load(Ordering::Relaxed) {
                p.stop();
            }
            if stdin_open {
                loop {
                    match rx.try_recv() {
                        Ok(line) => {
                            let line = line.trim();
                            if !line.is_empty() && !p.handle_line(line, &mut pending_set) {
                                p.stop();
                            }
                        }
                        Err(mpsc::TryRecvError::Empty) => break,
                        Err(mpsc::TryRecvError::Disconnected) => {
                            stdin_open = false;
                            break;
                        }
                    }
                }
            }
            let now = Instant::now();
            if now >= deadline {
                break;
            }
            let rem = deadline - now;
            if rem > SPIN {
                std::thread::sleep((rem - SPIN).min(MAX_SLEEP));
            } else {
                std::hint::spin_loop();
            }
        }
        let reason = std::mem::take(&mut pending_set).then_some("set");
        p.send_frame(reason);
    }
}
