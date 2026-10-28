# Adapter contract

Every adapter command of every image speaks the same contract, so a
consumer harness switches stack by image and command prefix alone
(`libiec61850-`, `iec61850bean-`, `dscsystems-`, `iec61850-rust-`). This file
covers what all commands share and the MMS and IED (client/server) commands.
The layer-2 commands have their own pages: [GOOSE](goose.md) and
[Sampled Values](sv.md).

Which command exists in which image is declared in
[`fixtures/capabilities.json`](../fixtures/capabilities.json) (rendered in
[`COVERAGE.md`](../COVERAGE.md)); the README prose is not a capability claim.

## Stability (v1)

From v1.0.0 the contract follows [semantic versioning](https://semver.org).
The public surface is:

- the image names `ghcr.io/otfabric/iec61850-interop-<stack>`,
- the command names and their flags and defaults,
- the event and operation shapes on stdout (field names, types, value
  encoding) and the exit codes,
- the fixture revisions (`mms-v2`, `iec61850-v3`, `goose-v1`, `sv-v1`) and
  the fixture files they name,
- the schema of `fixtures/capabilities.json` and of the release's
  `manifest.json`.

A **patch** release fixes an adapter so that it matches this contract, or
adds or corrects a known limitation. A **minor** release adds: a command, a
flag with a default that keeps the old behaviour, an optional event field, a
new event type, or a new fixture revision next to the existing ones. A
**major** release removes or renames any of the above, changes a field's
type or meaning, changes a default, or changes the content of an existing
fixture revision. Consumers must ignore event fields and event types they do
not know.

Not covered: stderr text, timing, the upstream library pins (they move in
patch or minor releases, recorded in `manifest.json` and
`capabilities.json`), the base images, and anything under a `known
limitation`.

## Shared rules

- **stdout** carries only JSON Lines, one object per line, flushed per line.
  **stderr** carries human-readable diagnostics and is not part of the
  contract.
- `--version` emits `{"event":"version","adapterVersion":V,"fixtureRevision":"iec61850-v3"}`
  and exits 0.
- `--capabilities` emits
  `{"event":"capabilities","adapterVersion":V,"fixtureRevision":"iec61850-v3","commands":[...]}`
  (the commands of that image) and exits 0.
- `V` is the image version, `ADAPTER_VERSION` at build time (the release
  tag, e.g. `v1.0.0`), `dev` for a local build.
- A command a stack cannot provide answers
  `{"event":"error","code":"unsupported","command":C,"reason":R}` and exits 4.
- Values use the typed value JSON `{"type":T,"value":V}` of
  [goose.md](goose.md#value-json) wherever a value is typed; the fixed-
  sequence clients report plain JSON values (`"value":true`).

## Servers

`*-mms-server` and `*-ied-server` listen on `--port` (default `1102`, the
port the image `EXPOSE`s). When they accept connections they emit one ready
event:

```json
{"event":"ready","address":"0.0.0.0:1102","fixture":"iec61850-v3","adapter":"libiec61850","version":"v1.0.0","ied_name":"InteropIED"}
```

`fixture` is the fixture revision served (`mms-v2` for MMS servers,
`iec61850-v3` for IED servers). A harness starts the container, waits for
this line, runs its test, and stops the container (SIGTERM, exit 0).

- `*-mms-server --fixture PATH --port N` — generic MMS server for
  `fixtures/mms/interop.json` (the image's default `CMD`), with the file
  store and journals of revision `mms-v2`.
- `*-ied-server --port N` — IEC 61850 server for `fixtures/iec61850/`
  (`interop.icd` + `values.json`).

## Fixed-sequence clients

`*-mms-client`, `*-ied-client`, `*-ied-controller` and `*-ied-reporter`
take `--host H --port N`, run a fixed sequence of operations against that
server and exit. Each operation is one line:

```jsonl
{"operation":"associate","ok":true}
{"operation":"identify","ok":true,"value":{"vendor":"OTFabric","model":"libiec61850-mms-server","revision":"1.0"}}
{"operation":"read","target":"InteropIEDInteropLD/GGIO1.SPS1.stVal[ST]","ok":true,"value":true}
{"operation":"write","target":"interop/float32","ok":true}
{"operation":"read","target":"interop/unknown","ok":false,"error":"<message>"}
{"operation":"conclude","ok":true}
```

- `*-mms-client`: the MMS service sequence of revision `mms-v2` (identify,
  name lists, reads and writes of every fixture type, NVLs in all scopes,
  files, journals, InformationReport, negative cases, reconnect and abort).
- `*-ied-client`: associate, server/LD/LN directories, reads of
  `GGIO1.SPS1.stVal[ST]`, `MMXU1.TotW.mag.f[MX]`, `GGIO1.DPS1.stVal[ST]`
  (Dbpos as its enumeration, `2` = on), `MMTR1.TotVAh.actVal[ST]`,
  `LLN0.Mod.ctlModel[CF]`, `LLN0.Mod.d[DC]`, a write of `LLN0.Mod.stVal`,
  the read of dataset `LLN0$dsInterop`, conclude.
- `*-ied-controller --do SPCSO1|SPCSO2|SPCSO3 --ctlval true|false`: direct,
  SBO and SBOw control, then the read-back of `stVal`.
- `*-ied-reporter [--sps1-initial 0|1]`: enables URCB `urcb01`, triggers a
  data-change report, emits it as
  `{"operation":"receive-report","ok":true,"rptID":...,"seqNum":N,"inclusion":[...],"values":[...],"reasons":["data-change"]}`,
  disables the URCB.

`error` is free text and not part of the contract; `ok` is.

Exit codes: `0` sequence completed (each line's `ok` is the result), `1`
argument or startup error, `2` connection failure, `3` (`*-ied-client`) at
least one operation emitted `ok:false`, `4` unsupported command.

## Layer 2

`*-goose-publisher`, `*-goose-subscriber`, `*-sv-publisher` and
`*-sv-subscriber` are long-running: they emit `ready` with `role` and
`iface`, then events per frame, and the publishers take `{"cmd":"set"}` /
`{"cmd":"stop"}` on stdin. They need `CAP_NET_RAW` and a shared Docker
bridge. See [goose.md](goose.md) and [sv.md](sv.md).
