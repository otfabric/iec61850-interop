# Fixtures

The inputs every adapter serves or expects, by revision (a revision is part of
the contract: [CONTAINER_CONTRACT.md](CONTAINER_CONTRACT.md#stability-v1)).

| Revision | Files | Used by |
|---|---|---|
| `mms-v2` | `fixtures/mms/interop.json` — domains, variables of every MMS type, NVLs, file store, journals | `*-mms-server`, `*-mms-client` |
| `iec61850-v3` | `fixtures/iec61850/interop.icd` (SCL: `InteropIED`/`InteropLD`, GGIO1, MMXU1, MMTR1, datasets, URCB/BRCB, `MSVCB02`) and `values.json` (runtime values, writable attributes) | `*-ied-*` |
| `goose-v1` | `fixtures/goose/interop-goose.cid` (`interop.icd` + GOOSE datasets, `GSEControl`, `GSE`) and `goose.json` (control blocks, members, values) | `*-goose-*` |
| `sv-v1` | `fixtures/sv/interop-sv.cid` (`interop.icd` + 9-2LE merging unit, `SampledValueControl`, `SMV`) and `sv.json` (streams, members, values) | `*-sv-*` |

The images carry the fixtures under `/fixtures/`. Consumers keep
synchronised copies in their `testdata/`; an image version and the fixture
revisions it names are updated together. `scripts/validate-fixtures.py`
cross-checks the files (values against the ICD, the CIDs against
`interop.icd`, the JSON against the CIDs).

Writable scalar setting for consumer write tests (ING CDC):
`GGIO1.SetInt1.setVal[SP]` in the interop LD (`InteropIEDInteropLD/GGIO1.SetInt1.setVal[SP]`
on the reference servers; resolve the LD from the server directory, contract 1.3). Do not treat `Mod.ctlModel` / `Mod.d`
as assumed-success write targets.

The GOOSE and SV fixtures are described in detail in [goose.md](goose.md) and [sv.md](sv.md).
