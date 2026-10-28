#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Regenerate COVERAGE.md and the stack × capability matrix in README.md
# (between the capability-matrix markers) from fixtures/capabilities.json.
#
# Usage: gen-coverage.sh           rewrite both files
#        gen-coverage.sh --check   fail when either file is out of date
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-write}"

python3 - "$ROOT" "$MODE" <<'PY'
import json, pathlib, sys

root, mode = pathlib.Path(sys.argv[1]), sys.argv[2]
caps = json.loads((root / "fixtures/capabilities.json").read_text())
commands = caps.get("commands", {})

STACKS = ["libiec61850", "iec61850bean", "dscsystems", "iec61850-rust"]
CAPABILITIES = [
    ("MMS server", "mms-server"),
    ("MMS client", "mms-client"),
    ("IED server", "ied-server"),
    ("IED client", "ied-client"),
    ("IED controller", "ied-controller"),
    ("IED reporter", "ied-reporter"),
    ("GOOSE publisher", "goose-publisher"),
    ("GOOSE subscriber", "goose-subscriber"),
    ("SV publisher", "sv-publisher"),
    ("SV subscriber", "sv-subscriber"),
]

def image_for(cmd: str) -> str:
    # Longest prefix first: "iec61850-rust-" before "iec61850bean-" is not
    # ambiguous, but keep the order explicit.
    for stack in sorted(STACKS, key=len, reverse=True):
        if cmd.startswith(stack + "-"):
            return f"iec61850-interop-{stack}"
    return "?"

def mark(yes: bool) -> str:
    return "✓" if yes else "—"

def matrix() -> list:
    svcb = caps.get("svcbOverMms") or {}
    head = "| Stack | Image | " + " | ".join(name for name, _ in CAPABILITIES) + " | SVCB over MMS |"
    rule = "|-------|-------|" + "|".join(":-:" for _ in CAPABILITIES) + "|:-:|"
    rows = [head, rule]
    for stack in STACKS:
        cells = [mark(commands.get(f"{stack}-{suffix}", False)) for _, suffix in CAPABILITIES]
        rows.append(f"| {stack} | `iec61850-interop-{stack}` | " + " | ".join(cells)
                    + f" | {mark(bool(svcb.get(stack)))} |")
    return rows

revisions = (f"`{caps.get('mmsFixtureRevision', '?')}`, `{caps.get('fixtureRevision', '?')}`, "
             f"`{caps.get('gooseFixtureRevision', '?')}`, `{caps.get('svFixtureRevision', '?')}`")

lines = [
    "# Adapter Command Coverage",
    "",
    "Availability of adapter commands across the four images. Compatibility matrices for the Go libraries live in [`go-mms/INTEROP.md`](https://github.com/otfabric/go-mms/blob/main/INTEROP.md) and [`go-iec61850/INTEROP.md`](https://github.com/otfabric/go-iec61850/blob/main/INTEROP.md).",
    "",
    "This file is generated from `fixtures/capabilities.json`; do not edit manually. Run `./scripts/gen-coverage.sh`.",
    "",
    f"Adapter version: `{caps.get('adapterVersion', '?')}` · Fixture revisions: {revisions}",
    "",
    "## Stack × capability",
    "",
    *matrix(),
    "",
    "SVCB over MMS: the IED server exposes `LLN0$MS$MSVCB02` of the IED fixture as MMS variables.",
    "",
    "## Commands",
    "",
    "| Adapter command | Image | Available |",
    "|----------------|-------|:---------:|",
]
for cmd, available in commands.items():
    lines.append(f"| `{cmd}` | `{image_for(cmd)}` | {mark(available)} |")

lines += [
    "",
    "## Known limitations (verified upstream gaps)",
    "",
    "| Stack | Version | Direction | Capability | Expected skip |",
    "|-------|---------|-----------|------------|---------------|",
]
for lim in caps.get("knownLimitations") or []:
    lines.append(
        f"| `{lim.get('stack','')}` | {lim.get('version','')} | {lim.get('direction','')} | "
        f"{lim.get('capability','')} | {('`' + lim['reproducedBy'] + '`') if lim.get('reproducedBy') else '—'} |"
    )

lines += [
    "",
    "## Notes",
    "",
    "- All adapter commands read from the fixture files in `fixtures/`.",
    "- All commands emit JSON Lines to stdout; diagnostics go to stderr. The contract is [`docs/contract.md`](docs/contract.md).",
    "- Each adapter command supports `--capabilities` (emit JSON and exit) and `--version` (emit version JSON and exit).",
    "- Fixed-sequence IED clients exit non-zero when any operation emits `ok:false` (connection failures remain exit 2).",
    "- Every IED server seeds its values from `fixtures/iec61850/values.json` (including `GGIO1.DPS1.stVal` = 2 and `MMTR1.TotVAh.actVal` = 42). Every fixed-sequence IED client reads, in this order: `GGIO1.SPS1.stVal[ST]`, `MMXU1.TotW.mag.f[MX]`, `GGIO1.DPS1.stVal[ST]` (Dbpos, reported as its enumeration 0..3, 2 = on), `MMTR1.TotVAh.actVal[ST]` (INT32U), `LLN0.Mod.ctlModel[CF]`, `LLN0.Mod.d[DC]`.",
    "- GOOSE commands (`*-goose-publisher`, `*-goose-subscriber`) need a raw socket: run them with `--cap-add NET_RAW` on a shared Docker network; see [`docs/goose.md`](docs/goose.md).",
    "- Sampled Values commands (`*-sv-publisher`, `*-sv-subscriber`) need the same; see [`docs/sv.md`](docs/sv.md). iec61850bean has no SV support, so `iec61850-interop-iec61850bean` has no SV commands.",
    "- `iec61850-interop-dscsystems` is built from GPL-3.0 code (dscsystems/go-iec61850); see `adapters/dscsystems/NOTICE`.",
    "- A skipped test without a registered limitation in this table must fail CI.",
    "",
]
coverage = "\n".join(lines)

BEGIN, END = "<!-- capability-matrix:begin -->", "<!-- capability-matrix:end -->"
readme_path = root / "README.md"
readme = readme_path.read_text()
if BEGIN not in readme or END not in readme:
    sys.exit("gen-coverage: README.md has no capability-matrix markers")
pre, rest = readme.split(BEGIN, 1)
_, post = rest.split(END, 1)
block = "\n".join([
    BEGIN,
    "<!-- generated from fixtures/capabilities.json by scripts/gen-coverage.sh; do not edit -->",
    "",
    *matrix(),
    "",
    END,
])
new_readme = pre + block + post

targets = [(root / "COVERAGE.md", coverage), (readme_path, new_readme)]
if mode == "--check":
    stale = [p.name for p, text in targets if p.read_text() != text]
    if stale:
        sys.exit(f"gen-coverage: {', '.join(stale)} out of date with fixtures/capabilities.json; run scripts/gen-coverage.sh")
    print("ok   COVERAGE.md and the README capability matrix match fixtures/capabilities.json")
else:
    for p, text in targets:
        p.write_text(text)
        print("wrote", p)
PY
