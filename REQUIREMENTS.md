# Requirements

Requirements for the `iec61850-interop` repository. These requirements constrain the design and implementation choices made in [PLAN.md](PLAN.md).

---

## 1. Purpose and scope

**REQ-1.1** The repository must validate `go-mms` and `go-iec61850` against independent, established third-party implementations.

**REQ-1.2** The repository must not become a general-purpose interoperability framework, conformance harness or compatibility-matrix product.

**REQ-1.3** All assertions must be expressed in Go test files using the actual `go-mms` and `go-iec61850` libraries as test drivers.

**REQ-1.4** The scope is limited to MMS over TCP (ISO 8802/ISO 9506 transport stack). GOOSE and Sampled Values are explicitly out of scope.

---

## 2. Library separation

**REQ-2.1** The `go-mms` library must be tested independently of `go-iec61850`. Tests at the MMS layer must not depend on IEC 61850 object semantics.

**REQ-2.2** The `go-iec61850` library must be tested at the IEC 61850 ACSI/data-model level. Its tests validate IEC 61850 mapping behaviour, not the underlying MMS encoding.

**REQ-2.3** `go-mms` must not accumulate IEC 61850 semantics as a result of this test suite or its adapter interfaces.

**REQ-2.4** Test ownership must follow the boundary:
  - *MMS layer*: Did `go-mms` correctly speak generic MMS over the full ISO stack?
  - *IEC 61850 layer*: Did `go-iec61850` correctly map IEC 61850 ACSI and data-model behaviour onto MMS?

---

## 3. Adapters

**REQ-3.1** Each third-party implementation must run as a separate, isolated Docker container. It must not be linked into the MIT-licensed Go libraries.

**REQ-3.2** Each adapter must expose four explicitly named commands or entry points — one per role and protocol layer. These commands may share a binary, library or package internally, but the MMS/IED and client/server roles must remain separately invocable and must not be selected through a generalised protocol test DSL or flag-driven dispatch:
  - `<adapter>-mms-server`
  - `<adapter>-mms-client`
  - `<adapter>-ied-server`
  - `<adapter>-ied-client`

**REQ-3.3 — Server adapter lifecycle.** Server adapters must:
  1. Load the applicable fixture.
  2. Open a listening socket.
  3. Emit exactly one JSON readiness event to stdout: `{"event":"ready","address":"<host>:<port>"}`.
  4. Serve requests until SIGTERM or SIGINT is received.
  5. Shut down cleanly.

**REQ-3.4 — Client adapter lifecycle.** Client adapters must:
  1. Load the applicable fixture.
  2. Connect to the server address supplied via environment variable.
  3. Execute a fixed, documented operation sequence.
  4. Emit one JSON Line to stdout per completed operation (see REQ-3.5).
  5. Exit with a meaningful exit code (see REQ-3.6).

**REQ-3.5 — Output contract.** Adapter output must use JSON Lines on stdout. Each line is one complete JSON object. Human-readable logs, library diagnostics and warnings must go to stderr. Stdout must contain only JSON Lines. This separation is mandatory because C and Java libraries commonly write to stdout by default.

Client result line format:

```jsonl
{"operation":"read","target":"interop/boolean","ok":true,"value":true}
{"operation":"write","target":"interop/float32","ok":true}
{"operation":"conclude","ok":true}
```

The result schema must not grow into a protocol-neutral test specification. Permitted fields: `operation`, `target`, `ok`, `value`, `error`, `diagnostics`.

**REQ-3.6 — Exit codes.** Adapters must exit with:

| Code | Condition |
|---|---|
| 0 | Sequence completed and every emitted operation has `ok:true` |
| 1 | Startup or configuration failure |
| 2 | Connection failure |
| 3 | Malformed or unreadable fixture, **or** one or more operations emitted `ok:false` |

Fixed-sequence clients (for example `*-ied-client`) must still attempt `conclude` after an operation failure, then exit `3` when any prior operation failed. Connection failure remains exit `2` and must not be reported as a successful conclude.

**REQ-3.7** Adapters must not contain assertions, test logic or adapter-specific fixture overrides.

**REQ-3.8 — Version pinning.** Every adapter dependency must be pinned to an immutable release tag and, where practical, verified by source archive checksum or digest. Floating against `master`, `latest` or an untagged branch is prohibited. The pinned versions must be recorded in the respective `Dockerfile` or a companion lockfile.

**REQ-3.9** The initial implementation must use libiec61850 as the sole adapter. iec61850bean may only be introduced after both libiec61850 MMS test directions pass (Steps 1–4 in PLAN.md).

**REQ-3.10** libiec61850 is GPLv3. Distribution of any image containing it must preserve the applicable GPLv3 notices and corresponding source obligations. libiec61850 must not be copied into this repository; it must be downloaded from a pinned release archive during the Docker build. The separation of containers does not by itself discharge GPLv3 obligations.

**REQ-3.11** iec61850bean is Apache-2.0. No additional licensing constraints apply beyond the Apache-2.0 terms.

---

## 4. Fixtures

**REQ-4.1** Fixtures must define the server model and initial state. They must not contain test expectations, assertions, scripted value transitions, callbacks, artificial latency or per-adapter overrides.

**REQ-4.2** The MMS fixture (`fixtures/mms/interop.json`) must be a declarative, static JSON document. It must be consumable by Go, Java and C adapters without transformation.

**REQ-4.3** The MMS fixture must cover the following initial type baseline and no others until a concrete test requires them:
  - `boolean`
  - `integer`
  - `unsigned`
  - `float32`
  - `visible-string`
  - `octet-string`
  - `bit-string`
  - `utc-time`
  - `array`
  - `structure`

**REQ-4.4** The IEC 61850 fixture must use SCL (IEC 61850-6) as its primary model format (`fixtures/iec61850/interop.icd`). A companion JSON file (`fixtures/iec61850/values.json`) supplies mutable runtime values and writable attribute references that SCL cannot express.

**REQ-4.5** The fixture and the test specification must be kept separate. Fixtures describe what the server exposes; tests describe what the client does and what it expects.

**REQ-4.6 — Fixture encoding norms.** The following encoding rules are normative for all adapters. Each adapter's fixture parser must document conformance to these rules.

  - **Octet strings** — case-insensitive hexadecimal without a `0x` prefix and with an even number of hex digits. Odd-length values are a fixture error (exit code 3).
  - **Bit strings** — MSB-first textual bit sequence. The meaningful bit count equals the string length exactly. Adapters must preserve the exact bit count on the wire and must not assert padding bits.
  - **UTC time** — ISO 8601 UTC string with a `Z` suffix. In Phase 1, quality fields (leapSecondsKnown, clockFailure, clockNotSynchronized, accuracy) are treated as zero/default. Tests must not assert specific quality bit values in Phase 1.
  - **Integer and unsigned** — fixture values must fit in an interoperable signed or unsigned range (no values requiring more than 32 bits in Phase 1). Tests must not assert a specific BER byte length when multiple canonical widths are valid for the given value.

**REQ-4.7 — Structure component ordering.** Structure components are ordered and anonymous. `GetVariableAccessAttributes` results must be validated in declaration order. Tests must not assume named structure components.

**REQ-4.8 — State reset.** A fresh server process or container must be started for each top-level test case or test group, reloading the fixture from disk each time. A fixture-reset endpoint must not be implemented. Tests must not rely on test ordering or cleanup writes to restore initial values.

---

## 5. Test structure

**REQ-5.1** Go test files are the primary test executables. The test files must import and exercise `go-mms` and `go-iec61850` directly.

**REQ-5.2** Each test file must correspond to one role: client or server:

```
tests/mms/go_mms_client_test.go
tests/mms/go_mms_server_test.go
tests/iec61850/go_iec61850_client_test.go
tests/iec61850/go_iec61850_server_test.go
```

**REQ-5.3** Test files must remain separate from fixture files. Test assertions must not be embedded in fixture JSON.

**REQ-5.4** Negative-case tests must assert the MMS error class or the broad outcome of a failed operation. They must not require byte-identical error responses unless the standard mandates a specific encoding.

**REQ-5.5 — CI version recording.** CI must record and retain the exact commit SHA of `go-mms`, `go-iec61850`, libiec61850 and iec61850bean used for each test run. This record must be accessible alongside test results.

**REQ-5.6 — Go module selection.** The default CI configuration must use pinned module commit SHAs, not published module versions. Local development may use a `go.work` file to substitute sibling directories for `go-mms` and `go-iec61850`. The `go.work` file must not be committed. Machine-specific `replace` directives must not appear in `go.mod`.

---

## 6. Observability and diagnostics

**REQ-6.1** On test failure (and optionally on every CI run), a packet capture must be retained as a debugging artifact:

```
artifacts/<adapter>-<direction>.pcap
```

**REQ-6.2** Packet captures must not be used as golden-file test oracles. They exist solely for post-failure diagnosis.

**REQ-6.3** The infrastructure must not require PCAP comparison to determine pass or fail.

---

## 7. Infrastructure

**REQ-7.1** The repository must provide a `docker-compose.yml` that can bring up adapter containers for manual debugging. Test automation does not require Compose to be running.

**REQ-7.2** The repository must provide a `Makefile` that orchestrates building adapters, running tests and collecting artifacts.

**REQ-7.3** No additional top-level directories (`schemas/`, `capabilities/`, `profiles/`, `matrix/`, `manifests/`, `overlays/`, `generated/`) may be created at the start. They may be introduced only if a concrete, recurring maintenance problem requires them.

**REQ-7.4** A JSON Schema for the MMS fixture may be added later if malformed fixtures become a demonstrated maintenance problem. It must not be created speculatively.

**REQ-7.5 — Readiness detection.** Go tests must wait for the adapter server's readiness event (a JSON Line on stdout containing `"event":"ready"`) before dialling. Fixed startup sleeps are prohibited.

**REQ-7.6 — Bounded timeouts.** Every external process launch, network dial, and per-operation call must carry an explicit timeout. No operation may block indefinitely.

**REQ-7.7 — Process and container cleanup.** Tests must clean up all adapter processes and containers after pass, failure, panic or timeout. A deferred cleanup function or `testing.Cleanup` must be registered before any adapter process is launched.

**REQ-7.8 — Port and address configuration.** Adapter containers must not bind host port 102 by default. The mapped host port must be configurable and must default to an unprivileged port (e.g. 1102). Adapter containers that communicate exclusively within the Docker Compose network may bind container port 102 internally. Go tests must obtain the server address from an environment variable or a test-time port allocation, not from a hardcoded address:

```
MMS_INTEROP_ADDRESS   default: 127.0.0.1
MMS_INTEROP_PORT      default: 1102
```

---

## 8. Delivery

**REQ-8.0 — Phase 0 gate.** The Phase 0 feasibility spike (proving that libiec61850's generic MMS server API is usable) must be completed and its findings documented before Step 1 of the delivery sequence begins. If the spike is unsuccessful, a fallback must be chosen and documented before proceeding.

**REQ-8.1** No later step may be considered delivered or merged into the main implementation sequence until all preceding acceptance criteria pass. Exploratory spikes, Docker build scaffolding, SCL authoring and other non-integrated groundwork may proceed in parallel with any phase.

**REQ-8.2** The first release is considered complete when: `go-mms` can associate, discover, read, write and disconnect successfully in both directions against libiec61850, using one shared deterministic fixture (Steps 1–4).

**REQ-8.3** Reports (buffered and unbuffered) and controls (direct and select-before-operate) must not be included in Phase 2A. They are deferred to Phase 2B.

**REQ-8.4** The following are permanently out of scope unless a separate decision explicitly reverses the exclusion:
  - GOOSE
  - Sampled Values
  - Broad vendor compatibility matrix
  - Metadata overlays or per-adapter fixture variants
  - Generalised test orchestration framework
  - Conformance certification claims
  - Triangle MicroWorks or other commercial validation tools (unless introduced explicitly as a later conformance stage)
