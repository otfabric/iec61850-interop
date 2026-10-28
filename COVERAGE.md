# Adapter Command Coverage

Availability of adapter commands across the two images. Compatibility matrices for the Go libraries live in [`go-mms/INTEROP.md`](https://github.com/otfabric/go-mms/blob/main/INTEROP.md) and [`go-iec61850/INTEROP.md`](https://github.com/otfabric/go-iec61850/blob/main/INTEROP.md).

This file is generated from `fixtures/capabilities.json`; do not edit manually. Run `./scripts/gen-coverage.sh`.

Adapter version: `0.2.0` · Fixture revisions: `mms-v2`, `iec61850-v2`

| Adapter command | Image | Available |
|----------------|-------|:---------:|
| `libiec61850-mms-server` | `mms-interop-libiec61850` | ✓ |
| `libiec61850-mms-client` | `mms-interop-libiec61850` | ✓ |
| `libiec61850-ied-server` | `mms-interop-libiec61850` | ✓ |
| `libiec61850-ied-client` | `mms-interop-libiec61850` | ✓ |
| `libiec61850-ied-controller` | `mms-interop-libiec61850` | ✓ |
| `libiec61850-ied-reporter` | `mms-interop-libiec61850` | ✓ |
| `iec61850bean-ied-server` | `mms-interop-iec61850bean` | ✓ |
| `iec61850bean-ied-client` | `mms-interop-iec61850bean` | ✓ |
| `iec61850bean-ied-controller` | `mms-interop-iec61850bean` | ✓ |
| `iec61850bean-ied-reporter` | `mms-interop-iec61850bean` | ✓ |

**Known limitations (verified upstream gaps):**

| Stack | Version | Direction | Capability | Expected skip |
|-------|---------|-----------|------------|---------------|
| `libiec61850` | 1.6.2.1-89-g6e265c54 | libiec61850-server → go-client | InformationReport, listOfVariable form | `TestClient_InformationReport` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | go-client → libiec61850-server | FileOpen initialPosition | `TestClient_FileOpen_InitialPosition` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | go-client → libiec61850-server | DeleteNamedVariableList scopeOfDelete domain/vmd (DeleteAll*NVLs) | `TestClient_DeleteAllNVLs_UnsupportedByPeer` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | go-client → libiec61850-server | ObtainFile | `TestClient_ObtainFile` |
| `libiec61850` | 1.6.2.1-89-g6e265c54 | libiec61850-client → go-server | FileOpen response without lastModified | `TestServer_FileOpen_NoLastModified` |
| `iec61850bean` | 1.x | go-client → iec61850bean-server | SBOw SelectWithValue | `TestBeanClient_Control_SBOwOperate` |

**Notes:**

- All adapter commands read from the fixture files in `fixtures/`.
- All commands emit JSON Lines to stdout; diagnostics go to stderr.
- Each adapter command supports `--capabilities` (emit JSON and exit) and `--version` (emit version JSON and exit).
- Fixed-sequence IED clients exit non-zero when any operation emits `ok:false` (connection failures remain exit 2).
- A skipped test without a registered limitation in this table must fail CI.
