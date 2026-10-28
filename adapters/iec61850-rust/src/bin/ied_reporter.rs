// SPDX-License-Identifier: MIT
//
// iec61850-rust-ied-reporter — URCB client on iec61850_client (RCB API and
// report dispatcher), the counterpart of libiec61850-ied-reporter.
//
//   iec61850-rust-ied-reporter [--host H] [--port P] [--sps1-initial 0|1]
//
// Sequence: get-rcb InteropLD/LLN0.RP.urcb01, enable-rcb (RptEna=true),
// write InteropLD/GGIO1.SPS1.stVal[ST] = !initial, receive-report (10 s),
// disable-rcb, conclude. References are the bare InteropLD, as in the C;
// IEC61850_RUST_LD replaces "InteropLD" and IEC61850_RUST_RCB "urcb01"
// (test aids, not part of the contract).
//
// Exit codes: 0 sequence done, 1 argument error, 2 connection failure.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use iec61850_client::{ClientReport, RcbWriteMask, ReasonForInclusion, ReportCallback};
use iec61850_model::{MmsValue, FC};

use adapter::ied::{emit_error, err_str};
use adapter::jl;

const PROG: &str = "ied-reporter";
const RPT_ID: &str = "interop_urcb01";
const DS_SIZE: usize = 2;

fn reason_str(r: ReasonForInclusion) -> &'static str {
    if r == ReasonForInclusion::DATA_CHANGE {
        "data-change"
    } else if r == ReasonForInclusion::QUALITY_CHANGE {
        "quality-change"
    } else if r == ReasonForInclusion::DATA_UPDATE {
        "data-update"
    } else if r == ReasonForInclusion::INTEGRITY {
        "integrity"
    } else if r == ReasonForInclusion::GI {
        "gi"
    } else {
        "unknown"
    }
}

fn emit_report(r: &ClientReport) {
    let included: Vec<bool> = (0..DS_SIZE)
        .map(|i| r.data_set_values.get(i).map(|v| v.is_some()).unwrap_or(false))
        .collect();
    let mut values = Vec::new();
    let mut reasons = Vec::new();
    for (i, inc) in included.iter().enumerate() {
        if !inc {
            continue;
        }
        let v = r.data_set_values[i].as_ref();
        values.push(match (i, v) {
            (0, Some(MmsValue::Boolean(b))) => b.to_string(),
            (1, Some(MmsValue::Integer(n))) => (*n as i32).to_string(),
            _ => "null".to_string(),
        });
        reasons.push(jl::q(reason_str(r.reasons.get(i).copied().unwrap_or_default())));
    }
    let inc: Vec<String> = included.iter().map(|b| b.to_string()).collect();
    jl::line(&format!(
        "{{\"operation\":\"receive-report\",\"ok\":true,\"rptID\":{},\"seqNum\":{},\"inclusion\":[{}],\"values\":[{}],\"reasons\":[{}]}}",
        jl::q(r.rpt_id.as_deref().unwrap_or("")),
        r.seq_num.unwrap_or(0),
        inc.join(","),
        values.join(","),
        reasons.join(",")
    ));
}

fn main() {
    jl::handle_meta_flags();
    let mut initial = true;
    let (host, port) = adapter::ied::host_port(PROG, |f, v| {
        if f == "--sps1-initial" {
            initial = adapter::atoi(v) != 0;
        }
    });
    let ld = std::env::var("IEC61850_RUST_LD").unwrap_or_else(|_| "InteropLD".into());
    let rcb = std::env::var("IEC61850_RUST_RCB").unwrap_or_else(|_| "urcb01".into());
    let rcb_ref = format!("{ld}/LLN0.RP.{rcb}");
    let sps1_ref = format!("{ld}/GGIO1.SPS1.stVal");

    let rt = adapter::ied::runtime();
    let code = rt.block_on(async {
        let conn = match adapter::ied::connect(&host, port).await {
            Ok(c) => c,
            Err(e) => {
                jl::log(&format!("{PROG}: connect {host}:{port}: {e}"));
                return 2;
            }
        };

        // 1. get-rcb
        let mut rcb = match conn.get_rcb_values(&rcb_ref).await {
            Ok(r) => r,
            Err(e) => {
                emit_error("get-rcb", Some(&rcb_ref), &err_str(&e, false));
                let _ = conn.disconnect().await;
                return 0;
            }
        };
        jl::line(&format!(
            "{{\"operation\":\"get-rcb\",\"target\":{},\"ok\":true,\"rptID\":{},\"rptEna\":{}}}",
            jl::q(&rcb_ref),
            jl::q(rcb.rpt_id().unwrap_or("")),
            rcb.rpt_ena()
        ));

        // 2. enable-rcb
        let first: Arc<Mutex<Option<ClientReport>>> = Arc::new(Mutex::new(None));
        let notify = Arc::new(tokio::sync::Notify::new());
        let (f2, n2) = (first.clone(), notify.clone());
        let cb: ReportCallback = Arc::new(move |r: Arc<ClientReport>| {
            let mut g = f2.lock().unwrap();
            if g.is_none() {
                *g = Some((*r).clone());
                n2.notify_one();
            }
        });
        if let Err(e) = conn.install_report_handler(Some(RPT_ID.into()), &rcb_ref, None, cb).await {
            jl::log(&format!("{PROG}: install report handler: {e}"));
        }
        let dispatcher = conn.spawn_report_dispatcher(Duration::from_millis(50));
        rcb.set_rpt_ena(true);
        if let Err(e) = conn.set_rcb_values(&rcb, RcbWriteMask::RPT_ENA, true).await {
            emit_error("enable-rcb", Some(&rcb_ref), &err_str(&e, false));
            let _ = conn.disconnect().await;
            dispatcher.abort();
            return 0;
        }
        jl::line(&format!("{{\"operation\":\"enable-rcb\",\"target\":{},\"ok\":true}}", jl::q(&rcb_ref)));

        // 3. write
        let t = format!("{sps1_ref}[ST]");
        match conn.write_boolean(&sps1_ref, FC::St, !initial).await {
            Ok(()) => jl::line(&format!("{{\"operation\":\"write\",\"target\":{},\"ok\":true}}", jl::q(&t))),
            Err(e) => emit_error("write", Some(&t), &err_str(&e, false)),
        }

        // 4. receive-report
        let got = tokio::time::timeout(Duration::from_secs(10), async {
            loop {
                if let Some(r) = first.lock().unwrap().clone() {
                    return r;
                }
                notify.notified().await;
            }
        })
        .await;
        match got {
            Ok(r) => emit_report(&r),
            Err(_) => emit_error("receive-report", None, "timeout-waiting-for-dchg-report"),
        }

        // 5. disable-rcb
        rcb.set_rpt_ena(false);
        match conn.set_rcb_values(&rcb, RcbWriteMask::RPT_ENA, true).await {
            Ok(()) => jl::line(&format!("{{\"operation\":\"disable-rcb\",\"target\":{},\"ok\":true}}", jl::q(&rcb_ref))),
            Err(e) => emit_error("disable-rcb", Some(&rcb_ref), &err_str(&e, false)),
        }

        // 6. conclude
        let _ = conn.disconnect().await;
        dispatcher.abort();
        jl::line("{\"operation\":\"conclude\",\"ok\":true}");
        0
    });
    std::process::exit(code);
}
