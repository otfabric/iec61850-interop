# iec61850-interop — Roadmap

This file is the single source of truth for `iec61850-interop` work items.
It is updated as phases complete and new work is scoped.

The master roadmap for the entire OTFabric IEC 61850 MMS stack is described in
`go-iec61850/PLAN.md`. This file contains only the `iec61850-interop`-owned deliverables.

---

## Current release: v0.1.1

**Available adapter commands**

| Image | Commands |
|-------|----------|
| `iec61850-interop-libiec61850` | `libiec61850-mms-server`, `libiec61850-mms-client`, `libiec61850-ied-server`, `libiec61850-ied-controller`, `libiec61850-ied-reporter` |
| `iec61850-interop-iec61850bean` | `iec61850bean-ied-server`, `iec61850bean-ied-controller`, `iec61850bean-ied-reporter` |

**Fixture revision: `iec61850-v2`**
- Three controllable DOs: SPCSO1 (direct), SPCSO2 (SBO normal), SPCSO3 (SBO enhanced).
- URCB `urcb01` dataset with SPS1 and Mod.

---

## Milestone M1 — Evidence reconciliation and capability contract (v0.2.0)

**Goal:** Every consumer can verify adapter capabilities at runtime. No manually maintained matrix can contradict the machine-readable manifest.

### M1.1 Capability manifest ⬜

Create `fixtures/capabilities.json`:

```json
{
  "schemaVersion": 1,
  "adapterVersion": "0.2.0",
  "fixtureRevision": "iec61850-v2",
  "commands": {
    "libiec61850-mms-server":      true,
    "libiec61850-mms-client":      true,
    "libiec61850-ied-server":      true,
    "libiec61850-ied-controller":  true,
    "libiec61850-ied-reporter":    true,
    "iec61850bean-ied-server":     true,
    "iec61850bean-ied-controller": true,
    "iec61850bean-ied-reporter":   true
  },
  "knownLimitations": [
    {
      "stack":       "iec61850bean",
      "version":     "1.x",
      "direction":   "go-client → iec61850bean-server",
      "capability":  "SBOw (SelectWithValue)",
      "reason":      "iec61850bean server does not expose SBOw[CO] as a writable MMS attribute",
      "reproducedBy": "TestBeanClient_Control_SBOwOperate"
    }
  ],
  "upstreams": {
    "libiec61850": { "version": "1.6.2", "checksum": "" },
    "iec61850bean": { "version": "", "artifact": "" }
  }
}
```

### M1.2 Adapter `--capabilities` and `--version` flags ⬜

Every adapter binary must respond to:
- `<cmd> --capabilities` → emit a JSON Lines capabilities record and exit 0.
- `<cmd> --version` → emit a JSON Lines version record and exit 0.

Format:
```json
{"event":"capabilities","adapterVersion":"0.2.0","fixtureRevision":"iec61850-v2","commands":[...]}
{"event":"version","adapterVersion":"0.2.0","git":"<sha>"}
```

### M1.3 Generate COVERAGE.md from manifest ⬜

Replace the manually maintained `COVERAGE.md` with one generated or validated
from `fixtures/capabilities.json` in CI. A script `scripts/gen-coverage.sh` should
regenerate it; CI should fail if the committed file is stale.

### M1.4 Harness validation in consumer repos ⬜

The Go interop harness (in `go-iec61850` and `go-mms`) must fail immediately — not silently skip — when:
- Adapter reports an unexpected `adapterVersion`.
- A required command is absent from `--capabilities`.
- Fixture revision is incompatible with the test.
- A test would skip solely due to missing infrastructure.

This work is implemented in the consumer repos but the adapter contract is defined here.

**Exit criterion:** `fixtures/capabilities.json` is the authoritative statement of adapter versions, upstream versions, fixture revision, available commands, and known unsupported directions. No markdown file may contradict it.

---

## Milestone M2 — Fixture and adapter depth (v0.2.x)

### M2.1 Extended IEC 61850 fixture ⬜

Expand `fixtures/iec61850/interop.icd` beyond the current baseline to include:
- Common Data Classes: SPS, DPS, MV, INS, SPC, DPC, ACT, BCR.
- Quality and timestamp attributes.
- Multiple functional constraints: SP, SV, CO, RP, BR.
- At least two logical nodes with different CDC profiles.

Expand `fixtures/iec61850/values.json` to match.

### M2.2 Multiple URCBs ⬜

Add a second URCB (`urcb02`) to the fixture with:
- Different dataset.
- Different trigger options.
- Different optional fields.

Required for multi-subscription and isolation testing.

### M2.3 BRCB support ⬜

Add one BRCB (`brcb01`) to the fixture.
Required for Phase F (BRCB) interop testing.
Decision on whether to include is deferred to `go-iec61850` PLAN.md Phase F.

### M2.4 Dynamic dataset adapter commands ⬜

For `libiec61850-ied-server` and `iec61850bean-ied-server`:
- Accept client-created dynamic (association-specific) datasets.
- Confirm the dataset is usable before reporting back.

Required for Phase D3 (dynamic datasets).

---

## Milestone M3 — Transport and fault injection (v0.3.0)

### M3.1 Packet-fragmenting proxy ⬜

Provide a TCP proxy binary (`iec61850-interop-proxy`) that can:
- Split TPKT packets at configured byte boundaries.
- Join multiple PDUs into one TCP segment.
- Delay individual frames.
- Drop frames by position or pattern.
- Close the TCP connection at defined state transitions.

Consumer repos use this proxy between Go stacks and adapters for fault-injection tests.

### M3.2 Stalling adapter mode ⬜

Add `--stall-before-respond-ms` and `--stall-after-N-pdus` flags to `libiec61850-mms-server`
for timeout and deadline testing in `go-mms`.

---

## Milestone M4 — Endurance and production evidence (v1.0.0)

### M4.1 Long-running fixture profile ⬜

Add a `fixtures/iec61850/endurance.icd` with:
- Multiple LDs and LNs.
- Dense dataset.
- Multiple URCBs.
- Multiple controllable DOs.

### M4.2 Digest pin automation ⬜

CI pipeline step that:
1. Builds and pushes images.
2. Extracts per-platform digests.
3. Opens a PR against `go-mms` and `go-iec61850` to update pinned digests in `interop.yml`.

### M4.3 Release evidence archive ⬜

On each tagged release, collect and attach as release artifacts:
- Capability manifest.
- Image digests (per-platform).
- Upstream library versions and checksums.
- Fixture revision.
- Go toolchain version used in CI.

**Exit criterion:** A third party can check out the release tag, pull the pinned adapter images, run one command, and reproduce the full compatibility result.

---

## Adapter command contract

All adapters must emit a JSON Lines readiness event before accepting connections:

```json
{"event":"ready","adapter":"libiec61850","version":"0.2.0","fixture":"iec61850-v2","port":102}
```

All adapters must accept `--capabilities` and `--version` with no side effects.

All adapters must exit with code 0 on clean conclude, non-zero on error.

---

## Known limitations register

| Stack | Direction | Capability | Reason | Test |
|-------|-----------|------------|--------|------|
| `iec61850bean` | go→bean server | SBOw `SelectWithValue` | Server does not expose `SBOw[CO]` as writable MMS | `TestBeanClient_Control_SBOwOperate` (skipped) |

New limitations discovered during interop testing must be added here before a skip is accepted in CI.
