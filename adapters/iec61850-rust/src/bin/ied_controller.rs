// SPDX-License-Identifier: MIT
//
// iec61850-rust-ied-controller — control client on
// iec61850_client::ControlObjectClient, the counterpart of
// libiec61850-ied-controller.
//
//   iec61850-rust-ied-controller [--host H] [--port P] [--ctlval 0|1]
//                                [--do SPCSO1|SPCSO2|SPCSO3]
//
//   SPCSO1 (direct-normal):  read-ctlmodel, operate, read-stval, conclude
//   SPCSO2 (sbo-normal):     read-ctlmodel, select, operate, read-stval, conclude
//   SPCSO3 (sbo-enhanced):   read-ctlmodel, select-with-value, operate,
//                            read-stval, conclude
//
// References are the bare "InteropLD/GGIO1.<DO>", as in the C. The
// environment variable IEC61850_RUST_LD replaces "InteropLD" (a test aid
// for servers that prefix the IED name; not part of the contract).
//
// As libiec61850's ControlObjectClient_create, creating the control object
// reads its ctlModel from the server; when that fails the operation line
// carries "failed to create ControlObjectClient".
//
// Exit codes: 0 sequence done, 1 argument error, 2 connection failure.

use iec61850_client::{ControlObjectClient, ControlOutcome, IedConnection};
use iec61850_model::{ControlModel, MmsValue, FC};

use adapter::ied::{emit_error, err_str};
use adapter::jl;

const PROG: &str = "ied-controller";

fn ctl_model_of(v: i32) -> Option<ControlModel> {
    Some(match v {
        0 => ControlModel::StatusOnly,
        1 => ControlModel::DirectNormal,
        2 => ControlModel::SboNormal,
        3 => ControlModel::DirectEnhanced,
        4 => ControlModel::SboEnhanced,
        _ => return None,
    })
}

/// ControlObjectClient_create: reads ctlModel, builds the client.
async fn create(conn: &IedConnection, ctl_ref: &str) -> Option<ControlObjectClient> {
    let v = conn.read_int32(&format!("{ctl_ref}.ctlModel"), FC::Cf).await.ok()?;
    let m = ctl_model_of(v)?;
    conn.create_control_object(ctl_ref, m).ok()
}

fn outcome_ok(op: &str, ctl_ref: &str, r: Result<ControlOutcome, iec61850_client::ClientError>, fallback: &str) -> bool {
    match r {
        Ok(ControlOutcome::Success) => true,
        Ok(ControlOutcome::Failure(cause)) => {
            jl::log(&format!("{PROG}: {op} {ctl_ref}: add cause {cause:?}"));
            emit_error(op, Some(ctl_ref), fallback);
            false
        }
        Err(e) => {
            emit_error(op, Some(ctl_ref), &err_str(&e, true));
            false
        }
    }
}

fn emit_operate_ok(ctl_ref: &str, ctlval: bool) {
    jl::line(&format!(
        "{{\"operation\":\"operate\",\"target\":{},\"ok\":true,\"ctlval\":{ctlval}}}",
        jl::q(ctl_ref)
    ));
}

async fn read_ctlmodel(conn: &IedConnection, ctl_ref: &str) {
    let t = format!("{ctl_ref}.ctlModel[CF]");
    match conn.read_int32(&format!("{ctl_ref}.ctlModel"), FC::Cf).await {
        Ok(v) => jl::line(&format!(
            "{{\"operation\":\"read-ctlmodel\",\"target\":{},\"ok\":true,\"value\":{v}}}",
            jl::q(&t)
        )),
        Err(e) => emit_error("read-ctlmodel", Some(&t), &err_str(&e, true)),
    }
}

async fn read_stval(conn: &IedConnection, ctl_ref: &str) {
    let t = format!("{ctl_ref}.stVal[ST]");
    match conn.read_boolean(&format!("{ctl_ref}.stVal"), FC::St).await {
        Ok(v) => jl::line(&format!(
            "{{\"operation\":\"read-stval\",\"target\":{},\"ok\":true,\"value\":{v}}}",
            jl::q(&t)
        )),
        Err(e) => emit_error("read-stval", Some(&t), &err_str(&e, true)),
    }
}

async fn operate(conn: &IedConnection, ctl_ref: &str, ctlval: bool) {
    let Some(ctrl) = create(conn, ctl_ref).await else {
        emit_error("operate", Some(ctl_ref), "failed to create ControlObjectClient");
        return;
    };
    if outcome_ok("operate", ctl_ref, ctrl.operate(MmsValue::Boolean(ctlval)).await, "operate returned false") {
        emit_operate_ok(ctl_ref, ctlval);
    }
}

async fn select(conn: &IedConnection, ctl_ref: &str) -> bool {
    let Some(ctrl) = create(conn, ctl_ref).await else {
        emit_error("select", Some(ctl_ref), "failed to create ControlObjectClient");
        return false;
    };
    match ctrl.select().await {
        Ok(true) => {
            jl::line(&format!("{{\"operation\":\"select\",\"target\":{},\"ok\":true}}", jl::q(ctl_ref)));
            true
        }
        Ok(false) => {
            emit_error("select", Some(ctl_ref), "select returned false");
            false
        }
        Err(e) => {
            emit_error("select", Some(ctl_ref), &err_str(&e, true));
            false
        }
    }
}

async fn select_with_value_and_operate(conn: &IedConnection, ctl_ref: &str, ctlval: bool) {
    let Some(ctrl) = create(conn, ctl_ref).await else {
        emit_error("select-with-value", Some(ctl_ref), "failed to create ControlObjectClient");
        return;
    };
    let r = ctrl.select_with_value(MmsValue::Boolean(ctlval)).await;
    if !outcome_ok("select-with-value", ctl_ref, r, "selectWithValue returned false") {
        return;
    }
    jl::line(&format!("{{\"operation\":\"select-with-value\",\"target\":{},\"ok\":true}}", jl::q(ctl_ref)));
    // Same client object, so Oper carries the ctlNum of the SBOw.
    if outcome_ok("operate", ctl_ref, ctrl.operate(MmsValue::Boolean(ctlval)).await, "operate returned false") {
        emit_operate_ok(ctl_ref, ctlval);
    }
}

fn main() {
    jl::handle_meta_flags();
    let mut ctlval = true;
    let mut do_name = "SPCSO1".to_string();
    let (host, port) = adapter::ied::host_port(PROG, |f, v| match f {
        "--ctlval" => ctlval = adapter::atoi(v) != 0,
        "--do" => do_name = v.to_string(),
        _ => {}
    });
    let expected = match do_name.as_str() {
        "SPCSO1" => 1,
        "SPCSO2" => 2,
        "SPCSO3" => 4,
        _ => jl::fatal(1, &format!("{PROG}: unknown --do {do_name} (supported: SPCSO1, SPCSO2, SPCSO3)")),
    };
    let ld = std::env::var("IEC61850_RUST_LD").unwrap_or_else(|_| "InteropLD".into());
    let ctl_ref = format!("{ld}/GGIO1.{do_name}");

    let rt = adapter::ied::runtime();
    let code = rt.block_on(async {
        let conn = match adapter::ied::connect(&host, port).await {
            Ok(c) => c,
            Err(e) => {
                jl::log(&format!("{PROG}: connect {host}:{port}: {e}"));
                return 2;
            }
        };
        read_ctlmodel(&conn, &ctl_ref).await;
        let conclude = |conn: IedConnection| async move {
            let _ = conn.disconnect().await;
            jl::line("{\"operation\":\"conclude\",\"ok\":true}");
        };
        if expected == 2 && !select(&conn, &ctl_ref).await {
            conclude(conn).await;
            return 0;
        }
        if expected == 4 {
            select_with_value_and_operate(&conn, &ctl_ref, ctlval).await;
        } else {
            operate(&conn, &ctl_ref, ctlval).await;
        }
        read_stval(&conn, &ctl_ref).await;
        conclude(conn).await;
        0
    });
    std::process::exit(code);
}
