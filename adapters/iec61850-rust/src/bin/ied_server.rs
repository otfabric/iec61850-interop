// SPDX-License-Identifier: MIT
//
// iec61850-rust-ied-server — IED server on iec61850_server::IedServer, the
// counterpart of libiec61850-ied-server.
//
//   iec61850-rust-ied-server [--port PORT] [--icd FILE] [--values FILE]
//
// The model is built at start-up from the ICD with iec61850_scl
// (parse_scl, ResolvedScl::build_model); the logical device is IED name +
// inst, InteropIEDInteropLD, as libiec61850 names it. The data sets and
// report control blocks of LLN0 are registered; the RCBs get libiec61850's
// instance names urcb0101 and brcb0101. values.json sets the initial
// values. SPCSO1/2/3 are control objects under their ctlModel with
// accept-all check handlers; an accepted Operate (test=false) sets stVal
// to ctlVal, as the C handlers do.
//
// Writes are allowed for CF, DC and SP. libiec61850 also allows ST; the
// iec61850-rust server never writes ST (IEC 61850-7-2), so such writes are
// answered object-access-denied.
//
// Exit codes: 0 clean shutdown, 1 startup / argument error.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use iec61850_model::cb::{OptFlds, ReportControlBlock};
use iec61850_model::tree::{DataAttribute, DataSet, DataSetEntry, IedModel, LogicalNode};
use iec61850_model::{ControlModel, MmsValue, NodeRef, ObjectRef, TrgOps, FC};
use iec61850_server::control::handler::OperateFuture;
use iec61850_server::control::{
    AlwaysAcceptCheckHandler, AlwaysAcceptWaitHandler, CheckHandler, ControlAction, ControlAddCause,
    ControlHandler, ControlObject, ControlObjectConfig, ControlObjectEntry, SboClass,
    WaitForExecutionHandler,
};
use iec61850_server::reporting::{Brcb, BufferedReportControl};
use iec61850_server::{
    Dataset, DatasetEntry, IedServer, IedServerConfig, Rcb, ReportControl, TriggerOptions, WriteAccessPolicies,
};
use serde_json::Value;

use adapter::{jl, typed};

const PROG: &str = "ied-server";
const IED_NAME: &str = "InteropIED";
const LD_INST: &str = "InteropLD";

type BoxErr = Box<dyn std::error::Error>;

fn load_model(path: &str) -> Result<IedModel, BoxErr> {
    let xml = std::fs::read_to_string(path)?;
    let raw = iec61850_scl::parse_scl(&xml)?;
    let resolved = iec61850_scl::ResolvedScl::from_raw(raw)?;
    Ok(resolved.build_model(IED_NAME)?)
}

/// Handle on the live value slot of an attribute (shares the model's Arc).
fn resolve_da(model: &IedModel, iec_path: &str) -> Option<DataAttribute> {
    let r = ObjectRef::parse_iec(iec_path).ok()?;
    match model.node_by_object_ref(&r)? {
        NodeRef::Da(da) => Some(DataAttribute {
            name: da.name.clone(),
            fc: da.fc,
            ty: da.ty,
            trg_ops: da.trg_ops,
            value: da.value.clone(),
            children: Vec::new(),
        }),
        _ => None,
    }
}

fn build_dataset(model: &IedModel, domain: &str, owner: &LogicalNode, ds: &DataSet) -> Result<Dataset, BoxErr> {
    let mut out = Dataset::new(format!("{}${}", owner.full_name(), ds.name));
    for entry in &ds.entries {
        let path = format!("{domain}/{}.{}", entry.ln_name, entry.do_path.join("."));
        let da = resolve_da(model, &path).ok_or_else(|| format!("data set {} references missing {path}", ds.name))?;
        out.push(DatasetEntry::new(data_reference(domain, entry), da.value));
    }
    Ok(out)
}

fn data_reference(domain: &str, entry: &DataSetEntry) -> String {
    let path = entry.do_path.join(".").replace('.', "$");
    format!("{domain}/{}${}${path}", entry.ln_name, entry.fc.as_str())
}

fn trigger_options(trg: TrgOps) -> TriggerOptions {
    let mut out = TriggerOptions::NONE;
    for (m, s) in [
        (TrgOps::DCHG, TriggerOptions::DATA_CHANGED),
        (TrgOps::QCHG, TriggerOptions::QUALITY_CHANGED),
        (TrgOps::DUPD, TriggerOptions::DATA_UPDATE),
        (TrgOps::INTEGRITY, TriggerOptions::INTEGRITY),
        (TrgOps::GI, TriggerOptions::GI),
    ] {
        if trg.contains(m) {
            out = out.union(s);
        }
    }
    out
}

fn opt_flds(rcb: &ReportControlBlock) -> OptFlds {
    if rcb.is_buffered {
        rcb.opt_flds
    } else {
        rcb.opt_flds.mask_urcb()
    }
}

fn register_control_blocks(server: &IedServer, domain: &str) -> Result<(), BoxErr> {
    let model = server.model();
    let ln0 = model
        .ld_by_inst(LD_INST)
        .and_then(|ld| ld.ln_by_name("LLN0"))
        .ok_or("InteropLD has no LLN0")?;
    let mut datasets: HashMap<&str, Dataset> = HashMap::new();
    for ds in &ln0.datasets {
        let built = build_dataset(&model, domain, ln0, ds)?;
        server.register_dataset(domain.to_string(), built.clone());
        datasets.insert(ds.name.as_str(), built);
    }
    for rcb in &ln0.rcbs {
        let ds = datasets
            .get(rcb.dataset_ref.as_str())
            .ok_or_else(|| format!("RCB {} references unknown data set", rcb.name))?
            .clone();
        // libiec61850 (and its SCL model generator) names the first
        // instance of an RCB <name>01.
        let inst = format!("{}01", rcb.name);
        if rcb.is_buffered {
            let brcb = Brcb::new(inst.clone(), ds.name.clone())
                .with_rpt_id(rcb.rpt_id.clone())
                .with_conf_rev(rcb.conf_rev)
                .with_trg_ops(trigger_options(rcb.trg_ops))
                .with_opt_flds(opt_flds(rcb))
                .with_buf_tm_ms(rcb.buf_tm_ms)
                .with_intg_pd_ms(rcb.intg_pd_ms);
            server.register_brcb(BufferedReportControl::new(format!("{domain}/LLN0$BR${inst}"), brcb), ds)?;
        } else {
            let urcb = Rcb::new(inst.clone(), ds.name.clone())
                .with_rpt_id(rcb.rpt_id.clone())
                .with_conf_rev(rcb.conf_rev)
                .with_trg_ops(trigger_options(rcb.trg_ops))
                .with_opt_flds(opt_flds(rcb))
                .with_buf_tm_ms(rcb.buf_tm_ms)
                .with_intg_pd_ms(rcb.intg_pd_ms);
            server.register_urcb(ReportControl::new(&format!("{domain}/LLN0$RP${inst}"), urcb), ds)?;
        }
    }
    Ok(())
}

/// Operate handler: stVal := ctlVal unless test, as the C handlers.
struct StValHandler {
    server: IedServer,
    st_val: DataAttribute,
    name: String,
}

impl ControlHandler for StValHandler {
    fn operate<'a>(&'a self, _action: &'a ControlAction, ctl_val: &'a MmsValue, test: bool) -> OperateFuture<'a> {
        Box::pin(async move {
            jl::log(&format!("{PROG}: operate {} ctlVal={ctl_val:?} test={test}", self.name));
            if !test {
                if let MmsValue::Boolean(b) = ctl_val {
                    if let Err(e) = self.server.update_boolean(&self.st_val, *b) {
                        jl::log(&format!("{PROG}: update {}.stVal: {e}", self.name));
                    }
                }
            }
            Ok::<(), ControlAddCause>(())
        })
    }
}

fn ctl_model_of(da: &DataAttribute) -> Option<ControlModel> {
    match da.snapshot() {
        MmsValue::Integer(0) => Some(ControlModel::StatusOnly),
        MmsValue::Integer(1) => Some(ControlModel::DirectNormal),
        MmsValue::Integer(2) => Some(ControlModel::SboNormal),
        MmsValue::Integer(3) => Some(ControlModel::DirectEnhanced),
        MmsValue::Integer(4) => Some(ControlModel::SboEnhanced),
        _ => None,
    }
}

fn register_controls(server: &IedServer, domain: &str) -> Result<(), BoxErr> {
    let model = server.model();
    for do_name in ["SPCSO1", "SPCSO2", "SPCSO3"] {
        let base = format!("{domain}/GGIO1.{do_name}");
        let cm = resolve_da(&model, &format!("{base}.ctlModel")).ok_or_else(|| format!("{base}.ctlModel not in model"))?;
        let ctl_model = ctl_model_of(&cm).ok_or_else(|| format!("{base}.ctlModel is not a control model"))?;
        let st_val = resolve_da(&model, &format!("{base}.stVal")).ok_or_else(|| format!("{base}.stVal not in model"))?;
        let obj = ControlObject::new(ControlObjectConfig {
            name: do_name.into(),
            ln_name: "GGIO1".into(),
            domain: domain.into(),
            ctl_model,
            sbo_timeout_ms: 30_000,
            sbo_class: SboClass::OperateOnce,
        });
        let entry = ControlObjectEntry::new(obj)
            .with_check(Arc::new(AlwaysAcceptCheckHandler) as Arc<dyn CheckHandler>)
            .with_wait(Arc::new(AlwaysAcceptWaitHandler) as Arc<dyn WaitForExecutionHandler>)
            .with_operate(Arc::new(StValHandler { server: server.clone(), st_val, name: base.clone() })
                as Arc<dyn ControlHandler>);
        server.control_objects().register(entry);
    }
    Ok(())
}

/// values.json "values": "InteropLD/LN.DO.DA" -> scalar, typed after the
/// attribute's current value.
fn apply_values(model: &IedModel, path: &str) -> Result<(), BoxErr> {
    let text = std::fs::read_to_string(path)?;
    let v: Value = serde_json::from_str(&text).map_err(|e| format!("{path}: {e}"))?;
    let values = v.get("values").and_then(Value::as_object).ok_or_else(|| format!("{path}: no \"values\" object"))?;
    for (k, raw) in values {
        let r = format!("{IED_NAME}{k}");
        let da = resolve_da(model, &r).ok_or_else(|| format!("values.json: {r} not in model"))?;
        let cur = da.snapshot();
        let nv = match (&cur, raw) {
            (MmsValue::Boolean(_), Value::Bool(b)) => MmsValue::Boolean(*b),
            (MmsValue::Integer(_), Value::Number(n)) if n.as_i64().is_some() => MmsValue::Integer(n.as_i64().unwrap()),
            (MmsValue::Unsigned(_), Value::Number(n)) if n.as_u64().is_some() => MmsValue::Unsigned(n.as_u64().unwrap()),
            (MmsValue::Float32(_), Value::Number(n)) => MmsValue::Float32(n.as_f64().unwrap_or(0.0) as f32),
            (MmsValue::Float64(_), Value::Number(n)) => MmsValue::Float64(n.as_f64().unwrap_or(0.0)),
            (MmsValue::VisibleString(_), Value::String(s)) => MmsValue::VisibleString(s.clone()),
            (MmsValue::MmsString(_), Value::String(s)) => MmsValue::MmsString(s.clone()),
            // Dbpos: the enumeration as 2 bits (2 = on = "10").
            (MmsValue::BitString { .. }, Value::Number(n)) if (0..=3).contains(&n.as_i64().unwrap_or(-1)) => {
                let d = n.as_i64().unwrap();
                typed::bits_value(&format!("{}{}", (d >> 1) & 1, d & 1)).unwrap()
            }
            _ => return Err(format!("values.json: {r}: value {raw} does not fit {cur:?}").into()),
        };
        da.store(nv);
    }
    Ok(())
}

fn main() {
    jl::handle_meta_flags();
    let mut a = adapter::Args::new(PROG);
    let mut port: i64 = 1102;
    let mut icd = "/fixtures/iec61850/interop.icd".to_string();
    let mut values = "/fixtures/iec61850/values.json".to_string();
    while let Some(f) = a.next_flag() {
        match f.as_str() {
            "--port" => port = adapter::atoi(&a.value(&f)),
            "--icd" => icd = a.value(&f),
            "--values" => values = a.value(&f),
            _ => jl::fatal(1, &format!("{PROG}: unknown argument {f}")),
        }
    }
    if port <= 0 || port > 65535 {
        jl::fatal(1, &format!("{PROG}: invalid port {port}"));
    }

    let model = load_model(&icd).unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: load {icd}: {e}")));
    apply_values(&model, &values).unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: {e}")));
    let domain = model
        .ld_by_inst(LD_INST)
        .unwrap_or_else(|| jl::fatal(1, &format!("{PROG}: the ICD has no logical device {LD_INST}")))
        .domain_name(&model.ied_name);

    let mut wap = WriteAccessPolicies::default();
    wap.set(FC::Cf, true);
    wap.set(FC::Dc, true);
    wap.set(FC::Sp, true);

    let addr: SocketAddr = ([0, 0, 0, 0], port as u16).into();
    let server = IedServer::builder()
        .model(Arc::new(model))
        .bind(addr)
        .config(IedServerConfig { max_mms_connections: 10, write_access_policies: wap, ..Default::default() })
        .build()
        .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: IedServer: {e}")));
    register_control_blocks(&server, &domain).unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: {e}")));
    register_controls(&server, &domain).unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: {e}")));

    adapter::exit_on_signals();
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build().expect("tokio runtime");
    rt.block_on(async move {
        let _handle = server
            .start()
            .await
            .unwrap_or_else(|e| jl::fatal(1, &format!("{PROG}: failed to start on port {port}: {e}")));
        jl::ready(&format!("0.0.0.0:{port}"), "iec61850-v3", IED_NAME);
        std::future::pending::<()>().await;
    });
}
