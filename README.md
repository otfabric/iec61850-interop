# iec61850-interop

> **Renamed from `mms-interop`.** The repository was `otfabric/mms-interop`
> until v0.4.1 and its images were `ghcr.io/otfabric/mms-interop-<stack>`.
> From v1.0.0 the images are `ghcr.io/otfabric/iec61850-interop-<stack>`.
> The old images stay published and pullable by their tags and digests (see
> [RELEASE.md](RELEASE.md)) but receive no new versions; GitHub redirects the
> old repository URL.

Interoperability infrastructure for IEC 61850 across all of its
communication profiles: **MMS** (client/server, ISO 9506), **IED services**
(the IEC 61850-8-1 ACSI mapping: browse, read/write, datasets, reports,
controls), **GOOSE** (IEC 61850-8-1, layer 2) and **Sampled Values**
(IEC 61850-9-2 / 9-2LE, layer 2).

The repository publishes one container image per independent IEC 61850 stack
(libiec61850, iec61850bean, dscsystems/go-iec61850, iec61850-rust), the
canonical fixtures every stack loads, and a stable JSON Lines contract that
every adapter command speaks. It does not own assertions about any library:
interop tests and compatibility matrices live in the consumer repositories.

```
                      iec61850-interop
        images + fixtures + JSON Lines contract (v1, semver)
           /                    |                     \
      go-mms               go-iec61850             iec61850ctl
   MMS interop tests   IED / GOOSE / SV tests     CLI e2e tests
```

## Stacks and capabilities

<!-- capability-matrix:begin -->
<!-- generated from fixtures/capabilities.json by scripts/gen-coverage.sh; do not edit -->

| Stack | Image | MMS server | MMS client | IED server | IED client | IED controller | IED reporter | GOOSE publisher | GOOSE subscriber | SV publisher | SV subscriber | SVCB over MMS |
|-------|-------|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| libiec61850 | `iec61850-interop-libiec61850` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| iec61850bean | `iec61850-interop-iec61850bean` | — | — | ✓ | ✓ | ✓ | ✓ | — | — | — | — | — |
| dscsystems | `iec61850-interop-dscsystems` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| iec61850-rust | `iec61850-interop-iec61850-rust` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |

<!-- capability-matrix:end -->

✓ = the command exists in the image. SVCB over MMS = the IED server exposes
the SV control block `LLN0$MS$MSVCB02` as MMS variables. The table is
generated from [`fixtures/capabilities.json`](fixtures/capabilities.json) by
`scripts/gen-coverage.sh` and checked in CI; that file, not this README, is
the authoritative capability claim, including the known limitations of each
stack. [COVERAGE.md](COVERAGE.md) lists every command and limitation.

| Stack | Upstream | Language | Pinned at |
|---|---|---|---|
| libiec61850 | [mz-automation/libiec61850](https://github.com/mz-automation/libiec61850) | C | `1.6.2.1-89-g6e265c54` |
| iec61850bean | [beanit/iec61850bean](https://github.com/beanit/iec61850bean) | Java | 1.9.0 |
| dscsystems | [dscsystems/go-iec61850](https://github.com/dscsystems/go-iec61850) | Go | `v0.6.5-1-g6bf3f37` |
| iec61850-rust | [csp0924/iec61850-rust](https://github.com/csp0924/iec61850-rust) | Rust | `v0.2.0` (`d4e8952`) |

The four share no protocol code with each other or with the otfabric Go
libraries, which is the point: a library that interoperates with all of them
has not just agreed with itself.

## Images

| Image | Commands |
|---|---|
| `ghcr.io/otfabric/iec61850-interop-libiec61850` | `libiec61850-*` |
| `ghcr.io/otfabric/iec61850-interop-iec61850bean` | `iec61850bean-*` |
| `ghcr.io/otfabric/iec61850-interop-dscsystems` | `dscsystems-*` (GPL-3.0, see [Licensing](#licensing)) |
| `ghcr.io/otfabric/iec61850-interop-iec61850-rust` | `iec61850-rust-*` |

Every image is multi-arch (`linux/amd64`, `linux/arm64`) and tagged
`vX.Y.Z`, `X.Y.Z`, `X.Y`, `X` and `latest`. Each command is a binary on the
`PATH`; select it with `--entrypoint`. The default entrypoint is the
stack's MMS server (the IED server for iec61850bean) on port 1102.

### Pinning by digest

Tags are for humans; CI pins the multi-arch digest. Every GitHub release
lists the four digests and ships a `manifest.json` with them, the upstream
pins and the fixture revisions:

```bash
gh release download v1.0.0 -R otfabric/iec61850-interop -p manifest.json
jq -r '.images' manifest.json

# or resolve a tag yourself
docker buildx imagetools inspect ghcr.io/otfabric/iec61850-interop-libiec61850:v1.0.0 \
  --format '{{ .Manifest.Digest }}'
```

Consumers read the image from an environment variable, one per stack:

```bash
LIBIEC61850_IMAGE=ghcr.io/otfabric/iec61850-interop-libiec61850@sha256:<digest>
IEC61850BEAN_IMAGE=ghcr.io/otfabric/iec61850-interop-iec61850bean@sha256:<digest>
DSCSYSTEMS_IMAGE=ghcr.io/otfabric/iec61850-interop-dscsystems@sha256:<digest>
IEC61850RUST_IMAGE=ghcr.io/otfabric/iec61850-interop-iec61850-rust@sha256:<digest>
```

The images are public; `docker pull` needs no login.

## Quick start

The examples use `v1.0.0` tags for brevity; pin digests in CI.

```bash
V=v1.0.0
R=ghcr.io/otfabric/iec61850-interop
docker network create interop
```

**MMS** — a libiec61850 MMS server and the dscsystems MMS client against it:

```bash
docker run -d --rm --name mms --network interop -p 1102:1102 $R-libiec61850:$V
docker logs mms        # {"event":"ready","address":"0.0.0.0:1102","fixture":"mms-v2",...}
docker run --rm --network interop --entrypoint dscsystems-mms-client \
  $R-dscsystems:$V --host mms --port 1102
docker stop mms
```

Port 1102 is published on the host as well, so an MMS client under test
(go-mms) can connect to `localhost:1102`.

**IED services** — an iec61850-rust IED server and the iec61850bean IED
client, controller and reporter against it:

```bash
docker run -d --rm --name ied --network interop -p 1102:1102 \
  --entrypoint iec61850-rust-ied-server $R-iec61850-rust:$V --port 1102
docker run --rm --network interop --entrypoint iec61850bean-ied-client \
  $R-iec61850bean:$V --host ied --port 1102
docker run --rm --network interop --entrypoint iec61850bean-ied-controller \
  $R-iec61850bean:$V --host ied --port 1102 --do SPCSO2 --ctlval 1
docker run --rm --network interop --entrypoint iec61850bean-ied-reporter \
  $R-iec61850bean:$V --host ied --port 1102
docker stop ied
```

**GOOSE** — layer 2, so both ends need `NET_RAW` on one bridge:

```bash
docker network create -o com.docker.network.bridge.name=goose0 iec61850-interop-goose
docker run -d --rm --name goose-sub --network iec61850-interop-goose --cap-add NET_RAW \
  --entrypoint iec61850-rust-goose-subscriber $R-iec61850-rust:$V --iface eth0 --app-id 0x1002
docker run -i --rm --network iec61850-interop-goose --cap-add NET_RAW \
  --entrypoint libiec61850-goose-publisher $R-libiec61850:$V \
  --iface eth0 --gocb-ref 'InteropIEDInteropLD/LLN0$GO$gcbBool' --go-id InteropGooseBool \
  --dataset 'InteropIEDInteropLD/LLN0$dsGooseBool' --app-id 0x1002 --dst 01:0c:cd:01:00:02 \
  --values '[{"type":"boolean","value":false},{"type":"bit-string","value":"0000000000000"}]'
# type {"cmd":"set","values":[{"type":"boolean","value":true},{"type":"bit-string","value":"0000000000000"}]}
# then {"cmd":"stop"}; docker logs goose-sub shows both states
```

Or check a whole publisher/subscriber pair against the fixture:

```bash
scripts/goose-crosscheck.sh $R-libiec61850:$V libiec61850-goose-publisher \
                            $R-dscsystems:$V  dscsystems-goose-subscriber
```

**Sampled Values** — the 9-2LE stream from one stack into another, with the
values, header fields and loss checked against `fixtures/sv/sv.json`:

```bash
scripts/sv-crosscheck.sh $R-dscsystems:$V   dscsystems-sv-publisher \
                         $R-libiec61850:$V  libiec61850-sv-subscriber        # MSVCB01, 9-2LE
scripts/sv-crosscheck.sh $R-iec61850-rust:$V iec61850-rust-sv-publisher \
                         $R-dscsystems:$V    dscsystems-sv-subscriber MSVCB02 # generic stream
```

[docs/sv.md](docs/sv.md) has the flags to run a publisher or subscriber
directly.

## Layer-2 networking (GOOSE, SV)

GOOSE and SV are raw Ethernet (EtherType `0x88B8` / `0x88BA`), not IP. The
adapters open an `AF_PACKET` socket on `eth0`, so:

- the container needs `NET_RAW` (in Docker's default set; `--cap-add NET_RAW`
  makes it explicit, and is required with `cap_drop: ALL`); the binaries
  carry `cap_net_raw+ep` and run as the non-root image user, except under
  `--security-opt no-new-privileges`, where you add `--user 0`;
- publisher and subscriber share one user-defined bridge network. Name the
  bridge device (`-o com.docker.network.bridge.name=goose0`) so the host can
  find it.

**Linux host.** The bridge device `goose0` is a port of the same segment. A
Go test on the host opens `goose0` with `CAP_NET_RAW` (`go test -exec sudo`
or `setcap cap_net_raw+ep` on the test binary) and exchanges frames with the
containers: `GOOSE_IFACE=goose0 go test -exec sudo -tags=interop ./...`. A
bridge with no container attached has no carrier: attach one container
first and wait a second before timing-sensitive sends.

**macOS (and Windows).** Docker Desktop runs containers in a Linux VM; the
bridge lives in the VM and macOS has no `AF_PACKET`. Run the Go side in a
container on the same network (`docker run --network iec61850-interop-goose
--cap-add NET_RAW -v "$PWD":/src -w /src golang:1.27 go test ...`), or with
`--network host`, which on Docker Desktop is the VM's namespace.

[docs/goose.md](docs/goose.md#l2-networking) has the full recipes and
caveats (VLAN offload, promiscuous mode, the bridge start-up stall).

## Contract

Every command of every image speaks one contract: JSON Lines on stdout,
diagnostics on stderr, a `ready` event before a server accepts connections
or a publisher sends, one line per operation for the fixed-sequence clients,
`--version` and `--capabilities` on every command, fixed exit codes.

- [docs/contract.md](docs/contract.md) — shared rules, MMS and IED servers and clients, exit codes, the stability promise.
- [docs/goose.md](docs/goose.md) — GOOSE publisher/subscriber, supervision, value JSON, L2 networking.
- [docs/sv.md](docs/sv.md) — SV publisher/subscriber, 9-2LE and generic profiles, loss statistics.

### Compatibility promise (v1)

From **v1.0.0** this repository follows semantic versioning. Image names,
command names and flags, event and operation shapes, exit codes, fixture
revisions (`mms-v2`, `iec61850-v3`, `goose-v1`, `sv-v1`) and the schemas of
`capabilities.json` and `manifest.json` change incompatibly only in a new
major version. Minor versions add (commands, optional fields, new fixture
revisions next to the old ones); patch versions fix adapters to match the
contract and update known limitations. Consumers ignore fields and events
they do not know. Details in [docs/contract.md](docs/contract.md#stability-v1).

## Fixtures

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
`InteropLD/GGIO1.SetInt1.setVal[SP]`. Do not treat `Mod.ctlModel` / `Mod.d`
as assumed-success write targets.

## Consumer repositories

| Repository | Uses |
|---|---|
| [otfabric/go-mms](https://github.com/otfabric/go-mms) | MMS servers and clients (`mms-v2`) |
| [otfabric/go-iec61850](https://github.com/otfabric/go-iec61850) | IED servers, clients, controllers and reporters (`iec61850-v3`); GOOSE and SV publishers and subscribers (`goose-v1`, `sv-v1`) |
| [otfabric/iec61850ctl](https://github.com/otfabric/iec61850ctl) | IED servers and clients for the CLI's end-to-end tests |

Each reads the images from `LIBIEC61850_IMAGE`, `IEC61850BEAN_IMAGE`,
`DSCSYSTEMS_IMAGE` and `IEC61850RUST_IMAGE` (the stacks it tests), pins them
by digest and runs its tests with `-tags=interop`
(`make interop`). A test starts the adapter container, waits for `ready`,
exercises the library, reads the adapter's JSON Lines, and stops the
container; nothing runs beforehand.

`go-mms` owns MMS (ISO stack, ACSE, BER, generic MMS services) and must not
grow IEC 61850 semantics; `go-iec61850` owns the IEC 61850 layer on top of
it. The test directions per repository are in their `INTEROP.md`.

## Licensing

The repository's own code and documentation are MIT unless noted otherwise.
Each image also contains its upstream library, under that library's licence:

| Image | Adapter code | Upstream library | Image as distributed |
|---|---|---|---|
| `iec61850-interop-libiec61850` | MIT | libiec61850 — GPL-3.0 (or commercial) | contains GPL-3.0 code |
| `iec61850-interop-iec61850bean` | MIT | iec61850bean — Apache-2.0 | MIT + Apache-2.0 |
| `iec61850-interop-dscsystems` | **GPL-3.0** (`adapters/dscsystems/LICENSE`, `NOTICE`) | dscsystems/go-iec61850 — GPL-3.0 | GPL-3.0 |
| `iec61850-interop-iec61850-rust` | MIT | iec61850-rust — MIT OR Apache-2.0 | MIT + (MIT OR Apache-2.0) |

**dscsystems isolation.** The dscsystems adapter is compiled into the same Go
binaries as the GPL-3.0 library, so `adapters/dscsystems/` is itself GPL-3.0
and lives only in that directory and that image. Nothing from it may be
copied or vendored into the MIT-licensed `go-mms`, `go-iec61850` or any other
directory of this repository. Consumers talk to every adapter only as a
separate program, over the network and its JSON Lines, which keeps the
consumers' own licence unaffected. Redistributing any image means keeping
the upstream notices and meeting the source obligations of its licence. The
dscsystems and iec61850-rust images carry their `LICENSE` and `NOTICE` files
under `/usr/share/doc/iec61850-interop-<stack>/`; the NOTICE names the
upstream commit the image was built from.

## Building locally

Requires Docker with BuildKit/buildx and Make.

```bash
make build              # all four images as iec61850-interop-<stack>:local
make validate-fixtures  # fixtures, RELEASE.md notes, generated docs (no Docker)
make smoke              # binaries, fixtures, ready events, client/server pairs
make goose-crosscheck   # every GOOSE publisher -> subscriber pair
make sv-crosscheck      # every SV publisher -> subscriber pair
make ci                 # all of the above, as CI runs it
```

Run a consumer's interop tests against the local images:

```bash
LIBIEC61850_IMAGE=iec61850-interop-libiec61850:local \
IEC61850BEAN_IMAGE=iec61850-interop-iec61850bean:local \
DSCSYSTEMS_IMAGE=iec61850-interop-dscsystems:local \
IEC61850RUST_IMAGE=iec61850-interop-iec61850-rust:local \
make interop
```

`docker compose --profile libiec61850 up` and `--profile goose` start a
server and a GOOSE pair for manual inspection.

## Releases

[RELEASE.md](RELEASE.md) has the notes of every version and how a release is
made: tag `vX.Y.Z` (or run the Release workflow), which builds each image
on native amd64 and arm64 runners, publishes the multi-arch manifests to
GHCR and creates the GitHub release with `manifest.json`. Tags are
immutable; a fix is a new version.

## Repository layout

```
adapters/
  libiec61850/     C adapter, built against a pinned libiec61850
  iec61850bean/    Java adapter (Maven)
  dscsystems/      Go adapter, GPL-3.0 (LICENSE, NOTICE)
  iec61850-rust/   Rust adapter (LICENSE-MIT, LICENSE-APACHE, NOTICE)
fixtures/          mms/, iec61850/, goose/, sv/, capabilities.json
docs/              contract.md, goose.md, sv.md
scripts/           smoke test, GOOSE/SV cross-checks, fixture validation, coverage, release notes
```
