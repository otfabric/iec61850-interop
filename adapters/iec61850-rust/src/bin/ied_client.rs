// SPDX-License-Identifier: MIT
//
// iec61850-rust-ied-client — IED client on iec61850_client::IedConnection,
// the counterpart of libiec61850-ied-client.
//
//   iec61850-rust-ied-client [--host HOST] [--port PORT]
//
// Fixed sequence, one JSON Line per operation: associate,
// get-server-directory, get-ld-directory, get-ln-directory (GGIO1),
// read SPS1.stVal[ST], TotW.mag.f[MX], Mod.ctlModel[CF], Mod.d[DC],
// write Mod.stVal[ST]=5, read-dataset LLN0$dsInterop, conclude.
// The logical device is taken from the server directory: "InteropLD",
// else a name ending in "InteropLD", else the first.
//
// Exit codes: 0 all ok, 1 argument error, 2 connection failure,
// 3 some operation ok:false (conclude still attempted).

use iec61850_client::{AcsiClass, IedConnection};
use iec61850_mms::{AccessResult, MmsData};
use iec61850_model::FC;

use adapter::ied::{emit_error, err_str};
use adapter::{cfmt, jl};

const PROG: &str = "ied-client";

fn emit_names(op: &str, target: Option<&str>, names: &[String]) {
    let t = target.map(|t| format!(",\"target\":{}", jl::q(t))).unwrap_or_default();
    let n: Vec<String> = names.iter().map(|s| jl::q(s)).collect();
    jl::line(&format!("{{\"operation\":\"{op}\"{t},\"ok\":true,\"names\":[{}]}}", n.join(",")));
}

fn resolve_ld(names: &[String]) -> Option<String> {
    if names.iter().any(|n| n == "InteropLD") {
        return Some("InteropLD".into());
    }
    if let Some(n) = names.iter().find(|n| n.ends_with("InteropLD")) {
        return Some(n.clone());
    }
    names.first().cloned()
}

fn emit_read(target: &str, value: String) {
    jl::line(&format!("{{\"operation\":\"read\",\"target\":{},\"ok\":true,\"value\":{value}}}", jl::q(target)));
}

async fn run(conn: &IedConnection) -> bool {
    let mut failed = false;
    let mut ld = String::new();
    match conn.get_server_directory(false).await {
        Ok(lds) => {
            emit_names("get-server-directory", None, &lds);
            match resolve_ld(&lds) {
                Some(l) => ld = l,
                None => {
                    emit_error("get-server-directory", None, "no logical device found");
                    failed = true;
                }
            }
        }
        Err(e) => {
            emit_error("get-server-directory", None, &err_str(&e, true));
            failed = true;
        }
    }
    if ld.is_empty() {
        ld = "InteropLD".into();
    }

    match conn.get_logical_device_directory(&ld).await {
        Ok(lns) => emit_names("get-ld-directory", Some(&ld), &lns),
        Err(e) => {
            emit_error("get-ld-directory", Some(&ld), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/GGIO1");
    match conn.get_logical_node_directory(&r, AcsiClass::DataObject).await {
        Ok(dos) => emit_names("get-ln-directory", Some(&r), &dos),
        Err(e) => {
            emit_error("get-ln-directory", Some(&r), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/GGIO1.SPS1.stVal");
    let t = format!("{r}[ST]");
    match conn.read_boolean(&r, FC::St).await {
        Ok(v) => emit_read(&t, v.to_string()),
        Err(e) => {
            emit_error("read", Some(&t), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/MMXU1.TotW.mag.f");
    let t = format!("{r}[MX]");
    match conn.read_float(&r, FC::Mx).await {
        Ok(v) => emit_read(&t, cfmt::g6(v as f64)),
        Err(e) => {
            emit_error("read", Some(&t), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/LLN0.Mod.ctlModel");
    let t = format!("{r}[CF]");
    match conn.read_int32(&r, FC::Cf).await {
        Ok(v) => emit_read(&t, v.to_string()),
        Err(e) => {
            emit_error("read", Some(&t), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/LLN0.Mod.d");
    let t = format!("{r}[DC]");
    match conn.read_string(&r, FC::Dc).await {
        Ok(v) => emit_read(&t, jl::q(&v)),
        Err(e) => {
            emit_error("read", Some(&t), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/LLN0.Mod.stVal");
    let t = format!("{r}[ST]");
    match conn.write_int32(&r, FC::St, 5).await {
        Ok(()) => jl::line(&format!("{{\"operation\":\"write\",\"target\":{},\"ok\":true}}", jl::q(&t))),
        Err(e) => {
            emit_error("write", Some(&t), &err_str(&e, true));
            failed = true;
        }
    }

    let r = format!("{ld}/LLN0$dsInterop");
    match conn.get_data_set_values(&r).await {
        Ok(results) => {
            let vals: Vec<String> = results
                .iter()
                .map(|a| match a {
                    AccessResult::Success(MmsData::Boolean(b)) => b.to_string(),
                    AccessResult::Success(MmsData::Integer(i)) => (*i as i32).to_string(),
                    AccessResult::Success(MmsData::Unsigned(u)) => (*u as u32).to_string(),
                    AccessResult::Success(MmsData::Float32(f)) => cfmt::g6(*f as f64),
                    AccessResult::Success(MmsData::Float64(f)) => cfmt::g6(*f),
                    AccessResult::Success(MmsData::VisibleString(s)) => jl::q(s),
                    _ => "null".into(),
                })
                .collect();
            jl::line(&format!(
                "{{\"operation\":\"read-dataset\",\"target\":{},\"ok\":true,\"values\":[{}]}}",
                jl::q(&r),
                vals.join(",")
            ));
        }
        Err(e) => {
            emit_error("read-dataset", Some(&r), &err_str(&e, true));
            failed = true;
        }
    }

    if let Err(e) = conn.disconnect().await {
        jl::log(&format!("{PROG}: conclude: {e}"));
    }
    jl::line("{\"operation\":\"conclude\",\"ok\":true}");
    failed
}

fn main() {
    jl::handle_meta_flags();
    let (host, port) = adapter::ied::host_port(PROG, |_, _| {});
    let rt = adapter::ied::runtime();
    let code = rt.block_on(async {
        let conn = match adapter::ied::connect(&host, port).await {
            Ok(c) => c,
            Err(e) => {
                jl::log(&format!("{PROG}: connect {host}:{port}: {e}"));
                emit_error("associate", None, &e);
                return 2;
            }
        };
        jl::line("{\"operation\":\"associate\",\"ok\":true}");
        if run(&conn).await {
            3
        } else {
            0
        }
    });
    std::process::exit(code);
}
