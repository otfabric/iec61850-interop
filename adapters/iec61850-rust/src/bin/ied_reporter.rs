// SPDX-License-Identifier: MIT
//
// iec61850-rust-ied-reporter — URCB client on iec61850_client (RCB API and
// report dispatcher), the counterpart of libiec61850-ied-reporter
// (contract 1.3).
//
//   iec61850-rust-ied-reporter [--host H] [--port P] [--sps1-initial 0|1]
//                              [--trigger dchg|gi]
//
// Names are resolved from the server directory: the logical device is the
// one named exactly "InteropLD", else the first whose name ends in
// "InteropLD" (an IED-name-prefixed domain such as "InteropIEDInteropLD"),
// else the first listed. The URCB is LLN0.RP.urcb01 if present, else its
// first indexed instance (urcb0101, IEC 61850-6). Line targets use the
// resolved LD and the SCL name urcb01: "<LD>/LLN0.RP.urcb01".
// IEC61850_RUST_LD and IEC61850_RUST_RCB, when set, replace the resolved
// LD and URCB instance (test aids, not part of the contract).
//
// Sequence: get-rcb, enable-rcb (RptEna=true), then with --trigger dchg
// (default) write <LD>/GGIO1.SPS1.stVal[ST] = !initial, or with
// --trigger gi trigger-gi (the URCB's GI = true); receive-report (10 s),
// disable-rcb, conclude.
//
// Exit codes: 0 sequence done, 1 argument error, 2 connection failure.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use iec61850_client::{AcsiClass, ClientReport, IedConnection, RcbWriteMask, ReasonForInclusion, ReportCallback};
use iec61850_model::{MmsValue, FC};

use adapter::ied::{emit_error, err_str};
use adapter::jl;

const PROG: &str = "ied-reporter";
const RCB_NAME: &str = "urcb01"; // SCL name; instances are urcb0101, ...
const DS_SIZE: usize = 2;

/// Contract name of a member's reason for inclusion. A member carries one
/// reason in practice; should a server set several bits, the first of the
/// IEC 61850-8-1 bit order wins.
fn reason_str(r: ReasonForInclusion) -> &'static str {
    if r.contains(ReasonForInclusion::DATA_CHANGE) {
        "data-change"
    } else if r.contains(ReasonForInclusion::QUALITY_CHANGE) {
        "quality-change"
    } else if r.contains(ReasonForInclusion::DATA_UPDATE) {
        "data-update"
    } else if r.contains(ReasonForInclusion::INTEGRITY) {
        "integrity"
    } else if r.contains(ReasonForInclusion::GI) {
        "gi"
    } else {
        "unknown"
    }
}

/// "urcb01", else its lowest indexed instance "urcb01NN".
fn pick_rcb(names: &[String]) -> Option<String> {
    if names.iter().any(|n| n == RCB_NAME) {
        return Some(RCB_NAME.into());
    }
    names
        .iter()
        .filter(|n| {
            n.strip_prefix(RCB_NAME)
                .map(|s| s.len() == 2 && s.bytes().all(|b| b.is_ascii_digit()))
                .unwrap_or(false)
        })
        .min()
        .cloned()
}

/// The URCB instance in `ld`: $IEC61850_RUST_RCB, else resolved from the
/// URCBs of LLN0, else "urcb01".
async fn resolve_rcb(conn: &IedConnection, ld: &str) -> String {
    if let Ok(rcb) = std::env::var("IEC61850_RUST_RCB") {
        return rcb;
    }
    match conn.get_logical_node_directory(&format!("{ld}/LLN0"), AcsiClass::Urcb).await {
        Ok(names) => pick_rcb(&names).unwrap_or_else(|| {
            jl::log(&format!("{PROG}: no {RCB_NAME} URCB in {ld}/LLN0 {names:?}; using {RCB_NAME}"));
            RCB_NAME.into()
        }),
        Err(e) => {
            jl::log(&format!("{PROG}: get {ld}/LLN0 URCB directory: {e}; using {RCB_NAME}"));
            RCB_NAME.into()
        }
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
    let mut trig_gi = false;
    let (host, port) = adapter::ied::host_port(PROG, |f, v| match f {
        "--sps1-initial" => initial = adapter::atoi(v) != 0,
        "--trigger" => match v {
            "dchg" => trig_gi = false,
            "gi" => trig_gi = true,
            _ => jl::fatal(1, &format!("{PROG}: invalid --trigger {v} (dchg|gi)")),
        },
        _ => {}
    });
    if std::env::args().last().as_deref() == Some("--trigger") {
        jl::fatal(1, &format!("{PROG}: --trigger needs a value (dchg|gi)"));
    }

    let rt = adapter::ied::runtime();
    let code = rt.block_on(async {
        let conn = match adapter::ied::connect(&host, port).await {
            Ok(c) => c,
            Err(e) => {
                jl::log(&format!("{PROG}: connect {host}:{port}: {e}"));
                return 2;
            }
        };

        let ld = adapter::ied::resolve_ld(&conn, PROG).await;
        let rcb_inst = resolve_rcb(&conn, &ld).await;
        // MMS reference of the instance; line target with the SCL name.
        let rcb_ref = format!("{ld}/LLN0.RP.{rcb_inst}");
        let rcb_target = if rcb_inst.starts_with(RCB_NAME) {
            format!("{ld}/LLN0.RP.{RCB_NAME}")
        } else {
            rcb_ref.clone()
        };
        let sps1_ref = format!("{ld}/GGIO1.SPS1.stVal");
        jl::log(&format!("{PROG}: LD {ld}, URCB {rcb_ref}"));

        // 1. get-rcb
        let mut rcb = match conn.get_rcb_values(&rcb_ref).await {
            Ok(r) => r,
            Err(e) => {
                emit_error("get-rcb", Some(&rcb_target), &err_str(&e, false));
                let _ = conn.disconnect().await;
                return 0;
            }
        };
        jl::line(&format!(
            "{{\"operation\":\"get-rcb\",\"target\":{},\"ok\":true,\"rptID\":{},\"rptEna\":{}}}",
            jl::q(&rcb_target),
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
        // Reports are matched on RptID: the one the server holds, or the
        // RCB reference (its default when RptID is empty).
        let rpt_id = rcb.rpt_id().filter(|s| !s.is_empty()).map(String::from);
        if let Err(e) = conn.install_report_handler(rpt_id, &rcb_ref, None, cb).await {
            jl::log(&format!("{PROG}: install report handler: {e}"));
        }
        let dispatcher = conn.spawn_report_dispatcher(Duration::from_millis(50));
        rcb.set_rpt_ena(true);
        if let Err(e) = conn.set_rcb_values(&rcb, RcbWriteMask::RPT_ENA, true).await {
            emit_error("enable-rcb", Some(&rcb_target), &err_str(&e, false));
            let _ = conn.disconnect().await;
            dispatcher.abort();
            return 0;
        }
        jl::line(&format!("{{\"operation\":\"enable-rcb\",\"target\":{},\"ok\":true}}", jl::q(&rcb_target)));

        // 3. write (dchg) or trigger-gi (gi)
        if trig_gi {
            match conn.trigger_grefcb(&rcb_ref).await {
                Ok(()) => jl::line(&format!(
                    "{{\"operation\":\"trigger-gi\",\"target\":{},\"ok\":true}}",
                    jl::q(&rcb_target)
                )),
                Err(e) => emit_error("trigger-gi", Some(&rcb_target), &err_str(&e, false)),
            }
        } else {
            let t = format!("{sps1_ref}[ST]");
            match conn.write_boolean(&sps1_ref, FC::St, !initial).await {
                Ok(()) => jl::line(&format!("{{\"operation\":\"write\",\"target\":{},\"ok\":true}}", jl::q(&t))),
                Err(e) => emit_error("write", Some(&t), &err_str(&e, false)),
            }
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
            Err(_) => emit_error(
                "receive-report",
                None,
                if trig_gi { "timeout-waiting-for-gi-report" } else { "timeout-waiting-for-dchg-report" },
            ),
        }

        // 5. disable-rcb
        rcb.set_rpt_ena(false);
        match conn.set_rcb_values(&rcb, RcbWriteMask::RPT_ENA, true).await {
            Ok(()) => jl::line(&format!("{{\"operation\":\"disable-rcb\",\"target\":{},\"ok\":true}}", jl::q(&rcb_target))),
            Err(e) => emit_error("disable-rcb", Some(&rcb_target), &err_str(&e, false)),
        }

        // 6. conclude
        let _ = conn.disconnect().await;
        dispatcher.abort();
        jl::line("{\"operation\":\"conclude\",\"ok\":true}");
        0
    });
    std::process::exit(code);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn v(names: &[&str]) -> Vec<String> {
        names.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn rcb_resolution() {
        assert_eq!(pick_rcb(&v(&["urcb0102", "urcb01"])).as_deref(), Some("urcb01"));
        assert_eq!(pick_rcb(&v(&["brcb01", "urcb0102", "urcb0101"])).as_deref(), Some("urcb0101"));
        assert_eq!(pick_rcb(&v(&["urcb02", "urcb011", "urcb01ab"])), None);
    }

    #[test]
    fn reasons() {
        assert_eq!(reason_str(ReasonForInclusion::GI), "gi");
        assert_eq!(reason_str(ReasonForInclusion::DATA_CHANGE), "data-change");
        assert_eq!(reason_str(ReasonForInclusion::QUALITY_CHANGE), "quality-change");
        assert_eq!(reason_str(ReasonForInclusion::DATA_UPDATE), "data-update");
        assert_eq!(reason_str(ReasonForInclusion::INTEGRITY), "integrity");
        assert_eq!(reason_str(ReasonForInclusion::empty()), "unknown");
    }
}
