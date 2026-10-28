# mms-interop

Interoperability **infrastructure** for [go-mms](https://github.com/otfabric/go-mms) and [go-iec61850](https://github.com/otfabric/go-iec61850).

This repository provides adapter images, fixture files and the adapter output contract. It does not own assertions about the Go libraries. Interoperability tests and compatibility matrices live in the Go repositories and consume the infrastructure here.

```
               mms-interop
          containers + fixtures
               /           \
              /             \
         go-mms           go-iec61850
      interop/             interop/
   owns MMS tests      owns IEC 61850 tests
```

## Purpose

- Publish reproducible adapter images for libiec61850, iec61850bean and dscsystems/go-iec61850.
- Define the canonical fixture models and runtime values that all interop tests rely on.
- Specify the JSON Lines adapter output contract that adapters must implement.
- Keep the adapter implementations simple and auditable against upstream library examples.

## Repository structure

```
mms-interop/
├── README.md
├── PLAN.md
├── COVERAGE.md               # Adapter command availability
├── Makefile
├── docker-compose.yml
├── docs/
│   └── goose.md              # GOOSE contract and L2 networking
│
├── fixtures/
│   ├── mms/
│   │   └── interop.json      # MMS server model and initial variable state
│   ├── iec61850/
│   │   ├── interop.icd       # SCL model (logical devices, nodes, data objects)
│   │   └── values.json       # Runtime values and writable attribute list
│   └── goose/
│       ├── interop-goose.cid # interop.icd + GOOSE datasets, GSEControl, GSE addresses
│       └── goose.json        # GOOSE control blocks, members, test values
│
└── adapters/
    ├── libiec61850/
    │   ├── Dockerfile
    │   ├── SPIKE.md           # Phase 0 findings
    │   └── src/
    ├── iec61850bean/
    │   ├── Dockerfile
    │   └── src/
    └── dscsystems/            # GPL-3.0 (LICENSE, NOTICE in this directory)
        ├── Dockerfile
        ├── cmd/
        └── internal/
```

## Library boundary

| Library | Responsibility |
|---|---|
| `go-mms` | ISO session / presentation / ACSE, MMS association, BER encoding, generic MMS services, named variable lists |
| `go-iec61850` | IEC 61850 object references, functional constraints, logical-node discovery, datasets, reports, controls, SCL handling — uses `go-mms` underneath |

`go-mms` must never accumulate IEC 61850 semantics, even as a convenience.

## Adapters

Third-party implementations run as separate Docker containers. They are intentionally simple: load a fixture, start a server or execute a fixed operation sequence, emit structured JSON Lines, exit.

### libiec61850

[libIEC61850](https://libiec61850.com) is the primary reference. It exposes a distinct low-level generic MMS API and a separate high-level IEC 61850 API, which maps cleanly onto the `go-mms` / `go-iec61850` split.

libiec61850 is built and executed as a separate GPLv3-licensed containerised program and is not linked into the MIT-licensed Go libraries. It is downloaded from a pinned release archive during the Docker build. Distribution of any image containing it must preserve the applicable GPLv3 notices and corresponding source obligations.

The authoritative machine-readable command list is [`fixtures/capabilities.json`](fixtures/capabilities.json) (also rendered as [`COVERAGE.md`](COVERAGE.md)). Do not treat README prose as a capability claim.

Commands currently declared for libiec61850:

- `libiec61850-mms-server` — generic MMS server loaded from `fixtures/mms/interop.json`
- `libiec61850-mms-client` — generic MMS client, executes a fixed sequence and emits JSON Lines
- `libiec61850-ied-server` — IEC 61850 IED server loaded from `fixtures/iec61850/`
- `libiec61850-ied-client` — IEC 61850 general browse/read/write/dataset client (fixed sequence)
- `libiec61850-ied-controller` — IEC 61850 control client (direct / SBO / SBOw against SPCSO*)
- `libiec61850-ied-reporter` — IEC 61850 client that subscribes to URCB and emits report fields as JSON Lines
- `libiec61850-goose-publisher` — GOOSE publisher (raw Ethernet), state changes from stdin
- `libiec61850-goose-subscriber` — GOOSE subscriber, one JSON line per frame plus TAL supervision

### iec61850bean

[IEC61850bean](https://www.beanit.com/iec-61850/) (Apache-2.0) is the secondary reference. It provides an independently implemented ASN.1/BER encoder, ACSE state machine, MMS protocol handler and IEC 61850 mapping — genuine protocol diversity rather than a language wrapper over the same core.

`iec61850bean` is used at the IEC 61850 semantic layer only. Its MMS layer is not exposed through a practical generic MMS API, so generic MMS interoperability is covered by the libiec61850 directions.

Commands currently declared for iec61850bean (see `fixtures/capabilities.json`):

- `iec61850bean-ied-server` — IEC 61850 IED server loaded from `fixtures/iec61850/`
- `iec61850bean-ied-client` — IEC 61850 general browse/read/write/dataset client (fixed sequence)
- `iec61850bean-ied-controller` — IEC 61850 control client (direct / SBO / SBOw against SPCSO*)
- `iec61850bean-ied-reporter` — IEC 61850 URCB reporter client (JSON Lines)

### dscsystems

[dscsystems/go-iec61850](https://github.com/dscsystems/go-iec61850) (GPL-3.0) is a third, independent stack: pure Go MMS, IEC 61850 and GOOSE, sharing no code with libiec61850, iec61850bean or the otfabric libraries. The adapter is pinned to upstream commit `6bf3f37` (`v0.6.5-1-g6bf3f37`, in `adapters/dscsystems/go.mod` and the Dockerfile).

It offers the libiec61850 command set under the `dscsystems-` prefix, with the same flags, fixture defaults and JSON Lines, so a consumer harness switches stacks by image and command prefix alone:

- `dscsystems-mms-server`, `dscsystems-mms-client`
- `dscsystems-ied-server`, `dscsystems-ied-client`, `dscsystems-ied-controller`, `dscsystems-ied-reporter`
- `dscsystems-goose-publisher`, `dscsystems-goose-subscriber`

Where the library behaves differently, the gap is a known limitation in `fixtures/capabilities.json`.

**Licensing boundary.** dscsystems code lives only in `adapters/dscsystems/` and the `mms-interop-dscsystems` image. That directory is GPL-3.0 (`LICENSE`, `NOTICE`); the rest of the repository stays MIT. Nothing from it may be copied or vendored into the MIT-licensed `go-mms` or `go-iec61850`: they talk to the adapter only over the network and its JSON Lines, as a separate program.

## GOOSE

GOOSE is layer 2. The publisher and subscriber commands of libiec61850 and dscsystems share one contract and one fixture (`fixtures/goose/`). They need `CAP_NET_RAW` and a Docker bridge network shared with the peer; a Go test on a Linux host joins through the bridge device, on macOS it has to run in a container. [`docs/goose.md`](docs/goose.md) has the contract, the fixture and the networking recipes.

## Fixtures

### MMS — `fixtures/mms/interop.json`

Defines domains, variables, types and initial values. Consumed by the libiec61850 C adapter. Consumer repositories (`go-mms`) carry a synchronized copy in `interop/testdata/`.

Initial type baseline: `boolean`, `integer`, `unsigned`, `float32`, `visible-string`, `octet-string`, `bit-string`, `utc-time`, `array`, `structure`.

New types are added only when `go-mms` claims support for them or when they represent a known interoperability risk.

### IEC 61850 — `fixtures/iec61850/`

`interop.icd` is the canonical SCL model file (logical devices, logical nodes, data objects, datasets, report control blocks). `values.json` supplies mutable runtime state that SCL cannot express.

Writable scalar setting used by consumer write e2e (ING CDC):

```text
InteropLD/GGIO1.SetInt1.setVal[SP]
```

Do not treat `Mod.ctlModel` / `Mod.d` as assumed-success write targets.

Consumer repositories (`go-iec61850`, `iec61850ctl`) carry synchronized copies in `interop/testdata/` / `e2e/testdata/`. The adapter image version and fixture revision must remain compatible — they are updated together.

### GOOSE — `fixtures/goose/`

Revision `goose-v1`. `interop-goose.cid` is `interop.icd` plus two GOOSE datasets (`dsGooseAll`: boolean, integer, unsigned, float32, quality, utc-time, Dbpos and a nested structure; `dsGooseBool`), their `GSEControl` blocks and `GSE` addresses. `goose.json` gives the same control blocks as on-wire references with initial and update values. `scripts/validate-fixtures.py` checks the two against each other and against `interop.icd`.

## Consuming the adapter images

Consumer repositories pin a specific adapter image version tag for local use and a digest for CI:

```bash
# Local development — version tag
LIBIEC61850_IMAGE=ghcr.io/otfabric/mms-interop-libiec61850:v0.3.0 make interop
DSCSYSTEMS_IMAGE=ghcr.io/otfabric/mms-interop-dscsystems:v0.3.0 make interop

# CI — digest-pinned for full reproducibility
LIBIEC61850_IMAGE=ghcr.io/otfabric/mms-interop-libiec61850@sha256:<digest> make interop
```

Each test lifecycle:

1. Starts the adapter container with `docker run`.
2. Waits for the readiness event on stdout (`{"event":"ready",...}`).
3. Exercises the Go library under test.
4. Collects adapter stdout (JSON Lines) and asserts results.
5. Stops the container.

No pre-running containers are required. Tests run with `-tags=interop` and `make interop` in each Go repository.

## Adapter output contract

Server readiness event (emitted to stdout before accepting connections):

```json
{"event":"ready","address":"0.0.0.0:102","fixture":"iec61850-v1","adapter":"libiec61850","version":"0.1.0"}
```

`fixture` identifies the canonical fixture revision consumed. `version` is the adapter image version, set via the `ADAPTER_VERSION` build argument and defaulting to `dev`.

Client adapter output (one JSON Line per operation):

```jsonl
{"operation":"identify","ok":true,"value":{"vendor":"OTFabric","model":"MMS-Interop","revision":"1.0"}}
{"operation":"read","target":"interop/boolean","ok":true,"value":true}
{"operation":"write","target":"interop/float32","ok":true}
{"operation":"conclude","ok":true}
```

Diagnostics go to stderr. Stdout must contain only JSON Lines.

GOOSE commands are long-running: after `ready` they emit `published` (publisher) or `goose` and `supervision` (subscriber) events, and the publisher reads `{"cmd":"set"}` / `{"cmd":"stop"}` on stdin. See [`docs/goose.md`](docs/goose.md).

## Test directions

Primary directions (tests live in `go-mms`, `go-iec61850`, and consumer CLIs such as `iec61850ctl`):

| Direction | Layer | Go role | Adapter command(s) | Phase |
|---|---|---|---|---|
| go-mms client → libiec61850 MMS server | MMS | Client | `libiec61850-mms-server` | 1A |
| libiec61850 MMS client → go-mms server | MMS | Server | `libiec61850-mms-client` | 1B |
| go-iec61850 / CLI client → libiec61850 IED server | IEC 61850 | Client | `libiec61850-ied-server` | 2A |
| libiec61850 client/controller/reporter → go-iec61850 / CLI server | IEC 61850 | Server | `libiec61850-ied-client`, `-controller`, `-reporter` | 2A / 3A / 3B |
| go-iec61850 / CLI client → iec61850bean IED server | IEC 61850 | Client | `iec61850bean-ied-server` | 2B |
| iec61850bean client/controller/reporter → go-iec61850 / CLI server | IEC 61850 | Server | `iec61850bean-ied-client`, `-controller`, `-reporter` | 2B / 3A / 3B |
| go-mms client ↔ dscsystems MMS server / client | MMS | Client / Server | `dscsystems-mms-server`, `dscsystems-mms-client` | 4 |
| go-iec61850 ↔ dscsystems IED server / clients | IEC 61850 | Client / Server | `dscsystems-ied-server`, `-client`, `-controller`, `-reporter` | 4 |
| go-iec61850 GOOSE publisher → libiec61850 / dscsystems subscriber | GOOSE | Publisher | `*-goose-subscriber` | 5 |
| libiec61850 / dscsystems publisher → go-iec61850 GOOSE subscriber | GOOSE | Subscriber | `*-goose-publisher` | 5 |

Fixed-sequence `*-ied-client` adapters exit `3` when any operation emits `ok:false` (after attempting conclude). Connection failure remains exit `2`.

## Building and publishing adapter images

```bash
# Build all three adapter images locally
make build

# GOOSE publisher -> subscriber, libiec61850 <-> dscsystems
make goose-crosscheck

# Publish to registry (requires REGISTRY and VERSION or defaults)
make publish
```

Docker Compose is provided for manual inspection:

```bash
docker compose up libiec61850-mms-server
docker compose down
```

## Running interop tests

Interop tests live in the Go repositories. To run them against locally built images:

```bash
# In go-mms
LIBIEC61850_IMAGE=mms-interop-libiec61850:local make interop

# In go-iec61850
LIBIEC61850_IMAGE=mms-interop-libiec61850:local \
IEC61850BEAN_IMAGE=mms-interop-iec61850bean:local \
DSCSYSTEMS_IMAGE=mms-interop-dscsystems:local \
make interop
```

## Prerequisites

- Docker with BuildKit / buildx
- Make

## License

The repository's original code and documentation are MIT-licensed unless otherwise noted.

Third-party source code, Docker images and generated artifacts retain their respective upstream licenses. libiec61850 is GPLv3; iec61850bean is Apache-2.0; dscsystems/go-iec61850 is GPLv3. `adapters/dscsystems/` links dscsystems/go-iec61850 and is therefore GPL-3.0 itself (see its `LICENSE` and `NOTICE`). Distribution of images containing any of these libraries must preserve the applicable upstream notices and source obligations.
