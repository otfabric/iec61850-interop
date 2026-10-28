# mms-interop Releases

## How a release is made

1. Write the notes of the version below, as the first `## vX.Y.Z` section. `make validate-fixtures` (and CI) fails when the newest section is missing, empty or not closed by `---`.
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
