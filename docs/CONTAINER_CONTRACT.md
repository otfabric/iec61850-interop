# Adapter contract

Every adapter command of every image speaks the same contract, so a
consumer harness switches stack by image and command prefix alone
(`libiec61850-`, `iec61850bean-`, `dscsystems-`, `iec61850-rust-`, `cbsigma-`, and
`otfabric-` for OT Fabric's own stack, below). This file
covers what all commands share and the MMS and IED (client/server) commands.
The layer-2 commands have their own pages: [GOOSE](goose.md) and
[Sampled Values](sv.md); their UDP transport (R-GOOSE and R-SV, contract
1.4) is on [routable.md](routable.md).

Which command exists in which image is declared in
[`fixtures/capabilities.json`](../fixtures/capabilities.json) (rendered in
[`CAPABILITIES.md`](CAPABILITIES.md)); the README prose is not a capability claim.

## Adapters

| Adapter (command prefix) | Image | Stack |
|---|---|---|
| `libiec61850` | `ghcr.io/otfabric/iec61850-interop-libiec61850` | reference |
| `iec61850bean` | `ghcr.io/otfabric/iec61850-interop-iec61850bean` | reference |
| `dscsystems` | `ghcr.io/otfabric/iec61850-interop-dscsystems` | reference |
| `iec61850-rust` | `ghcr.io/otfabric/iec61850-interop-iec61850-rust` | reference |
| `cbsigma` | `ghcr.io/otfabric/iec61850-interop-cbsigma` | reference (IED and GOOSE commands) |
| `otfabric` | `ghcr.io/otfabric/iec61850-interop-otfabric` | OT Fabric's go-iec61850 and go-mms, not independent, not in a release ([own/go-iec61850](../own/go-iec61850/README.md)) |

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
- Both may carry `upstream` (contract 1.2):
  `{"name":N,"version":V,"revision":SHA,"license":L,"independentEngine":B}`,
  the stack behind the image. `independentEngine` is `false` for an image of
  OT Fabric's own stack (`otfabric`): results against it are not independent
  evidence. An image without `upstream` is a reference of `versions.yaml`
  (`independentEngine` true); the five reference images have none.
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
  SBO and SBOw control, then the read-back of `stVal`. From contract 1.3 the
  controller takes the logical device from the server directory, as the
  reporter does (below); the `target` of its lines is `<LD>/GGIO1.<DO>`.
- `*-ied-reporter [--sps1-initial 0|1] [--trigger dchg|gi]`: enables URCB
  `urcb01`, makes it send a report, emits the report as
  `{"operation":"receive-report","ok":true,"rptID":...,"seqNum":N,"inclusion":[...],"values":[...],"reasons":[...]}`,
  disables the URCB. With `--trigger dchg` (the default) the report is a
  data change: the reporter writes `GGIO1.SPS1.stVal[ST]` = the negation of
  `--sps1-initial` (default 1, so `false`; `values.json` has `false`, so
  pass `0` for a change) and expects `"reasons":["data-change"]`. A server
  may refuse that write (ST is read-only in IEC 61850-7-2); then no report
  follows. With `--trigger gi` (contract 1.3) the reporter writes the
  URCB's `GI` = true instead (operation `trigger-gi`) and expects a general
  interrogation report: both members of `dsInterop` included, reasons
  `["gi","gi"]`, values `values.json`'s `SPS1.stVal` and `Mod.stVal`
  (`[false,1]`). Every IEC 61850 server answers this one.

  Reporter names (contract 1.3): the reporter takes the logical device
  from the server directory (the one named `InteropLD` or ending in it, as
  `InteropIEDInteropLD`), and the URCB as `LLN0$RP$urcb01` or its first
  instance (`LLN0$RP$urcb0101`, IEC 61850-6 indexed instances). The
  `target` of its lines is `<LD>/LLN0.RP.urcb01` with that LD. The
  reporters of contract 1.2 and earlier addressed `InteropLD` and `urcb01`
  as given, which only a server with bare instance names served.

  `--capabilities` of an image whose reporter takes `--trigger` carries
  `"reporterTriggers":["dchg","gi"]` (contract 1.3); without it a harness
  runs only the default.

  `--capabilities` of an image whose GOOSE and SV commands take
  `--transport udp` carries `"transports":["ethernet","udp"]` (contract
  1.4, [routable.md](routable.md)); without it they have the Ethernet
  transport only.

`*-ied-client` also takes `--local-tsel HEX` and `--remote-tsel HEX`, the
calling and called COTP T-selectors of 1 to 4 octets in hex (`01`, `0001`,
`00000001`), so a consumer can check that its server accepts the selectors
real clients send. Without them each stack sends its own defaults, which are
part of what is tested:

| Stack | Calling T-selector | Called T-selector | With `--local-tsel`/`--remote-tsel` |
|-------|:--:|:--:|---|
| libiec61850 | `0001` | `0001` | sent as given (S-selector `0001`, P-selector `00000001` unchanged) |
| iec61850bean | `0000` | `0001` | sent as given |
| iec61850-rust | `0001` | `0001` | sent as given |
| cbsigma | `0001` | `0001` | sent as given (S-selector `0001`, P-selector `00000001` unchanged) |
| dscsystems | `0001` | `0001` | unsupported (exit 4): the library cannot set them |

An invalid value (odd length, not hex, more than 4 octets) is an argument
error (exit 1).

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
