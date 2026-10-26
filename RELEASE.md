# mms-interop Releases

## How a release is made

1. Write the notes of the version below, as the first `## vX.Y.Z` section. `make validate-fixtures` (and CI) fails when the newest section is missing, empty or not closed by `---`.
2. `make ci` on the commit to release.
3. Tag `vX.Y.Z` and push the tag. The release workflow validates the fixtures and the notes of the tag before anything is built, builds both adapter images for linux/amd64 and linux/arm64 with `ADAPTER_VERSION` set to the tag, pushes them to GHCR as `<image>:X.Y.Z`, `X.Y` and `X`, and creates the GitHub release with `manifest.json` (upstream pins and image digests) and `checksums.txt`.
4. Consumers pin the digests from the release or from `manifest.json`.

The notes are the body of the GitHub release, followed by the image digests
and the pinning snippet: everything after the heading `## vX.Y.Z` up to the
next line that is exactly `---` (`scripts/release-notes.sh vX.Y.Z` prints it).
This is the format of every otfabric repository and the rule of the shared
release workflow, so: one section per version, newest first, each ending with
a `---` line, and no `---` line inside a section.

Tags are immutable. A fix is a new version.

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
