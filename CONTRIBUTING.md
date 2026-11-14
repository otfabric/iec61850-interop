# Contributing

Branches and pull requests; `main` is protected. Read [AGENTS.md](AGENTS.md)
for where the truth lives and the hard rules, and [REQUIREMENTS.md](REQUIREMENTS.md)
R1: nothing outside `own/` knows a library under test.

Before a pull request:

```bash
make validate-fixtures lint     # no Docker
make build smoke interop        # the images, their smoke tests and the cross-stack self-test
```

- **An adapter change**: `make build-<stack>`, then `make smoke ADAPTERS=<stack>`
  and `make interop`. A command the stack cannot provide answers the contract's
  unsupported event (exit 4), never a fake success.
- **A contract change** bumps `contract_version` in versions.yaml, reaches every
  adapter and [docs/CONTAINER_CONTRACT.md](docs/CONTAINER_CONTRACT.md) in the
  same change, and follows its semantic versioning.
- **A release** has its notes in [RELEASE.md](RELEASE.md) and `interop_version`
  in the same pull request.
- **OT Fabric's own stack** (`own/go-iec61850/`) is built and tested only by the
  manual **Own stack (go-iec61850)** workflow and
  `make -f own/go-iec61850/Makefile image smoke interop`; it is not in
  `ADAPTERS`, `fixtures/capabilities.json` or a release.

## When CI runs

| Event | Workflow | What runs |
|---|---|---|
| Pull request | `pr.yml` | `policy.yml` (otfabric-policy) always; `ci.yml`, `validate-fixtures.yml` unless every changed file is documentation; then `OT Fabric Required Checks` |
| Push to a branch | none | nothing; dispatch `ci.yml`, `validate-fixtures.yml` to test a branch without a pull request |
| Push to `main` | `policy.yml` | the light checks; the merged commit was tested as the pull request's merge commit |
| Schedule | `upstream.yml` | the scheduled runs, on `main` |
| Release | `release.yml` | the release workflow tests the release commit again |

Documentation means an added or modified `*.md` file outside `.github/`, `testdata/` and `vendor/`, other than `AGENTS.md` and `CLAUDE.md`. A deleted or renamed document, or any other file, runs everything. `OT Fabric Required Checks` fails unless every job that applies succeeded; its summary lists them.
