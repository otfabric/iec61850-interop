#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Regenerate COVERAGE.md from fixtures/capabilities.json.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CAPS="$ROOT/fixtures/capabilities.json"
OUT="$ROOT/COVERAGE.md"

python3 - <<'PY' "$CAPS" "$OUT"
import json, sys
caps_path, out_path = sys.argv[1], sys.argv[2]
caps = json.load(open(caps_path))
commands = caps.get("commands", {})

def image_for(cmd: str) -> str:
    if cmd.startswith("libiec61850-"):
        return "mms-interop-libiec61850"
    if cmd.startswith("iec61850bean-"):
        return "mms-interop-iec61850bean"
    return "?"

lines = [
    "# Adapter Command Coverage",
    "",
    "Availability of adapter commands across the two images. Compatibility matrices for the Go libraries live in [`go-mms/INTEROP.md`](https://github.com/otfabric/go-mms/blob/main/INTEROP.md) and [`go-iec61850/INTEROP.md`](https://github.com/otfabric/go-iec61850/blob/main/INTEROP.md).",
    "",
    "This file is generated from `fixtures/capabilities.json`; do not edit manually. Run `./scripts/gen-coverage.sh`.",
    "",
    f"Adapter version: `{caps.get('adapterVersion', '?')}` · Fixture revisions: `{caps.get('mmsFixtureRevision', '?')}`, `{caps.get('fixtureRevision', '?')}`",
    "",
    "| Adapter command | Image | Available |",
    "|----------------|-------|:---------:|",
]
for cmd, available in commands.items():
    mark = "✓" if available else "—"
    lines.append(f"| `{cmd}` | `{image_for(cmd)}` | {mark} |")

lines += [
    "",
    "**Known limitations (verified upstream gaps):**",
    "",
    "| Stack | Version | Direction | Capability | Expected skip |",
    "|-------|---------|-----------|------------|---------------|",
]
for lim in caps.get("knownLimitations") or []:
    lines.append(
        f"| `{lim.get('stack','')}` | {lim.get('version','')} | {lim.get('direction','')} | "
        f"{lim.get('capability','')} | `{lim.get('reproducedBy','')}` |"
    )

lines += [
    "",
    "**Notes:**",
    "",
    "- All adapter commands read from the fixture files in `fixtures/`.",
    "- All commands emit JSON Lines to stdout; diagnostics go to stderr.",
    "- Each adapter command supports `--capabilities` (emit JSON and exit) and `--version` (emit version JSON and exit).",
    "- Fixed-sequence IED clients exit non-zero when any operation emits `ok:false` (connection failures remain exit 2).",
    "- A skipped test without a registered limitation in this table must fail CI.",
    "",
]
open(out_path, "w").write("\n".join(lines))
print("wrote", out_path)
PY
