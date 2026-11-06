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
