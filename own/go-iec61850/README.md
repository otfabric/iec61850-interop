# iec61850-interop-otfabric

[go-iec61850](https://github.com/otfabric/go-iec61850), OT Fabric's own IEC 61850
stack, and [go-mms](https://github.com/otfabric/go-mms), the MMS stack it is built
on, behind the [adapter contract](../../docs/CONTAINER_CONTRACT.md) (and its
[GOOSE](../../docs/goose.md) and [Sampled Values](../../docs/sv.md) parts), so
that other stacks can test against them. Anything that uses an
`iec61850-interop-<stack>` image can use this one in its place, with the command
prefix `otfabric-`. Unlike those, it is not a reference: its tags are go-iec61850
versions, and no harness release or `manifest.json` contains it.

**For testing only.** It runs unauthenticated MMS and IEC 61850 servers. Do not
expose it to a network you do not control. Old images stay available, so check
the [go-iec61850](https://github.com/otfabric/go-iec61850/security/advisories)
and [go-mms](https://github.com/otfabric/go-mms/security/advisories) advisories
for the version you pull.

**Not an independent implementation.** `--capabilities` and `--version` say
`upstream.independentEngine: false`, and `results.json` records
`independent: false`. A result against it says nothing independent about
go-iec61850 or go-mms when OT Fabric produces it. A result you publish, naming
the implementation and version, is independent evidence, and we would like to
hear about it.

## Use

```sh
docker run -d --name go-iec61850 -p 127.0.0.1:1102:1102 \
  --entrypoint otfabric-ied-server ghcr.io/otfabric/iec61850-interop-otfabric:<tag>
docker logs -f go-iec61850    # {"event":"ready",...,"adapter":"otfabric",...}
```

`<tag>` is a go-iec61850 version such as `v1.6.0`, or `latest`, the newest one
published. A rebuild of a version replaces its tag, so pin the digest if you
need an exact build. The digest is in the summary of the
[workflow run](https://github.com/otfabric/iec61850-interop/actions/workflows/own-go-iec61850.yml),
and `docker run --rm --entrypoint otfabric-ied-server <image> --version` prints
the versions (go-iec61850, its commit, and the go-mms in the image).

| | |
|---|---|
| Upstream | go-iec61850 and go-mms, the latest releases when the image was built |
| Licence | MIT; every module's licence is in the image under `/licenses` |
| Adapter name | `otfabric` (command prefix `otfabric-`) |
| Commands | `otfabric-mms-server`, `-mms-client` (go-mms); `-ied-server`, `-ied-client`, `-ied-controller`, `-ied-reporter`, `-goose-publisher`, `-goose-subscriber`, `-sv-publisher`, `-sv-subscriber` (go-iec61850) |
| Language | Go |
| Protocol engine | OT Fabric's own |

The GOOSE and SV commands need `NET_RAW` and a shared Docker bridge, as the
reference images do ([docs/goose.md](../../docs/goose.md#l2-networking)).

## Why it is apart from the other adapters

`REQUIREMENTS.md` R1: nothing under `adapters/`, `fixtures/`, `scripts/` or the
top-level `Makefile` knows a library under test, and CI enforces that. This
directory and `.github/workflows/own-go-iec61850.yml` are the one exception. The
scripts take the image through `ADAPTER_IMAGE_otfabric`; it is not in
`fixtures/capabilities.json` or `ADAPTERS`.

No release loop can form (`otfabric/engineering` ADR-0012). This image is built
by hand, from the latest go-iec61850 and go-mms releases, and nothing is
committed: this repository has no pin of either to bump. No release or required
check of go-iec61850 or go-mms needs the image, and neither commits a pin of it.
It is not in a harness release or `manifest.json`.

One image carries both libraries: go-iec61850 is built on go-mms, the contract
gives each stack one image with one command prefix, and `mms-interop` (the
former name of this repository) gets no new images.

## Publishing

Run the **Own stack (go-iec61850)** workflow by hand, from `main`. It finds the
latest go-iec61850 and go-mms releases and the latest harness release, and
prints them. It builds the image, runs the smoke test and the cross-stack
self-test against the reference images on both architectures, then publishes
`:<go-iec61850 version>` and `:latest`. Run from a branch, it stops before
publishing. Nothing runs it automatically, so after a release of either library
the image stays on the old versions until someone does.

```sh
make -f own/go-iec61850/Makefile image smoke interop            # against the latest harness release
make build && make -f own/go-iec61850/Makefile smoke interop VERSION=    # against references built here
```

## What it cannot do

Only what the public APIs of go-iec61850 and go-mms offer; the adapter encodes
no protocol itself.

- **GOOSE `timeAllowedToLive`.** go-iec61850's publisher sends one fixed value
  (2 × MaxTime) in every frame, not twice the interval to the next frame as
  [docs/goose.md](../../docs/goose.md) describes. Subscribers see a valid,
  longer TAL.
- **GOOSE `--dataset ""` without `--nds-com`**: the library refuses to publish
  without a data set reference, so the publisher exits 1.
- **Duplicate and out-of-order GOOSE/SV frames**: the library's subscribers drop
  them, so no event is printed for them (SV still counts them in `stats`).
- **GOOSE float values**: go-mms has one float type; a value that fits a
  float32 exactly is sent and reported as `float32`. The fixtures use float32
  only.
- **MMS ObtainFile**: go-mms's file provider copies a file within the server's
  store; it does not fetch the source from the client (the role reversal of
  ISO 9506), so a source the server lacks gives a File-class error.
- **MMS server, `report-trigger`**: writing `-1` aborts the association, as
  go-mms's own fixture server does; the client's `server-abort` step uses it.
- **Conclude against iec61850bean**: its server never answers MMS Conclude
  (go-iec61850 INTEROP.md). The IED clients wait 5 s, then abort the
  association; the `conclude` line follows either way, as in the reference
  clients, and stderr says which.
