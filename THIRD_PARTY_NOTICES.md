# Third-party notices

The repository's own code and documentation are MIT ([LICENSE](LICENSE)) unless noted otherwise. Each published image also contains its upstream library under that library's licence. `adapters/dscsystems/` is GPL-3.0 ([adapters/dscsystems/LICENSE](adapters/dscsystems/LICENSE), [NOTICE](adapters/dscsystems/NOTICE)) and lives only in that directory and image.

| Image | Adapter code | Upstream | Pin | Upstream licence | Image as distributed |
|---|---|---|---|---|---|
| `iec61850-interop-libiec61850` | MIT | [libiec61850](https://github.com/mz-automation/libiec61850) | `6e265c5` | GPL-3.0 (or commercial) | contains GPL-3.0 code |
| `iec61850-interop-iec61850bean` | MIT | [iec61850bean](https://github.com/beanit/iec61850bean) | 1.9.0 | Apache-2.0 | MIT + Apache-2.0 |
| `iec61850-interop-dscsystems` | GPL-3.0 | dscsystems/go-iec61850 | `v0.6.6` (`4833767`) | GPL-3.0 | GPL-3.0 |
| `iec61850-interop-iec61850-rust` | MIT | [iec61850-rust](https://github.com/csp0924/iec61850-rust) | `d4e8952` | MIT OR Apache-2.0 | MIT + (MIT OR Apache-2.0) |
| `iec61850-interop-cbsigma` | MIT | [cB-Sigma/iec61850](https://github.com/cB-Sigma/iec61850) and [iec61850-sim](https://github.com/cB-Sigma/iec61850-sim) | `v0.1.0` (`f121ba3`), `v0.1.0` (`92530d8`) | Apache-2.0 | MIT + Apache-2.0 + BSD-3-Clause |

`iec61850-interop-cbsigma` also links cB-Sigma's iotsec, iotcrypto and xsd (Apache-2.0; iotcrypto's bigmod package BSD-3-Clause), golang.org/x/net, x/sys and x/text (BSD-3-Clause), gopkg.in/yaml.v3 (Apache-2.0 and MIT) and the Go standard library (BSD-3-Clause). The versions are in [adapters/cbsigma/go.mod](adapters/cbsigma/go.mod); the image carries every module's licence and notice under `/licenses`, with the list in `/licenses/BUNDLED-MODULES.txt`, and [adapters/cbsigma/NOTICE](adapters/cbsigma/NOTICE) under `/licenses/iec61850-interop/`.

`iec61850-interop-otfabric` ([own/go-iec61850](own/go-iec61850/README.md)) is not a release image: its adapter code and its upstreams, go-iec61850 and go-mms (and their Go module dependencies), are MIT or BSD-3-Clause; the licence of every module it contains is in the image under `/licenses`, with the list in `/licenses/BUNDLED-MODULES.txt`.

Consumers talk to every adapter as a separate program, over the network and its JSON Lines, which keeps their own licence unaffected. Redistributing an image means keeping the upstream notices and meeting the source obligations of its licence. The dscsystems and iec61850-rust images carry their `LICENSE` and `NOTICE` under `/usr/share/doc/iec61850-interop-<stack>/`. The libiec61850 and iec61850bean images do not yet carry licence texts.
