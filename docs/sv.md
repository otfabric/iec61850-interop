# Sampled Values adapters

Sampled Values (IEC 61850-9-2) are raw Ethernet like GOOSE: EtherType
`0x88BA`, multicast destination, no IP. The SV adapter commands open an
`AF_PACKET` socket, so they need `CAP_NET_RAW` and a layer-2 segment shared
with the peer; the L2 recipe of [`goose.md`](goose.md#l2-networking)
applies unchanged (Docker bridge, `NET_RAW`, the host side of the bridge on
Linux, a container on macOS).

| Command | Image |
|---|---|
| `libiec61850-sv-publisher`, `libiec61850-sv-subscriber` | `iec61850-interop-libiec61850` |
| `dscsystems-sv-publisher`, `dscsystems-sv-subscriber` | `iec61850-interop-dscsystems` |
| `iec61850-rust-sv-publisher`, `iec61850-rust-sv-subscriber` | `iec61850-interop-iec61850-rust` |

iec61850bean has no Sampled Values support (no publisher, subscriber or
SVCB model), so its image has no SV commands.

All three stacks implement one command line and JSON Lines contract, so a
test switches peer by image and command name only.
The rules every command shares (`--version`, `--capabilities`,
stdout/stderr, the v1 stability promise) are in [contract.md](contract.md);
this page is the Sampled Values part of that contract.

## Fixture — `fixtures/sv/` (revision `sv-v1`)

- `interop-sv.cid` — `fixtures/iec61850/interop.icd` plus the 9-2LE
  merging-unit model: logical nodes `TCTR1..4` (`Amp`) and `TVTR1..4`
  (`Vol`) of CDC SAV (`instMag.i` INT32, `q`), the dataset `PhsMeas1`, the
  `SampledValueControl` blocks `MSVCB01` and `MSVCB02` and their `SMV`
  addresses. The validator checks that everything in `interop.icd` is in
  it unchanged.
- `sv.json` — the same streams as on the wire, with the member list and
  two value sets: `values` (sent from the start) and `update` (sent after
  the first `set`).

| Stream | svID | APPID | Destination | Dataset | Rate | ASDUs/frame |
|---|---|---|---|---|---|---|
| `MSVCB01` (9-2LE) | `InteropSv92LE` | `0x4000` | `01:0c:cd:04:00:00` | `PhsMeas1`: IA IB IC IN VA VB VC VN, each INT32 + quality | 80 samples/period at 50 Hz = 4000/s, smpCnt 0..3999 | 1 |
| `MSVCB02` (generic) | `InteropSvGeneric` | `0x4001` | `01:0c:cd:04:00:01` | `dsSvGeneric`: float32 (`TotW.mag.f`), quality (`TotW.q`), unsigned (`TotVAh.actVal`), quality (`TotVAh.q`), integer (`Mod.stVal`) | 1000 samples/s, smpCnt 0..999 | 4 |

Both: confRev 1, VLAN ID 0 with priority 4, smpSynch 0. On-wire references:
datSet `InteropIEDInteropLD/LLN0$PhsMeas1` / `InteropIEDInteropLD/LLN0$dsSvGeneric`.

`MSVCB02` and `dsSvGeneric` are also in the IED fixture
(`fixtures/iec61850/interop.icd`, revision `iec61850-v3`), so an IED server
whose stack models SV control blocks exposes `LLN0$MS$MSVCB02` over MMS.
`fixtures/capabilities.json` (`svcbOverMms`) records which do.

### 9-2LE profile

The ASDU carries svID, smpCnt, confRev, smpSynch and seqData only (no
datSet, refrTm, smpRate or smpMod, as in the UCA 9-2LE guideline). seqData
is 64 bytes: per channel a big-endian INT32 followed by a 32-bit quality
word. `values` is the eight channels in dataset order, each an `integer`
followed by its quality, so 16 typed values:

```json
[{"type":"integer","value":1000},{"type":"bit-string","value":"0000000000000"},
 {"type":"integer","value":-500},{"type":"bit-string","value":"0000000000000"}, ...]
```

### Generic profile

The ASDU carries every optional field: datSet, refrTm (time of the
sample), smpRate and smpMod. seqData is the dataset members in order with
the IEC 61850-9-2 fixed-length encoding: `integer` INT32 (4 bytes),
`unsigned` INT32U (4), `float32` (4), `float64` (8), `boolean` (1),
`bit-string` quality (4-byte word), `utc-time` (8, UtcTime). `values` is
one typed value per member.

### Quality

A quality is a 13-character `bit-string`, MSB-first as in the MMS value
JSON (validity is positions 0–1, `test` position 11). On the wire it is a
32-bit word in which position *i* is bit *i* counting from the least
significant bit, so validity `invalid` (`01…`) is `0x00000002`,
`questionable` (`11…`) `0x00000003` and `test` `0x00000800`. This is the
orientation of libiec61850, dscsystems and iec61850-rust and of Wireshark;
bits 13–31 (the 9-2LE `derived` flag and reserved bits) are written as
zero and not reported.

### Differences between the stacks

- **smpMod width.** libiec61850's and dscsystems' publishers encode smpMod
  in two octets, iec61850-rust's in one. dscsystems' and iec61850-rust's
  subscribers read either; libiec61850's SVReceiver accepts only one octet
  and drops an ASDU with two, so the generic stream (which carries smpMod)
  from libiec61850 or dscsystems never reaches `libiec61850-sv-subscriber`.
  CI checks those two pairs as known limitations: they must still fail
  with nothing received.
- **SV control blocks over MMS.** The libiec61850 and dscsystems IED
  servers expose `LLN0$MS$MSVCB02` (SvEna, MsvID, DatSet, ConfRev, SmpRate,
  OptFlds, SmpMod, DstAddress, noASDU); iec61850-rust and iec61850bean do
  not. OptFlds reads as 5 bits from libiec61850 and 10 bits from
  dscsystems.

## Contract

### `*-sv-publisher`

```
--iface IF --app-id 0xNNNN --dst MAC --vlan-id N --vlan-prio N --sv-id ID
--dataset REF --conf-rev N --smp-rate N
--smp-mod samples-per-period|samples-per-second|seconds-per-sample
--no-asdu N [--smp-synch 0|1|2] [--simulation] [--profile 9-2LE|generic]
[--frequency HZ] [--report-every N] --values JSON
```

Defaults: `--iface eth0`, `--vlan-id 0`, `--vlan-prio 4`, `--conf-rev 1`,
`--smp-rate 80`, `--smp-mod samples-per-period`, `--no-asdu 1`,
`--smp-synch 0`, `--profile 9-2LE`, `--frequency 50`, `--report-every 1000`.
`--app-id` accepts `0x4000` or a decimal number. `--dataset` is the datSet
reference (sent only by the generic profile). `--values` is a JSON array
of typed values; the 9-2LE profile needs exactly 16 (eight
`integer`/`bit-string` pairs).

1. Emits `{"event":"ready","address":IF,"fixture":"sv-v1","adapter":...,"version":...,"role":"publisher","iface":IF,"src":MAC,"svID":ID}`.
2. Publishes continuously. The sample rate is
   - `samples-per-period`: smpRate × `--frequency` samples/s,
   - `samples-per-second`: smpRate samples/s,
   - `seconds-per-sample`: one sample every smpRate seconds;

   one frame carries `--no-asdu` consecutive samples, so frames go out at
   sample rate / noASDU, paced against absolute deadlines (a late frame is
   sent at once, the schedule is not shifted).
3. smpCnt starts at 0 and increments per ASDU. It wraps at the number of
   samples per second (`samples-per-period`, `samples-per-second`; 4000 for
   the 9-2LE stream, as the 9-2LE guideline specifies) and at 65536 for
   `seconds-per-sample`.
4. Reads JSON Lines on stdin:
   - `{"cmd":"set","values":[...]}` — every frame after it carries the
     new values.
   - `{"cmd":"stop"}` — emits `{"event":"stopped","frames":F,"samples":S}`
     (the totals) and exits 0. SIGTERM/SIGINT do the same.
   - EOF on stdin is not a stop (as for GOOSE).
5. Emits `{"event":"published","reason":R,"frames":F,"samples":S,"smpCnt":N}`
   — `frames`/`samples` are totals since the start, `smpCnt` that of the
   last ASDU of the frame — for the first frame (`reason` `start`), the
   first frame after each `set` (`set`), and each frame that takes
   `samples` past a multiple of `--report-every` (`periodic`).
6. A rejected stdin line or a send failure emits
   `{"event":"error","error":"..."}` and the publisher carries on.

`--simulation` sets the Edition 2 S bit (MSB of Reserved1), as for GOOSE.
Frames are always 802.1Q-tagged with `--vlan-id`/`--vlan-prio`.

Exit codes: 0 stop or signal, 1 argument or socket error.

### `*-sv-subscriber`

```
--iface IF [--app-id 0xNNNN] [--sv-id ID] [--dst MAC] [--sample-every N]
[--fixture PATH]
```

Filters are ANDed; without any, every SV frame on IF is reported.
`--sample-every` defaults to 1. `--fixture` defaults to
`/fixtures/sv/sv.json`: the subscriber decodes seqData with the member
list of the stream whose svID matches; for an unknown svID it decodes
nothing and reports `"values":null` plus `"seqData":"<hex>"`.

1. Emits `{"event":"ready",...,"role":"subscriber","iface":IF,"src":MAC}`.
2. For each accepted ASDU whose smpCnt is a multiple of `--sample-every`:

```json
{"event":"sv","appID":"0x4000","dst":"01:0c:cd:04:00:00","src":"02:42:ac:12:00:02",
 "svID":"InteropSv92LE","datSet":null,"smpCnt":0,"confRev":1,
 "refrTm":null,"smpSynch":0,"smpRate":null,"smpMod":null,
 "simulation":false,"asduIndex":0,"noASDU":1,
 "values":[{"type":"integer","value":1000},{"type":"bit-string","value":"0000000000000"}, ...]}
```

   Absent optional fields are `null`. `smpMod` is the string form
   (`samples-per-period`, `samples-per-second`, `seconds-per-sample`).
   `refrTm` is formatted like the GOOSE `t`. `asduIndex` is the 0-based
   position of the ASDU in its frame, `noASDU` the frame's ASDU count.
3. Once per second after `ready`, cumulative over all accepted ASDUs:
   `{"event":"stats","received":N,"lost":N,"duplicates":N,"outOfOrder":N}`.

Counting, per stream (APPID, svID): the first ASDU only sets the reference.
Then with `d = smpCnt − previous`: `d = 1` in order; `d = 0` a duplicate;
`d > 1` adds `d − 1` to `lost`; `d < 0` is a wrap when
`previous − smpCnt > wrap / 2` and adds `(wrap − 1 − previous) + smpCnt` to
`lost`, otherwise it counts as `outOfOrder` and the reference stays. `wrap`
is `smpCntWrap` of the fixture stream with that svID, else the largest
smpCnt seen plus one. `received` counts every accepted ASDU.

Exit codes: 0 on signal, 1 argument or socket error.

### Field notes

- `appID`, MACs: as for GOOSE (`0x` + four lower-case hex digits;
  lower-case `:`-separated MACs).
- The VLAN tag of a received frame is not reported (VLAN offload strips
  it, as for GOOSE).
- `noASDU`, `role`, `reason`, `frames`, `samples`, `stopped`, `seqData`,
  `--frequency`, `--report-every` and `--fixture` are additions to the
  minimal contract.

## Cross-check

`scripts/sv-crosscheck.sh PUB_IMAGE PUB_CMD SUB_IMAGE SUB_CMD [STREAM]`
runs a publisher and a subscriber on a Docker bridge for a few seconds,
sends one `set`, and checks the subscriber's values against `sv.json`
before and after it, the header fields, and loss: the samples the
publisher reports in `stopped` against the subscriber's `received`, and
the subscriber's own `lost`/`duplicates`/`outOfOrder` (at most 1 % lost or
reordered, no duplicates). It prints the loss figures. `FLAGS=all`
adds `--simulation` and `--smp-synch 2`; `HOST_SIDE=1` runs the
subscriber on the host side of the bridge, as for GOOSE. CI
runs it for every publisher → subscriber pair of the SV stacks and both
streams.
