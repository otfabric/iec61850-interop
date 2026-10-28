#!/usr/bin/env python3
"""
validate-fixtures.py — cross-validate mms-interop fixture files.

Checks:
  1. interop.json — valid JSON; required top-level keys present.
  2. values.json  — valid JSON; required keys present.
  3. interop.icd  — valid XML; minimal SCL structure.
  4. Every attribute reference in values.json resolves to an ICD object.
  5. Every dataset member references an object that exists in the ICD.
  6. Every URCB DatSet references a dataset that exists in the ICD.
  7. goose.json and interop-goose.cid — the GOOSE fixture: interop.icd's
     model unchanged, every control block's GSEControl, GSE address,
     dataset members and typed values agree.

Exit code: 0 if all checks pass, 1 if any check fails.
"""

import json
import sys
from datetime import datetime
import xml.etree.ElementTree as ET
from pathlib import Path

ROOT = Path(__file__).parent.parent
MMS_FIXTURE   = ROOT / "fixtures" / "mms" / "interop.json"
VALUES_JSON   = ROOT / "fixtures" / "iec61850" / "values.json"
INTEROP_ICD   = ROOT / "fixtures" / "iec61850" / "interop.icd"
GOOSE_JSON    = ROOT / "fixtures" / "goose" / "goose.json"
GOOSE_SCL     = ROOT / "fixtures" / "goose" / "interop-goose.cid"

SCL_NS = "http://www.iec.ch/61850/2003/SCL"

PASS = 0
FAIL = 0


def ok(msg: str) -> None:
    global PASS
    print(f"PASS  {msg}")
    PASS += 1


def err(msg: str) -> None:
    global FAIL
    print(f"FAIL  {msg}", file=sys.stderr)
    FAIL += 1


# ---------------------------------------------------------------------------
# 1. interop.json
# ---------------------------------------------------------------------------

def validate_mms_fixture() -> dict:
    try:
        text = MMS_FIXTURE.read_text()
    except FileNotFoundError:
        err(f"interop.json not found at {MMS_FIXTURE}")
        return {}

    try:
        data = json.loads(text)
        ok("interop.json — valid JSON")
    except json.JSONDecodeError as e:
        err(f"interop.json — JSON parse error: {e}")
        return {}

    # Required top-level keys: identity + domains.
    if not isinstance(data.get("identity"), dict):
        err("interop.json — missing 'identity' key")
    else:
        ok("interop.json — 'identity' key present")

    if isinstance(data.get("domains"), dict) and data["domains"]:
        ok(f"interop.json — 'domains' key present ({len(data['domains'])} domain(s))")
    else:
        err("interop.json — missing or empty 'domains' key")
        return data

    if isinstance(data.get("revision"), str) and data["revision"]:
        ok(f"interop.json — revision {data['revision']}")
    else:
        err("interop.json — missing 'revision' (adapters report it in the ready event)")

    validate_mms_sections(data)
    return data


def validate_mms_sections(data: dict) -> None:
    """Cross-reference the status, report, journal and file sections."""
    variables = {
        f"{dom}/{name}": spec
        for dom, d in data["domains"].items()
        for name, spec in (d.get("variables") or {}).items()
    }

    status = data.get("status")
    if status is not None:
        if status.get("logical") in (0, 1, 2) and status.get("physical") in (0, 1, 2, 3):
            ok("interop.json — status logical/physical in range")
        else:
            err(f"interop.json — status out of range: {status}")

    for i, rep in enumerate(data.get("informationReports") or []):
        refs = rep.get("variables") or []
        missing = [r for r in refs if r not in variables]
        if not refs or missing:
            err(f"interop.json — informationReports[{i}]: unknown or no variables {missing or refs}")
        else:
            form = f"vmdList {rep['vmdList']}" if rep.get("vmdList") else "listOfVariable"
            ok(f"interop.json — informationReports[{i}] ({form}) references {len(refs)} variable(s)")

    for dom, d in data["domains"].items():
        for jname, journal in (d.get("journals") or {}).items():
            entries = journal.get("entries") or []
            ids = [e.get("entryId") for e in entries]
            times = [e.get("occurrenceTime") for e in entries]
            problems = []
            if len(set(ids)) != len(ids) or not all(isinstance(x, int) and x >= 0 for x in ids):
                problems.append("entryId values must be unique non-negative integers")
            if times != sorted(times):
                problems.append("entries must be in occurrenceTime order")
            for e in entries:
                if e.get("tag") not in variables:
                    problems.append(f"tag {e.get('tag')!r} is not a fixture variable")
                if not 0 <= e.get("reasonCode", -1) < 128:
                    problems.append(f"reasonCode {e.get('reasonCode')!r} is not a 7-bit value")
            if problems:
                for p in problems:
                    err(f"interop.json — journal {dom}/{jname}: {p}")
            else:
                ok(f"interop.json — journal {dom}/{jname}: {len(entries)} entries")

    for name, f in (data.get("files") or {}).items():
        forms = [k for k in ("content", "hex", "size") if k in f]
        if len(forms) != 1:
            err(f"interop.json — file {name}: exactly one of content/hex/size required, got {forms}")
            continue
        if "hex" in f:
            try:
                bytes.fromhex(f["hex"])
            except ValueError:
                err(f"interop.json — file {name}: invalid hex")
                continue
        if "size" in f and not (isinstance(f["size"], int) and f["size"] >= 0):
            err(f"interop.json — file {name}: size must be a non-negative integer")
            continue
        if "lastModified" in f:
            try:
                datetime.strptime(f["lastModified"], "%Y-%m-%dT%H:%M:%SZ")
            except (TypeError, ValueError):
                err(f"interop.json — file {name}: lastModified must be YYYY-MM-DDTHH:MM:SSZ")
                continue
        ok(f"interop.json — file {name} ({forms[0]}{'' if 'lastModified' in f else ', no lastModified'})")


# ---------------------------------------------------------------------------
# 2. values.json
# ---------------------------------------------------------------------------

def validate_values_json() -> dict:
    """
    Returns the 'values' sub-dict (attribute ref → value) for cross-validation.
    """
    try:
        text = VALUES_JSON.read_text()
    except FileNotFoundError:
        err(f"values.json not found at {VALUES_JSON}")
        return {}, []

    try:
        data = json.loads(text)
        ok("values.json — valid JSON")
    except json.JSONDecodeError as e:
        err(f"values.json — JSON parse error: {e}")
        return {}, []

    if not isinstance(data, dict):
        err("values.json — top-level value must be a JSON object")
        return {}, []

    values = data.get("values", {})
    writable = data.get("writable", [])

    if not isinstance(values, dict):
        err("values.json — 'values' key must be a JSON object")
        return {}, []

    ok(f"values.json — 'values' key: {len(values)} attribute refs")
    ok(f"values.json — 'writable' key: {len(writable)} writable ref(s)")
    return values, writable


# ---------------------------------------------------------------------------
# 3. interop.icd — parse SCL
# ---------------------------------------------------------------------------

def parse_icd() -> ET.Element | None:
    try:
        tree = ET.parse(INTEROP_ICD)
        ok("interop.icd — valid XML")
        return tree.getroot()
    except FileNotFoundError:
        err(f"interop.icd not found at {INTEROP_ICD}")
    except ET.ParseError as e:
        err(f"interop.icd — XML parse error: {e}")
    return None


def tag(local: str) -> str:
    return f"{{{SCL_NS}}}{local}"


def _build_type_maps(root: ET.Element) -> tuple[dict, dict, dict, dict, dict]:
    """
    Parse DataTypeTemplates and return five maps:
      ln_type_dos:    LNodeType id → {do_name: do_type_id}
      do_type_das:    DOType id   → [(da_name, btype, da_type_id), ...]
      do_type_sdos:   DOType id   → {sdo_name: do_type_id}
      da_type_bdas:   DAType id   → [bda_name, ...]
    """
    ln_type_dos:  dict[str, dict[str, str]]             = {}
    do_type_das:  dict[str, list[tuple[str, str, str]]] = {}
    do_type_sdos: dict[str, dict[str, str]]             = {}
    da_type_bdas: dict[str, list[str]]                   = {}

    dtt = root.find(tag("DataTypeTemplates"))
    if dtt is None:
        return ln_type_dos, do_type_das, do_type_sdos, da_type_bdas

    for lnt in dtt.iter(tag("LNodeType")):
        lnt_id = lnt.get("id", "")
        dos: dict[str, str] = {}
        for do in lnt:
            if do.tag == tag("DO"):
                dos[do.get("name", "")] = do.get("type", "")
        ln_type_dos[lnt_id] = dos

    for dot in dtt.iter(tag("DOType")):
        dot_id = dot.get("id", "")
        das:  list[tuple[str, str, str]] = []
        sdos: dict[str, str]             = {}
        for child in dot:
            if child.tag == tag("DA"):
                das.append((child.get("name", ""),
                             child.get("bType", ""),
                             child.get("type", "")))
            elif child.tag == tag("SDO"):
                sdos[child.get("name", "")] = child.get("type", "")
        do_type_das[dot_id]  = das
        do_type_sdos[dot_id] = sdos

    for dat in dtt.iter(tag("DAType")):
        dat_id = dat.get("id", "")
        bdas: list[str] = []
        for bda in dat.iter(tag("BDA")):
            bdas.append(bda.get("name", ""))
        da_type_bdas[dat_id] = bdas

    return ln_type_dos, do_type_das, do_type_sdos, da_type_bdas


def collect_icd_paths(root: ET.Element) -> set[str]:
    """
    Build the set of all IEC 61850 object paths present in the ICD.

    SCL defines objects through type references:
      LN.lnType → LNodeType → DO.type → DOType → DA/SDO

    Path format: LD/LN.DO.DA  (functional constraint omitted — values.json
    uses the same format without FC suffix).
    """
    paths: set[str] = set()
    ln_type_dos, do_type_das, do_type_sdos, da_type_bdas = _build_type_maps(root)

    for ied in root.iter(tag("IED")):
        for ld in ied.iter(tag("LDevice")):
            ld_inst = ld.get("inst", "")
            for ln in list(ld.iter(tag("LN0"))) + list(ld.iter(tag("LN"))):
                if ln.tag == tag("LN0"):
                    ln_name = "LLN0"
                else:
                    ln_name = f"{ln.get('lnClass','')}{ln.get('inst','')}"
                ln_type = ln.get("lnType", "")
                for do_name, do_type in ln_type_dos.get(ln_type, {}).items():
                    base = f"{ld_inst}/{ln_name}.{do_name}"
                    paths.add(base)
                    for da_name, b_type, da_type in do_type_das.get(do_type, []):
                        paths.add(f"{base}.{da_name}")
                        # DA with bType="Struct" references a DAType → collect BDAs
                        if b_type == "Struct" and da_type:
                            for bda_name in da_type_bdas.get(da_type, []):
                                paths.add(f"{base}.{da_name}.{bda_name}")
                    for sdo_name, sdo_type in do_type_sdos.get(do_type, {}).items():
                        paths.add(f"{base}.{sdo_name}")
                        for da_name, b_type, da_type in do_type_das.get(sdo_type, []):
                            paths.add(f"{base}.{sdo_name}.{da_name}")
                            if b_type == "Struct" and da_type:
                                for bda_name in da_type_bdas.get(da_type, []):
                                    paths.add(f"{base}.{sdo_name}.{da_name}.{bda_name}")
    return paths


def collect_datasets(root: ET.Element) -> dict[str, list[str]]:
    """
    Return a dict of {LD/LN.DataSet -> [member refs]}.
    """
    datasets: dict[str, list[str]] = {}

    for ied in root.iter(tag("IED")):
        for ld in ied.iter(tag("LDevice")):
            ld_inst = ld.get("inst", "")
            for ln in list(ld.iter(tag("LN0"))) + list(ld.iter(tag("LN"))):
                ln_class = ln.get("lnClass", "LLN0") if ln.tag == tag("LN0") else ln.get("lnClass", "")
                ln_inst  = ln.get("inst", "") if ln.tag != tag("LN0") else ""
                ln_name  = f"{ln_class}{ln_inst}"
                for ds in ln.iter(tag("DataSet")):
                    ds_name = ds.get("name", "")
                    key = f"{ld_inst}/{ln_name}.{ds_name}"
                    members = []
                    for fcda in ds.iter(tag("FCDA")):
                        # Reconstruct the object reference from FCDA attributes.
                        fcda_ld = fcda.get("ldInst", ld_inst)
                        fcda_prefix = fcda.get("prefix", "")
                        fcda_ln_class = fcda.get("lnClass", "")
                        fcda_ln_inst  = fcda.get("lnInst", "")
                        fcda_do  = fcda.get("doName", "")
                        fcda_da  = fcda.get("daName", "")
                        ln_full = f"{fcda_prefix}{fcda_ln_class}{fcda_ln_inst}"
                        ref = f"{fcda_ld}/{ln_full}.{fcda_do}"
                        if fcda_da:
                            ref += f".{fcda_da}"
                        members.append(ref)
                    datasets[key] = members
    return datasets


def collect_urcbs(root: ET.Element) -> list[dict]:
    """
    Return a list of {name, ld, ln, datSet} for each ReportControl.
    """
    rcbs = []
    for ied in root.iter(tag("IED")):
        for ld in ied.iter(tag("LDevice")):
            ld_inst = ld.get("inst", "")
            for ln in list(ld.iter(tag("LN0"))) + list(ld.iter(tag("LN"))):
                ln_class = ln.get("lnClass", "LLN0") if ln.tag == tag("LN0") else ln.get("lnClass", "")
                ln_inst  = ln.get("inst", "") if ln.tag != tag("LN0") else ""
                ln_name  = f"{ln_class}{ln_inst}"
                for rc in ln.iter(tag("ReportControl")):
                    # buffered="false" = URCB; buffered="true" = BRCB
                    buffered = rc.get("buffered", "false").lower() == "true"
                    rcbs.append({
                        "name": rc.get("name", ""),
                        "ld": ld_inst,
                        "ln": ln_name,
                        "datSet": rc.get("datSet", ""),
                        "buffered": buffered,
                    })
    return rcbs


# ---------------------------------------------------------------------------
# 4. Cross-validate values.json against ICD paths
# ---------------------------------------------------------------------------

def validate_values_against_icd(values: dict, writable: list, icd_paths: set[str]) -> None:
    missing = []
    for ref in values:
        if ref not in icd_paths:
            missing.append(ref)
    if missing:
        for ref in sorted(missing):
            err(f"values.json 'values' ref '{ref}' not found in interop.icd")
    else:
        ok(f"values.json — all {len(values)} 'values' refs resolve in ICD")

    for ref in writable:
        # writable entries are typically DO-level refs (no DA suffix).
        do_level = ref.rstrip("/")
        if do_level not in icd_paths and not any(p.startswith(do_level + ".") for p in icd_paths):
            err(f"values.json 'writable' ref '{ref}' not found in interop.icd")
        else:
            ok(f"values.json — 'writable' ref '{ref}' resolves in ICD")


# ---------------------------------------------------------------------------
# 5. Validate dataset members against ICD paths
# ---------------------------------------------------------------------------

def validate_dataset_members(datasets: dict[str, list[str]], icd_paths: set[str]) -> None:
    all_ok = True
    for ds_ref, members in datasets.items():
        for m in members:
            # Normalise: strip trailing .stVal etc. to DO level for existence check.
            if m not in icd_paths:
                # Try DO-level check (strip last component).
                do_level = ".".join(m.split(".")[:-1]) if "." in m else m
                if do_level not in icd_paths:
                    err(f"dataset {ds_ref} member '{m}' not found in ICD")
                    all_ok = False
    if all_ok:
        total = sum(len(v) for v in datasets.values())
        ok(f"datasets — all {total} members across {len(datasets)} datasets resolve in ICD")


# ---------------------------------------------------------------------------
# 6. Validate URCB DatSet references
# ---------------------------------------------------------------------------

def validate_urcb_datasets(rcbs: list[dict], datasets: dict[str, list[str]]) -> None:
    all_ok = True
    for rcb in rcbs:
        if rcb["buffered"]:
            continue  # BRCB — skip for now
        ds_name = rcb["datSet"]
        if not ds_name:
            continue
        # The datSet attribute is a local name; the full key is LD/LN.datSet.
        full_key = f"{rcb['ld']}/{rcb['ln']}.{ds_name}"
        if full_key not in datasets:
            err(f"URCB {rcb['ld']}/{rcb['ln']}.{rcb['name']} references "
                f"unknown dataset '{ds_name}' (expected '{full_key}')")
            all_ok = False
    if all_ok:
        urcb_count = sum(1 for r in rcbs if not r["buffered"])
        ok(f"URCBs — all {urcb_count} URCB DatSet references are valid")



# ---------------------------------------------------------------------------
# 7. GOOSE fixture
# ---------------------------------------------------------------------------

GOOSE_TYPES = {"boolean", "integer", "unsigned", "float32", "float64",
               "visible-string", "mms-string", "octet-string", "bit-string",
               "utc-time", "binary-time", "array", "structure"}


def _canonical(elem: ET.Element | None) -> str:
    if elem is None:
        return ""
    for e in elem.iter():
        e.tail = (e.tail or "").strip() or None
        e.text = (e.text or "").strip() or None
    return ET.tostring(elem, encoding="unicode")


def _ln_name(ln: ET.Element) -> str:
    if ln.tag == tag("LN0"):
        return "LLN0"
    return f"{ln.get('prefix', '')}{ln.get('lnClass', '')}{ln.get('inst', '')}"


def _model_shape(root: ET.Element) -> list[tuple]:
    shape = []
    for ld in root.iter(tag("LDevice")):
        for ln in list(ld.iter(tag("LN0"))) + list(ld.iter(tag("LN"))):
            shape.append((ld.get("inst"), _ln_name(ln), ln.get("lnType")))
    return sorted(shape)


def _typed_ok(v, path: str) -> bool:
    if not isinstance(v, dict) or v.get("type") not in GOOSE_TYPES or "value" not in v:
        err(f"goose.json {path}: not a typed value {v!r}")
        return False
    t, val = v["type"], v["value"]
    good = True
    if t == "boolean":
        good = isinstance(val, bool)
    elif t in ("integer", "unsigned", "float32", "float64"):
        good = isinstance(val, (int, float)) and not isinstance(val, bool)
        if t == "unsigned" and good:
            good = val >= 0
    elif t == "bit-string":
        good = isinstance(val, str) and set(val) <= {"0", "1"}
    elif t in ("utc-time", "binary-time"):
        try:
            datetime.strptime(val, "%Y-%m-%dT%H:%M:%S.%fZ")
        except (TypeError, ValueError):
            good = False
    elif t in ("array", "structure"):
        good = isinstance(val, list) and all(_typed_ok(c, f"{path}[{i}]") for i, c in enumerate(val))
    else:
        good = isinstance(val, str)
    if not good:
        err(f"goose.json {path}: value {val!r} does not fit type {t}")
    return good


def validate_goose(icd_root: ET.Element) -> None:
    try:
        data = json.loads(GOOSE_JSON.read_text())
        ok("goose.json — valid JSON")
    except (FileNotFoundError, json.JSONDecodeError) as e:
        err(f"goose.json — {e}")
        return
    try:
        cid = ET.parse(GOOSE_SCL).getroot()
        ok("interop-goose.cid — valid XML")
    except (FileNotFoundError, ET.ParseError) as e:
        err(f"interop-goose.cid — {e}")
        return

    if data.get("revision") != "goose-v1":
        err(f"goose.json revision {data.get('revision')!r}, expected 'goose-v1'")

    # The GOOSE SCL is interop.icd plus GOOSE: same types, same model.
    if _canonical(cid.find(tag("DataTypeTemplates"))) != _canonical(icd_root.find(tag("DataTypeTemplates"))):
        err("interop-goose.cid DataTypeTemplates differ from interop.icd")
    else:
        ok("interop-goose.cid — DataTypeTemplates identical to interop.icd")
    if _model_shape(cid) != _model_shape(icd_root):
        err("interop-goose.cid logical devices/nodes differ from interop.icd")
    else:
        ok("interop-goose.cid — logical devices and nodes identical to interop.icd")

    ied = cid.find(tag("IED"))
    ied_name = ied.get("name", "") if ied is not None else ""
    if ied_name != data.get("iedName"):
        err(f"goose.json iedName {data.get('iedName')!r} != SCL IED {ied_name!r}")
    paths = collect_icd_paths(cid)
    datasets = collect_datasets(cid)
    gse = {g.get("cbName"): g for g in cid.iter(tag("GSE"))}
    gsecs = {}
    for ld in cid.iter(tag("LDevice")):
        for gc in ld.iter(tag("GSEControl")):
            gsecs[gc.get("name")] = (ld.get("inst"), gc)

    for name, cb in data.get("controlBlocks", {}).items():
        where = f"goose.json controlBlocks.{name}"
        if name not in gsecs:
            err(f"{where}: no GSEControl {name} in interop-goose.cid")
            continue
        ld_inst, gc = gsecs[name]
        ld_ref = f"{ied_name}{ld_inst}"
        expect = {
            "gocbRef": f"{ld_ref}/LLN0$GO${name}",
            "datSet": f"{ld_ref}/LLN0${gc.get('datSet')}",
            "goID": gc.get("appID"),
            "confRev": int(gc.get("confRev", "0")),
        }
        g = gse.get(name)
        if g is None:
            err(f"{where}: no GSE address for {name} in the Communication section")
            continue
        p = {e.get("type"): (e.text or "").strip() for e in g.iter(tag("P"))}
        expect.update({
            "dst": p.get("MAC-Address", "").replace("-", ":").lower(),
            "appID": f"0x{int(p.get('APPID', '0'), 16):04x}",
            "vlanId": int(p.get("VLAN-ID", "0"), 16),
            "vlanPrio": int(p.get("VLAN-PRIORITY", "0")),
            "minTime": int(g.findtext(tag("MinTime"), "0")),
            "maxTime": int(g.findtext(tag("MaxTime"), "0")),
        })
        bad = [f"{k}={cb.get(k)!r} (SCL {v!r})" for k, v in expect.items() if cb.get(k) != v]
        if bad:
            err(f"{where}: disagrees with interop-goose.cid: {', '.join(bad)}")
        else:
            ok(f"{where}: references, address and timing match the SCL")

        members = cb.get("members", [])
        ds_members = datasets.get(f"{ld_inst}/LLN0.{gc.get('datSet')}", [])
        if [m.get("ref") for m in members] != ds_members:
            err(f"{where}: members {[m.get('ref') for m in members]} != dataset {ds_members}")
        missing = [m["ref"] for m in members if m.get("ref") not in paths]
        if missing:
            err(f"{where}: members not in the model: {missing}")
        fcs = [f.get("fc") for ds in cid.iter(tag("DataSet")) if ds.get("name") == gc.get("datSet")
               for f in ds.iter(tag("FCDA"))]
        if [m.get("fc") for m in members] != fcs:
            err(f"{where}: member FCs {[m.get('fc') for m in members]} != dataset FCs {fcs}")
        for key in ("values", "update"):
            vals = cb.get(key, [])
            if len(vals) != len(members):
                err(f"{where}.{key}: {len(vals)} values for {len(members)} members")
                continue
            typed = all(_typed_ok(v, f"{name}.{key}[{i}]") for i, v in enumerate(vals))
            types = [v["type"] for v in vals] == [m.get("type") for m in members]
            if not types:
                err(f"{where}.{key}: value types differ from member types")
            if typed and types:
                ok(f"{where}.{key}: {len(vals)} typed values match the members")

# ===========================================================================
# Main
# ===========================================================================

def main() -> int:
    print("=== Fixture validation ===")
    print()

    print("--- interop.json ---")
    validate_mms_fixture()
    print()

    print("--- values.json ---")
    values, writable = validate_values_json()
    print()

    print("--- interop.icd ---")
    icd_root = parse_icd()
    if icd_root is None:
        return 1

    print()
    print("--- Cross-validation ---")
    if icd_root is not None:
        icd_paths  = collect_icd_paths(icd_root)
        ok(f"ICD — {len(icd_paths)} object paths indexed")

        datasets = collect_datasets(icd_root)
        ok(f"ICD — {len(datasets)} datasets indexed")

        rcbs = collect_urcbs(icd_root)
        ok(f"ICD — {len(rcbs)} report control blocks indexed")

        if values:
            validate_values_against_icd(values, writable, icd_paths)

        if datasets:
            validate_dataset_members(datasets, icd_paths)

        if rcbs:
            validate_urcb_datasets(rcbs, datasets)

        print()
        print("--- goose.json / interop-goose.cid ---")
        validate_goose(icd_root)

    print()
    print(f"Results: {PASS} passed  {FAIL} failed")
    return 0 if FAIL == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
