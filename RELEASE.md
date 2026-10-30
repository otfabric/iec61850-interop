# iec61850-interop Releases

> **Renamed from `mms-interop`.** Up to v0.4.1 this repository was
> `otfabric/mms-interop` and published `ghcr.io/otfabric/mms-interop-<stack>`.
> The entries below v1.0.0 keep the image names they were published under:
> those images are still pullable by tag and digest, but get no new versions.
> From v1.0.0 the images are `ghcr.io/otfabric/iec61850-interop-<stack>`.

## How a release is made

1. Set `interop_version` in versions.yaml and write the notes of the version below, as the first `## vX.Y.Z` section. `make validate-fixtures` (and CI) fails when the newest section is missing, empty or not closed by `---`.
2. `make ci` on the commit to release.
3. Publish that commit, either by tagging `vX.Y.Z` and pushing the tag, or by running **Actions → Release → Run workflow** on the branch with bump `patch` (default), `minor` or `major`. A manual run takes the highest existing `vX.Y.Z` tag, increments the chosen component, and tags the selected commit with the result (so the notes for that next version must already be in the tree). The workflow validates the fixtures and the notes of the tag before anything is built, then builds each adapter image on native `ubuntu-24.04` (linux/amd64) and `ubuntu-24.04-arm` (linux/arm64) runners in parallel — no QEMU — merges the two digests into one multi-arch manifest, pushes `<image>:vX.Y.Z`, `:X.Y.Z`, `:X.Y`, `:X` and `:latest` to GHCR, and creates the GitHub release with `manifest.json` (upstream pins and image digests) and `checksums.txt`.
4. Consumers pin the digests from the release or from `manifest.json`.

The notes are the body of the GitHub release, followed by the image digests
and the pinning snippet: everything after the heading `## vX.Y.Z` up to the
next line that is exactly `---` (`scripts/release-notes.sh vX.Y.Z` prints it).
This is the format of every otfabric repository and the rule of the shared
release workflow, so: one section per version, newest first, each ending with
a `---` line, and no `---` line inside a section.

Tags are immutable. A fix is a new version.

---

## v1.0.1

**Date:** 2026-10-28

## Summary

Provenance fix. The release's `manifest.json` reported the adapter's own
Maven version (`1.0.0`) in its `iec61850bean` field; it now reports the
iec61850bean library version the image is built with (`1.9.0`). Images,
commands, events and fixtures are unchanged from v1.0.0.

## Changes

### Fixed

- `manifest.json` `iec61850bean` is read from the `com.beanit:iec61850bean` dependency of `adapters/iec61850bean/pom.xml`, not from the first `<version>` in the file.
- `make ci` prints the same version in its provenance block (it printed the POM `modelVersion`, `4.0.0`).

---

## v1.0.0

**Date:** 2026-10-28

## Summary

The repository is renamed from `mms-interop` to **iec61850-interop**, since it
covers MMS, IEC 61850 IED services, GOOSE and Sampled Values, and the
adapter contract becomes stable. The adapters, commands, fixtures and event
shapes are those of v0.4.1; what changes is where they are published and the
promise that comes with them.

## Breaking changes

- Images move to new names. Pull `ghcr.io/otfabric/iec61850-interop-<stack>`
  instead of `ghcr.io/otfabric/mms-interop-<stack>`:
  - `ghcr.io/otfabric/iec61850-interop-libiec61850`
  - `ghcr.io/otfabric/iec61850-interop-iec61850bean`
  - `ghcr.io/otfabric/iec61850-interop-dscsystems` (GPL-3.0)
  - `ghcr.io/otfabric/iec61850-interop-iec61850-rust`
- Local builds are tagged `iec61850-interop-<stack>:local`; the Compose
  project and the GOOSE/SV bridge network are `iec61850-interop` and
  `iec61850-interop-goose`.
- The Go module paths are `github.com/otfabric/iec61850-interop` and
  `github.com/otfabric/iec61850-interop/adapters/dscsystems`; the Rust crate is
  `iec61850-interop-iec61850-rust`.
- A fixture without `identity.model` now identifies as `iec61850-interop`
  (was `mms-interop`). `fixtures/mms/interop.json` sets the model, so the
  published images answer as before.

Unchanged, so consumers only swap the image references: the command names
(`libiec61850-ied-server`, ...), their flags and output, the consumer
environment variables (`LIBIEC61850_IMAGE`, `IEC61850BEAN_IMAGE`,
`DSCSYSTEMS_IMAGE`, `IEC61850RUST_IMAGE`), and the fixture revisions
`mms-v2`, `iec61850-v3`, `goose-v1`, `sv-v1`.

## Compatibility promise

From v1.0.0 the repository follows semantic versioning. The image names,
command names and flags, event and operation shapes, exit codes, fixture
revisions, and the schemas of `capabilities.json` and `manifest.json` change
incompatibly only in a new major version. Minor versions add (commands,
optional fields, event types, fixture revisions next to the old ones); patch
versions fix adapters to match the contract and update known limitations.
Consumers ignore fields and events they do not know. The full statement is
in `docs/contract.md`.

## Changes

### Added

- `docs/contract.md`: the shared adapter contract (ready, version and
  capabilities events, fixed-sequence client output, exit codes) and the
  stability promise; `docs/goose.md` and `docs/sv.md` refer to it.
- A stack × capability matrix in `README.md` and `COVERAGE.md`, generated
  from `fixtures/capabilities.json` by `scripts/gen-coverage.sh`;
  `--check` mode runs in CI and in `make validate-fixtures`.
- `manifest.json` gains `repository` and `svFixture`.
- `org.opencontainers.image.source` and `description` labels on the
  libiec61850 and iec61850bean images (the other two had them), pointing at
  `https://github.com/otfabric/iec61850-interop`.

### Changed

- README rewritten around the four stacks and four profiles: capability
  matrix, image names and digest pinning, quick starts for MMS, IED, GOOSE
  and SV, L2 networking on Linux and macOS, licensing per image, consumer
  repositories.
- `capabilities.json` `adapterVersion` is `1.0.0`.

---

## v0.4.1

**Date:** 2026-10-28

## Summary

CDC values. The libiec61850 and iec61850bean IED servers now serve
`GGIO1.DPS1.stVal` = 2 and `MMTR1.TotVAh.actVal` = 42 from
`fixtures/iec61850/values.json`, as the dscsystems and iec61850-rust
servers already did, and every fixed-sequence IED client reads both. The
fixture is unchanged (`iec61850-v3`).

## Changes

### Added

- `libiec61850-ied-client`, `iec61850bean-ied-client`, `dscsystems-ied-client` and `iec61850-rust-ied-client` read `GGIO1.DPS1.stVal[ST]` (Dbpos, reported as its enumeration 0..3, 2 = on) and `MMTR1.TotVAh.actVal[ST]` (INT32U) after `MMXU1.TotW.mag.f[MX]`, with the existing read event shape: `{"operation":"read","target":"InteropIEDInteropLD/GGIO1.DPS1.stVal[ST]","ok":true,"value":2}`.
- Smoke/CI: every IED client run checks both values (libiec61850 and iec61850bean against themselves and each other; dscsystems and iec61850-rust against libiec61850, both directions).

### Changed

- `libiec61850-ied-server` served `DPS1.stVal` 0 and `TotVAh.actVal` 0; `iec61850bean-ied-server` served 1 and 0. Both now seed them from `values.json` (2 and 42).

## Notes

- `capabilities.json` `adapterVersion` is `0.4.1`; fixture revisions are unchanged.

---

## v0.4.0

**Date:** 2026-10-28

## Summary

Sampled Values. libiec61850, dscsystems and iec61850-rust gain an SV
publisher and subscriber with one contract (`docs/sv.md`), one fixture
(`fixtures/sv/`, revision `sv-v1`) and a pairwise cross-check that reports
loss. The IED fixture (`iec61850-v3`) gains the SV control block
`MSVCB02`, which the libiec61850 and dscsystems IED servers expose over
MMS. iec61850bean has no SV support.

## Changes

### Added

- `libiec61850-sv-publisher`/`-sv-subscriber`, `dscsystems-sv-publisher`/`-sv-subscriber`, `iec61850-rust-sv-publisher`/`-sv-subscriber`. The publisher paces samples against absolute deadlines (9-2LE: 4000 frames/s), wraps smpCnt at the samples per second, takes `{"cmd":"set"}` / `{"cmd":"stop"}` on stdin, and emits throttled `published` events and a final `stopped` with the totals. The subscriber emits every Nth ASDU (`--sample-every`) as an `sv` event decoded with the fixture's member list, and once per second `stats` (received, lost, duplicates, outOfOrder from smpCnt). Profiles: 9-2LE (8 × INT32 + quality, no optional fields) and generic (dataset members in order with every optional field). `--simulation` sets the Edition 2 S bit.
- Fixture revision `sv-v1` (`fixtures/sv/`): `interop-sv.cid` (`interop.icd` plus the 9-2LE merging-unit model `TCTR1..4`/`TVTR1..4`, dataset `PhsMeas1`, `SampledValueControl` `MSVCB01` (9-2LE, 80 samples/period at 50 Hz, 1 ASDU) and `MSVCB02` (generic, 1000 samples/s, 4 ASDUs) with their `SMV` addresses) and `sv.json`. `scripts/validate-fixtures.py` checks both against each other and that the CID contains `interop.icd` unchanged; the fixture workflow runs genmodel on the CID.
- `scripts/sv-crosscheck.sh`: SV publisher → subscriber on a Docker bridge, with a `set`, checking values, header fields and loss (sent vs received, at most 1 %). CI runs it for all nine directions of the three stacks and both streams, plus host-side runs.
- `fixtures/capabilities.json`: the SV commands, `svFixtureRevision`, `svcbOverMms` (libiec61850 and dscsystems yes; iec61850-rust and iec61850bean no), and SV known limitations.
- `make sv-crosscheck`.

### Changed

- IED fixture revision `iec61850-v3`: `interop.icd` adds the dataset `dsSvGeneric`, the `SampledValueControl` `MSVCB02`, `SMVsc` and a Communication section with its `SMV` address (libiec61850's genmodel requires one). Every adapter reports `iec61850-v3`; existing logical nodes, data and report control blocks are unchanged.
- The GOOSE and SV cross-checks time the publisher's `set` and `stop` from its `ready` event, not from `docker run`, so a slow container start no longer shortens the first state.

### Notes

- smpMod width differs: libiec61850's and dscsystems' publishers send two octets, iec61850-rust's one. libiec61850's subscriber accepts only one and drops the ASDU otherwise, so the generic stream from libiec61850 or dscsystems does not reach `libiec61850-sv-subscriber`; CI checks that these two pairs still fail.
- MSVCB `OptFlds` reads as 5 bits from libiec61850 and 10 bits from dscsystems.
- The SV publishers stay on one CPU (a veth can reorder frames of a thread that moves between CPUs); the libiec61850 and dscsystems subscribers enlarge their socket receive buffer (capped by `net.core.rmem_max`); iec61850-rust's HAL does not allow it.
- iec61850-rust handles at most 10 ASDUs per frame.

---

## v0.3.0

**Date:** 2026-10-28

## Summary

Two more interop stacks and GOOSE. `mms-interop-dscsystems` runs
dscsystems/go-iec61850 (pure Go, GPL-3.0) and `mms-interop-iec61850-rust`
runs csp0924/iec61850-rust (Rust, MIT OR Apache-2.0) behind the same
commands and JSON Lines as the libiec61850 adapter. libiec61850,
dscsystems and iec61850-rust all gain a GOOSE publisher and subscriber
with one shared contract, fixture and Docker L2 recipe.

## Changes

### Added

- Image `ghcr.io/otfabric/mms-interop-dscsystems` (`adapters/dscsystems/`, GPL-3.0 with `LICENSE` and `NOTICE` in that directory), pinned to dscsystems/go-iec61850 commit `6bf3f37` (`v0.6.5-1-g6bf3f37`): `dscsystems-mms-server`, `-mms-client`, `-ied-server`, `-ied-client`, `-ied-controller`, `-ied-reporter`, `-goose-publisher`, `-goose-subscriber`. Flags, fixture defaults, ready events and output lines mirror the libiec61850 commands, so a harness switches stack by image and command prefix.
- Image `ghcr.io/otfabric/mms-interop-iec61850-rust` (`adapters/iec61850-rust/`, with upstream's `LICENSE-MIT`, `LICENSE-APACHE` and a `NOTICE`), pinned to csp0924/iec61850-rust commit `d4e8952` (`v0.2.0`) and built with Rust 1.95.0: the same eight commands under the `iec61850-rust-` prefix. Upstream has no generic MMS server, so the adapter runs the association with upstream's ISO layers and encodes the remaining services itself. No command is unsupported; one that were would answer `{"event":"error","code":"unsupported",...}` and exit 4.
- `libiec61850-goose-publisher` and `libiec61850-goose-subscriber` (raw GoosePublisher / GooseReceiver API).
- GOOSE contract (`docs/goose.md`): the publisher announces `--values` as stNum 1, retransmits MinTime → doubling → MaxTime, takes `{"cmd":"set"}` / `{"cmd":"stop"}` on stdin and emits `published` per frame; the subscriber emits one `goose` event per frame (gocbRef, datSet, goID, appID, dst, src, stNum, sqNum, timeAllowedToLive, confRev, test, simulation, ndsCom, numDatSetEntries, t, values) and `supervision` timeout/ok per gocbRef. `test` is goosePdu field [7], `simulation` the Edition 2 S bit of Reserved1.
- Fixture revision `goose-v1` (`fixtures/goose/`): `interop-goose.cid` (interop.icd plus datasets `dsGooseAll` — boolean, integer, unsigned, float32, quality, utc-time, Dbpos, nested MV structure — and `dsGooseBool`, `GSEControl` `gcbAll`/`gcbBool` and their `GSE` addresses) and `goose.json` (on-wire references, members, initial and update values). `scripts/validate-fixtures.py` checks both against each other and against `interop.icd`; the fixture workflow runs genmodel on the CID.
- `scripts/goose-crosscheck.sh`: publisher → subscriber check between two images on a Docker bridge, optionally from the host side of the bridge. CI runs it between every pair of libiec61850, dscsystems and iec61850-rust (all nine directions), with and without the flags, and from the host side.
- `docker compose --profile goose` (bridge `goose0`), `make build-dscsystems`, `make build-iec61850-rust`, `make goose-crosscheck`.
- `fixtures/capabilities.json`: the new commands, `gooseFixtureRevision`, the dscsystems upstream, and seven dscsystems known limitations (alternate access ranges in the server; alternate access, NVL writes, association-scope NVLs and FileOpen lastModified in the client; Enum type description and ST writes in the IED server), the iec61850-rust upstream, and five iec61850-rust known limitations (RptEna without Resv and ST writes in the IED server, abort in the MMS client, ObtainFile role reversal and domain/VMD NVL deletion in the MMS server).

### Changed

- CI runs on pull requests, builds and smoke-tests all four images (including MMS and IED pairs of dscsystems and iec61850-rust against libiec61850) and vets the Go adapter; the release workflow builds and publishes the dscsystems and iec61850-rust images for linux/amd64 and linux/arm64 and records their digests and upstream commits in `manifest.json`.
- The libiec61850 image ships `fixtures/goose/`; its GOOSE binaries carry `cap_net_raw+ep`, so they run as the non-root adapter user in a container with `NET_RAW`.

### Notes

- The dscsystems MMS server sends both InformationReport forms of the fixture, listOfVariable first; the libiec61850 server sends only the named-list form. It seeks FileOpen `initialPosition`, which libiec61850 does not.
- The dscsystems IED server returns directory names in model order, libiec61850 sorted.
- The iec61850-rust MMS server keeps its file store in memory (`--filestore` is accepted and ignored), lists directories sorted by path, seeks FileOpen `initialPosition` and, like dscsystems, sends both InformationReport forms.
- A Go test on macOS cannot reach the GOOSE adapters natively: run it in a container on the bridge network (`docs/goose.md`).

---

## v0.2.0

**Date:** 2026-10-28

## Summary

The libiec61850 MMS adapters cover every MMS service a Go client or server
exposes, not only Identify, GetNameList, Read, Write and named variable
lists. FileDirectory, journals, reports, Status, alternate access and
abort were not exercised before; go-mms issue #2 (FileDirectory against
conforming servers) went unnoticed because of that.

## Changes

### Added

- Fixture revision `mms-v2` (`fixtures/mms/interop.json`): VMD `status`, `informationReports` (listOfVariable and VMD named-list form), domain `journals` with entry IDs, occurrence times and reason codes, a file store (`files`, with text, hex or generated content, including a subdirectory and a file larger than one PDU), named structure components, and the writable `array-rw`, `structure-rw` and `report-trigger` variables.
- `libiec61850-mms-server` serves Status from the fixture, the file store (FileDirectory, FileOpen/Read/Close, FileDelete, FileRename, ObtainFile), ReadJournal, writes with alternate access, and sends the fixture reports plus an UnsolicitedStatus when `interop/report-trigger` is written.
- `libiec61850-mms-client` runs a service sequence after the existing phases (`mms_client_services.c`): Status, GetNameList for journals, VMD variables and an unknown domain, GetVariableAccessAttributes of composites, array element and component read/write, named variable lists in domain, VMD and association scope (including a server-registered list), FileDirectory, FileRead with initial position and multi-chunk reads, FileDelete, FileRename, ObtainFile, ReadJournal by time range and start-after, InformationReport reception, client abort and server-initiated abort. Each step emits one JSON line (`op`, `target`, `ok`, `value`/`names`/`error`).
- Fixture file `NOTIME.TXT` without `lastModified` (the field is optional in the fixture as in MMS), and client steps that list `COMTRADE\` (the per-level name of a subdirectory) and read `NOTIME.TXT`.
- `scripts/validate-fixtures.py` checks the `mms-v2` sections: revision, status range, report variables, journal entry IDs, order, tags and reason codes, the file content forms and the `lastModified` format.
- `fixtures/capabilities.json` gains `mmsFixtureRevision` and five libiec61850 known limitations: listOfVariable InformationReports (wrong length encoding), FileOpen `initialPosition` (not seeked), bulk DeleteNamedVariableList, ObtainFile (the server fetches the file from the client), and an uninitialised `lastModified` in the client when a FileOpen response omits it.

### Changed

- The libiec61850 build enables `MMS_RENAME_FILE_SERVICE`, which the default profile compiles out.
- `mms_server.c` defines `_XOPEN_SOURCE 500` so `nftw`/`FTW_DEPTH` compile against glibc (Debian in the adapter image).
- `make build-libiec61850` forwards `ADAPTER_VERSION` into the image (CI and local builds can set `ADAPTER_VERSION=v0.2.0` so the ready event is not `dev`).
- The Dockerfile pins libiec61850 at upstream HEAD `6e265c54af66` (`v1.6.2.1-89-g6e265c54`), up from v1.6.1. `fixtures/capabilities.json` records that version. FileOpen `initialPosition` is now rejected when it is past EOF (LIB61850-557) but the server still does not seek, so that known limitation remains.
- COVERAGE.md lists both fixture revisions.

---

## v0.1.4

**Date:** 2026-07-26

## Summary

A writable integer setting in the IEC 61850 fixture, for write tests that
need a target the servers really accept.

## Changes

### Added

- `InteropLD/GGIO1.SetInt1` (CDC ING) with a writable `setVal[SP]`, initial value 0, in `interop.icd` and `values.json`; both IED servers serve it.

### Changed

- The README names `GGIO1.SetInt1.setVal[SP]` as the write target and says not to treat `Mod.ctlModel` or `Mod.d` as writes that succeed.
- `fixtures/capabilities.json` says adapter version 0.1.4.

---

## v0.1.3

**Date:** 2026-07-26

## Summary

The fixed-sequence IED clients are declared again and report failures in
their exit code; COVERAGE.md is generated.

## Changes

### Added

- `scripts/gen-coverage.sh` renders COVERAGE.md from `fixtures/capabilities.json`.
- The smoke test checks that the IED client sequence associates, concludes and succeeds.

### Changed

- `libiec61850-ied-client` and `iec61850bean-ied-client` are listed in `fixtures/capabilities.json` and in the `--capabilities` output.
- The `*-ied-client` adapters emit an `associate` operation, still attempt `conclude` after a failed operation, and then exit 3 when any operation emitted `ok:false`. A connection failure stays exit 2.
- The libiec61850 IED client finds the interop logical device with or without the IED name prefix.
- The libiec61850 IED server allows writes to FC ST, so `Mod.stVal` can be written.
- `make verify-binaries` checks the controller and reporter binaries.
- The README points to `fixtures/capabilities.json` as the authoritative command list.
- The generated `static_model.c` and `static_model.h` are no longer committed.

---

## v0.1.2

**Date:** 2026-07-20

## Summary

Capability and version metadata for every adapter, and fixture revision
`iec61850-v2`.

## Changes

### Added

- `--capabilities` and `--version` on every adapter command: JSON on stdout, exit 0.
- `fixtures/capabilities.json`: the commands of each image, the known upstream limitations and the upstream versions. COVERAGE.md is generated from it.
- Fixture `iec61850-v2`: `GGIO1.DPS1` (double-point status), `MMTR1.TotVAh` (binary counter reading) and the buffered report control block `LLN0.brcb01`.

### Known limitations

- The iec61850bean server does not expose `SBOw[CO]` as a writable MMS attribute, so select-with-value against it is not possible.

---

## v0.1.1

**Date:** 2026-07-20

## Summary

Control and reporting clients in both images.

## Changes

### Added

- `libiec61850-ied-controller` and `iec61850bean-ied-controller`: control clients for direct, SBO and SBOw operation.
- `iec61850bean-ied-reporter`: URCB reporter client.
- `GGIO1.SPCSO2` (SBO with normal security, ctlModel 2) and `GGIO1.SPCSO3` (ctlModel 4) in the IEC 61850 fixture, both writable.

### Changed

- The iec61850bean IED server reflects an operated `ctlVal` into the `stVal` of the control object, and its `ready` event names the IED.
- The libiec61850 reporter accepts dataset values that arrive as an MMS array.
- GitHub Actions updated to their current major versions.

---

## v0.1.0

**Date:** 2026-07-19

## Summary

First release: two adapter images with one shared deterministic fixture.

## Changes

### Added

- `ghcr.io/otfabric/mms-interop-libiec61850`: `libiec61850-mms-server`, `libiec61850-mms-client`, `libiec61850-ied-server`, `libiec61850-ied-client` and `libiec61850-ied-reporter`, built from a pinned libiec61850.
- `ghcr.io/otfabric/mms-interop-iec61850bean`: `iec61850bean-ied-server` and `iec61850bean-ied-client`.
- Fixtures `fixtures/mms/interop.json`, `fixtures/iec61850/interop.icd` and `fixtures/iec61850/values.json`, baked into the images.
- Every command emits JSON Lines on stdout and diagnostics on stderr.
- Fixture validation, smoke tests, and a release workflow that publishes multi-arch images with `manifest.json` and `checksums.txt`.

---
