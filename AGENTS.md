# AGENTS.md — iec61850-interop

## What this is

Independent IEC 61850 stacks (libiec61850, iec61850bean, dscsystems/go-iec61850, iec61850-rust, cB-Sigma iec61850 with iec61850-sim) as containers behind one adapter contract, covering MMS, IED services, GOOSE and Sampled Values, used as oracles by go-mms, go-iec61850 and iec61850ctl. Renamed from `mms-interop`. Profile: **interoperability**.

## Organisation standards

This repository follows **otfabric/engineering policy 1.5**, profile **interoperability** (`.otfabric/policy.yaml`, mode `enforce`). Where this file and the organisation standards disagree, this file wins here — say so in your report.

## Where the truth lives

| Question | Source |
|---|---|
| Adapter contract | docs/CONTAINER_CONTRACT.md, docs/goose.md, docs/sv.md, docs/routable.md |
| Which command exists in which image | fixtures/capabilities.json, rendered in docs/CAPABILITIES.md |
| Pins | versions.yaml, checked by `make validate-fixtures` |
| Fixtures | fixtures/ |
| What this repository must and must not do | REQUIREMENTS.md |
| What is done and what is next | PLAN.md |
| OT Fabric's own stack (not a reference) | own/go-iec61850/README.md, ADR-0012 |

## Hard rules

1. Nothing floats: upstreams by commit or exact version, base images by digest.
2. Capabilities live in fixtures/capabilities.json, not in prose; regenerate with `scripts/gen-coverage.sh`.
3. Nothing from `adapters/dscsystems/` (GPL-3.0) may be copied outside that directory.
4. The contract follows semantic versioning from v1.0.0 (docs/CONTAINER_CONTRACT.md).
5. Licences: every redistributed component in THIRD_PARTY_NOTICES.md.
6. `interop_version` in versions.yaml equals the newest section of RELEASE.md and the tag.
7. REQUIREMENTS.md R1: only `own/` and its workflow know a library under test; the scripts take its image through `ADAPTER_IMAGE_otfabric`.

## Commands

| When you change… | Run |
|---|---|
| anything | `make validate-fixtures lint` |
| an adapter | `make build-<stack>`, `make smoke ADAPTERS=<stack>`, `make interop` |
| Go in adapters/dscsystems | `make go-test go-lint go-vuln` |
| GOOSE or SV | `make interop` (or `make goose-crosscheck sv-crosscheck`) |
| own/go-iec61850 | `make -f own/go-iec61850/Makefile image smoke interop` |
| everything | `make ci` |

## Git and releases

Branches and PRs. Do not push to `main`, push tags or publish images unless the maintainer asks in the session.
