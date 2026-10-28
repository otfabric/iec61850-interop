# Adapter Command Coverage

Availability of adapter commands across the four images. Compatibility matrices for the Go libraries live in [`go-mms/INTEROP.md`](https://github.com/otfabric/go-mms/blob/main/INTEROP.md) and [`go-iec61850/INTEROP.md`](https://github.com/otfabric/go-iec61850/blob/main/INTEROP.md).

This file is generated from `fixtures/capabilities.json`; do not edit manually. Run `./scripts/gen-coverage.sh`.

Adapter version: `1.0.0` · Fixture revisions: `mms-v2`, `iec61850-v3`, `goose-v1`, `sv-v1`

## Stack × capability

| Stack | Image | MMS server | MMS client | IED server | IED client | IED controller | IED reporter | GOOSE publisher | GOOSE subscriber | SV publisher | SV subscriber | SVCB over MMS |
|-------|-------|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| libiec61850 | `iec61850-interop-libiec61850` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| iec61850bean | `iec61850-interop-iec61850bean` | — | — | ✓ | ✓ | ✓ | ✓ | — | — | — | — | — |
| dscsystems | `iec61850-interop-dscsystems` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| iec61850-rust | `iec61850-interop-iec61850-rust` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |

SVCB over MMS: the IED server exposes `LLN0$MS$MSVCB02` of the IED fixture as MMS variables.

## Commands

| Adapter command | Image | Available |
|----------------|-------|:---------:|
| `libiec61850-mms-server` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-mms-client` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-ied-server` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-ied-client` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-ied-controller` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-ied-reporter` | `iec61850-interop-libiec61850` | ✓ |
| `iec61850bean-ied-server` | `iec61850-interop-iec61850bean` | ✓ |
| `iec61850bean-ied-client` | `iec61850-interop-iec61850bean` | ✓ |
| `iec61850bean-ied-controller` | `iec61850-interop-iec61850bean` | ✓ |
| `iec61850bean-ied-reporter` | `iec61850-interop-iec61850bean` | ✓ |
| `libiec61850-goose-publisher` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-goose-subscriber` | `iec61850-interop-libiec61850` | ✓ |
| `dscsystems-mms-server` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-mms-client` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-ied-server` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-ied-client` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-ied-controller` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-ied-reporter` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-goose-publisher` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-goose-subscriber` | `iec61850-interop-dscsystems` | ✓ |
| `iec61850-rust-mms-server` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-mms-client` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-ied-server` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-ied-client` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-ied-controller` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-ied-reporter` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-goose-publisher` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-goose-subscriber` | `iec61850-interop-iec61850-rust` | ✓ |
| `libiec61850-sv-publisher` | `iec61850-interop-libiec61850` | ✓ |
| `libiec61850-sv-subscriber` | `iec61850-interop-libiec61850` | ✓ |
| `dscsystems-sv-publisher` | `iec61850-interop-dscsystems` | ✓ |
| `dscsystems-sv-subscriber` | `iec61850-interop-dscsystems` | ✓ |
| `iec61850-rust-sv-publisher` | `iec61850-interop-iec61850-rust` | ✓ |
| `iec61850-rust-sv-subscriber` | `iec61850-interop-iec61850-rust` | ✓ |

## Known limitations (verified upstream gaps)

| Stack | Version | Direction | Capability | Expected skip |
|-------|---------|-----------|------------|---------------|
| `libiec61850` | 1.6.2.1-89-g6e265c54 | libiec61850-server → go-client | InformationReport, listOfVariable form | `TestClient_InformationReport` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | go-client → libiec61850-server | FileOpen initialPosition | `TestClient_FileOpen_InitialPosition` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | go-client → libiec61850-server | DeleteNamedVariableList scopeOfDelete domain/vmd (DeleteAll*NVLs) | `TestClient_DeleteAllNVLs_UnsupportedByPeer` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | go-client → libiec61850-server | ObtainFile | `TestClient_ObtainFile` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | libiec61850-client → go-server | FileOpen response without lastModified | `TestServer_FileOpen_NoLastModified` |
| `iec61850bean` | 1.x | go-client → iec61850bean-server | SBOw SelectWithValue | `TestBeanClient_Control_SBOwOperate` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | go-client → dscsystems-server | Alternate access indexRange (read and write) | `TestClient_ReadArrayRange, TestClient_ReadVariables_WriteVariables_AlternateAccess` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | dscsystems-client → go-server | Alternate access read/write (array element, index range, component) | `TestServer_ReadArrayElements, TestServer_ReadComponent, TestServer_WriteArrayElements, TestServer_WriteComponent` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | dscsystems-client → go-server | Write to a named variable list | `TestServer_NVL_DomainScope, TestServer_NVL_VMDScope, TestServer_InformationReport` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | dscsystems-client → go-server | Association-specific named variable lists | `TestServer_NVL_AssociationScope` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | dscsystems-client → go-server | FileOpen lastModified | `TestServer_FileOpenReadClose, TestServer_FileOpen_InitialPosition, TestServer_FileRead_MultiChunk` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | iec61850bean-client → dscsystems-ied-server | GetVariableAccessAttributes type of Enum (read LLN0.Mod.ctlModel[CF]) | — |
| `dscsystems` | v0.6.5-1-g6bf3f37 | go-client → dscsystems-ied-server | Write of FC ST (and the dchg report it would trigger) | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | libiec61850-client → iec61850-rust-ied-server | SetURCBValues RptEna=true without prior Resv (implicit URCB reservation) | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | go-client → iec61850-rust-ied-server | Write of FC ST (and the dchg report it would trigger) | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | iec61850-rust-client → go-server | Abort (A-ABORT) | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | go-client → iec61850-rust-server | ObtainFile | `TestClient_ObtainFile` |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | go-client → iec61850-rust-server | DeleteNamedVariableList with domain/VMD scope | `TestClient_DeleteAllNVLs_UnsupportedByPeer` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | libiec61850-sv-publisher / dscsystems-sv-publisher → libiec61850-sv-subscriber | Sampled Values ASDUs carrying smpMod (the generic stream, MSVCB02) | `scripts/sv-crosscheck.sh … MSVCB02` |
| `dscsystems` | v0.6.5-1-g6bf3f37 | go-client → dscsystems-ied-server | MSVCB OptFlds width | — |
| `iec61850bean` | 1.x | — | Sampled Values | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | go-client → iec61850-rust-ied-server | SVCB over MMS (LLN0$MS$MSVCB02) | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | * → iec61850-rust-sv-subscriber, iec61850-rust-sv-publisher → * | More than 10 ASDUs per SV frame | — |
| `iec61850-rust` | v0.2.0-0-gd4e8952 | * → iec61850-rust-sv-subscriber | SV receive buffer | — |
| `dscsystems` | v0.6.5-1-g6bf3f37 | * → dscsystems-sv-subscriber | smpRate 0 / empty datSet | — |

## Notes

- All adapter commands read from the fixture files in `fixtures/`.
- All commands emit JSON Lines to stdout; diagnostics go to stderr. The contract is [`docs/contract.md`](docs/contract.md).
- Each adapter command supports `--capabilities` (emit JSON and exit) and `--version` (emit version JSON and exit).
- Fixed-sequence IED clients exit non-zero when any operation emits `ok:false` (connection failures remain exit 2).
- Every IED server seeds its values from `fixtures/iec61850/values.json` (including `GGIO1.DPS1.stVal` = 2 and `MMTR1.TotVAh.actVal` = 42). Every fixed-sequence IED client reads, in this order: `GGIO1.SPS1.stVal[ST]`, `MMXU1.TotW.mag.f[MX]`, `GGIO1.DPS1.stVal[ST]` (Dbpos, reported as its enumeration 0..3, 2 = on), `MMTR1.TotVAh.actVal[ST]` (INT32U), `LLN0.Mod.ctlModel[CF]`, `LLN0.Mod.d[DC]`.
- GOOSE commands (`*-goose-publisher`, `*-goose-subscriber`) need a raw socket: run them with `--cap-add NET_RAW` on a shared Docker network; see [`docs/goose.md`](docs/goose.md).
- Sampled Values commands (`*-sv-publisher`, `*-sv-subscriber`) need the same; see [`docs/sv.md`](docs/sv.md). iec61850bean has no SV support, so `iec61850-interop-iec61850bean` has no SV commands.
- `iec61850-interop-dscsystems` is built from GPL-3.0 code (dscsystems/go-iec61850); see `adapters/dscsystems/NOTICE`.
- A skipped test without a registered limitation in this table must fail CI.
