# GOOSE adapters and L2 networking

GOOSE (IEC 61850-8-1 clause 18) is raw Ethernet: EtherType `0x88B8`,
multicast destination, no IP. The GOOSE adapter commands open an
`AF_PACKET` socket on a network interface, so they need `CAP_NET_RAW` and
a layer-2 segment shared with the peer under test.

| Command | Image |
|---|---|
| `libiec61850-goose-publisher`, `libiec61850-goose-subscriber` | `mms-interop-libiec61850` |
| `dscsystems-goose-publisher`, `dscsystems-goose-subscriber` | `mms-interop-dscsystems` |
| `iec61850-rust-goose-publisher`, `iec61850-rust-goose-subscriber` | `mms-interop-iec61850-rust` |

All three stacks implement the same command line and JSON Lines contract, so a
test switches peer by image and command name only.

## Fixture — `fixtures/goose/`

- `interop-goose.cid` — `fixtures/iec61850/interop.icd` (same IED
  `InteropIED`, LD `InteropLD`, logical nodes and DataTypeTemplates; the
  validator checks they stay identical) plus two datasets, two
  `GSEControl` blocks in `LLN0` and their `GSE` addresses.
- `goose.json` — the same control blocks as on the wire, with the member
  list and two value sets: `values` (initial state, stNum 1) and `update`
  (state of the first `set`).

| Control block | gocbRef | APPID | Destination | Dataset |
|---|---|---|---|---|
| `gcbAll` | `InteropIEDInteropLD/LLN0$GO$gcbAll` | `0x1001` | `01:0c:cd:01:00:01` | `dsGooseAll`: boolean (`SPS1.stVal`), integer (`Mod.stVal`), unsigned (`TotVAh.actVal`), float32 (`TotW.mag.f`), quality bit-string (`SPS1.q`), utc-time (`SPS1.t`), Dbpos bit-string (`DPS1.stVal`), nested structure (`MMXU1.TotW[MX]` = `{ {f}, q, t }`) |
| `gcbBool` | `InteropIEDInteropLD/LLN0$GO$gcbBool` | `0x1002` | `01:0c:cd:01:00:02` | `dsGooseBool`: `SPS1.stVal`, `SPS1.q` |

Both: goID = the SCL `appID` (`InteropGooseAll`, `InteropGooseBool`),
confRev 1, VLAN ID 0 with priority 4, MinTime 10 ms, MaxTime 1000 ms.

## Contract

### `*-goose-publisher`

```
--iface IF --gocb-ref REF --go-id ID --dataset REF --app-id 0xNNNN
--dst MAC --vlan-id N --vlan-prio N --conf-rev N --min-time ms --max-time ms
[--test] [--simulation] [--nds-com] --values JSON
```

Defaults: `--iface eth0`, `--go-id` = gocbRef, `--dataset ""`,
`--vlan-id 0`, `--vlan-prio 4`, `--conf-rev 1`, `--min-time 10`,
`--max-time 1000`. `--app-id` accepts `0x1001` (hex) or a decimal number.
`--values` is a JSON array of typed values (below).

1. Emits `{"event":"ready","address":IF,"fixture":"goose-v1","adapter":...,"version":...,"role":"publisher","iface":IF,"src":MAC,"gocbRef":REF}`.
2. Publishes `--values` as state 1: stNum 1, sqNum 0.
3. Retransmits on the IEC 61850-8-1 schedule: MinTime, then doubling,
   capped at MaxTime, which repeats (10, 20, 40, …, 640, 1000, 1000, … ms
   with the defaults). Each frame's timeAllowedToLive is twice the
   interval until the next frame.
4. Reads JSON Lines on stdin:
   - `{"cmd":"set","values":[...]}` — state change: stNum+1, sqNum 0, the
     schedule restarts, `t` is the time of the change.
   - `{"cmd":"stop"}` — exits 0.
   - EOF on stdin is not a stop: the publisher keeps publishing until
     SIGTERM/SIGINT (so `docker run` without `-i` works).
5. Emits one `{"event":"published","stNum":N,"sqNum":N,"timeAllowedToLive":ms}`
   per frame put on the wire.
6. A rejected stdin line or a send failure emits
   `{"event":"error","error":"..."}` and the publisher carries on.

Frames are always 802.1Q-tagged with `--vlan-id`/`--vlan-prio`
(libiec61850's default; priority tagging is how GOOSE gets precedence).

Exit codes: 0 stop or signal, 1 argument or socket error.

### `*-goose-subscriber`

```
--iface IF [--app-id 0xNNNN] [--gocb-ref REF] [--dst MAC]
```

Filters are ANDed; without any, every GOOSE frame on IF is reported.

1. Emits `{"event":"ready",...,"role":"subscriber","iface":IF,"src":MAC}`.
2. Per accepted frame:

```json
{"event":"goose","gocbRef":"InteropIEDInteropLD/LLN0$GO$gcbAll",
 "datSet":"InteropIEDInteropLD/LLN0$dsGooseAll","goID":"InteropGooseAll",
 "appID":"0x1001","dst":"01:0c:cd:01:00:01","src":"02:42:ac:12:00:02",
 "stNum":1,"sqNum":0,"timeAllowedToLive":20,"confRev":1,
 "test":false,"simulation":false,"ndsCom":false,"numDatSetEntries":8,
 "t":"2026-10-28T02:36:12.244363009Z",
 "values":[{"type":"boolean","value":false},{"type":"integer","value":1}, ...]}
```

3. Supervision, per gocbRef: when the timeAllowedToLive of the last frame
   passes without another frame,
   `{"event":"supervision","gocbRef":REF,"state":"timeout"}`; the next
   frame of that gocbRef emits `{"event":"supervision","gocbRef":REF,"state":"ok"}`
   before its `goose` event. There is no `ok` for the first frame.

Exit codes: 0 on signal, 1 argument or socket error.

### Field notes

- `appID` is `0x` + four lower-case hex digits; MACs are lower-case,
  `:`-separated.
- `t` is RFC 3339 in UTC with up to nine fraction digits, trailing zeros
  dropped (Go's `time.RFC3339Nano`). The UtcTime fraction converts as
  `(fraction * 1e9) >> 24` in both adapters.
- `test` is goosePdu field `[7]` — `test` in Edition 1, renamed
  `simulation` in the Edition 2 ASN.1. `simulation` is the Edition 2 S bit
  (MSB of Reserved1 in the APDU header). `--test` and `--simulation` set
  them independently. libiec61850 calls field `[7]`
  `GoosePublisher_setSimulation` / `GooseSubscriber_isTest` and has no API
  for the S bit: its adapters set and read the bit in the frame
  themselves (the publisher then encodes with
  `GoosePublisher_publishAndDump` on `lo` and sends the patched frame on IF).
- The VLAN tag of a received frame is not reported: Linux strips it before
  an `AF_PACKET` socket sees the frame (VLAN offload), unless the reader
  asks for auxiliary data, which neither stack does.
- `numDatSetEntries` and `role` are additions to the minimal contract.

### Value JSON

The typed form of the MMS value contract: `{"type":T,"value":V}`.

| type | value |
|---|---|
| `boolean` | JSON boolean |
| `integer`, `unsigned` | JSON number |
| `float32`, `float64` | JSON number; float32 in the shortest form that reads back as the same float32 (`0.1`, not `0.10000000149011612`) |
| `visible-string`, `mms-string` | JSON string |
| `octet-string` | lower-case hex |
| `bit-string` | MSB-first binary digits, length = number of bits (quality: 13) |
| `utc-time`, `binary-time` | `YYYY-MM-DDThh:mm:ss.mmmZ` (millisecond precision, rounded) |
| `array`, `structure` | JSON array of typed values, in order |

## L2 networking

Run the adapters on a user-defined Docker bridge network with a fixed
bridge device name, and give them `NET_RAW` (and `NET_ADMIN` if the
libiec61850 subscriber should switch the interface to promiscuous mode;
it works without, a Docker bridge floods multicast to every port):

```bash
docker network create \
  -o com.docker.network.bridge.name=goose0 \
  mms-interop-goose

docker run -d --name goose-sub --network mms-interop-goose \
  --cap-add NET_RAW --cap-add NET_ADMIN \
  --entrypoint libiec61850-goose-subscriber \
  ghcr.io/otfabric/mms-interop-libiec61850:v0.3.0 --iface eth0 --app-id 0x1001
```

Inside a container the interface is `eth0`. The GOOSE binaries carry the
file capability `cap_net_raw+ep`, so they run as the image's non-root
user as long as the container has `NET_RAW` (part of Docker's default
set). With `--security-opt no-new-privileges` file capabilities are not
honoured: add `--user 0` then.

`docker compose --profile goose up` starts a libiec61850 publisher and a
dscsystems subscriber on this network (see `docker-compose.yml`).

### Go test on a Linux host

Two options, both used by CI:

1. **Host side of the bridge.** The bridge device `goose0` is a port of
   the same L2 segment. A process on the host (or a container with
   `--network host`) opens `goose0`: frames the adapters send arrive on it,
   and frames sent on it reach every container on the network. The test
   needs `CAP_NET_RAW`, e.g. `go test -exec sudo -tags=interop ./...` or
   `sudo setcap cap_net_raw+ep` on the compiled test binary.

   ```bash
   GOOSE_IFACE=goose0 go test -exec sudo -tags=interop ./interop/...
   ```

   A bridge without any container attached has no carrier, and the first
   container to join sees its first second or so of sends stall while
   the bridge comes up (retransmissions are lost, not delayed). Attach the
   adapter you do not test the timing of first, or an idle container
   (`docker run -d --network mms-interop-goose --entrypoint sleep IMAGE infinity`),
   and wait a second before the publisher starts. With adapters on both
   ends in containers, the first one started is that port.

2. **Test in a golang container on the network.** Works on any Docker
   host:

   ```bash
   docker run --rm --network mms-interop-goose --cap-add NET_RAW \
     -v "$PWD":/src -w /src golang:1.27 \
     env GOOSE_IFACE=eth0 go test -tags=interop ./interop/...
   ```

   The test container starts and stops the adapter containers through the
   Docker socket (`-v /var/run/docker.sock:/var/run/docker.sock`) or the
   harness starts them beforehand.

`scripts/goose-crosscheck.sh PUB_IMAGE PUB_CMD SUB_IMAGE SUB_CMD [CB]`
is the reference for driving a publisher/subscriber pair: it is what CI
runs between every pair of libiec61850, dscsystems and iec61850-rust.

### macOS (and Windows) hosts

Docker Desktop runs containers in a Linux VM. The `goose0` bridge lives
in that VM, not on the macOS host, and macOS has no `AF_PACKET`, so a Go
test running natively on macOS cannot exchange GOOSE frames with the
adapters. Run the Go side inside a container on `mms-interop-goose`
(option 2), or with `--network host`, which on Docker Desktop is the VM's
network namespace and therefore sees `goose0` (option 1 inside the VM).
