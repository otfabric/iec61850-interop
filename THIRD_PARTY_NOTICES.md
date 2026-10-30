# Third-party notices

The repository's own code and documentation are MIT ([LICENSE](LICENSE)) unless noted otherwise. Each published image also contains its upstream library under that library's licence. `adapters/dscsystems/` is GPL-3.0 ([adapters/dscsystems/LICENSE](adapters/dscsystems/LICENSE), [NOTICE](adapters/dscsystems/NOTICE)) and lives only in that directory and image.

| Image | Adapter code | Upstream | Pin | Upstream licence | Image as distributed |
|---|---|---|---|---|---|
| `iec61850-interop-libiec61850` | MIT | [libiec61850](https://github.com/mz-automation/libiec61850) | `6e265c5` | GPL-3.0 (or commercial) | contains GPL-3.0 code |
| `iec61850-interop-iec61850bean` | MIT | [iec61850bean](https://github.com/beanit/iec61850bean) | 1.9.0 | Apache-2.0 | MIT + Apache-2.0 |
| `iec61850-interop-dscsystems` | GPL-3.0 | dscsystems/go-iec61850 | `6bf3f37` | GPL-3.0 | GPL-3.0 |
| `iec61850-interop-iec61850-rust` | MIT | [iec61850-rust](https://github.com/csp0924/iec61850-rust) | `d4e8952` | MIT OR Apache-2.0 | MIT + (MIT OR Apache-2.0) |

Consumers talk to every adapter as a separate program, over the network and its JSON Lines, which keeps their own licence unaffected. Redistributing an image means keeping the upstream notices and meeting the source obligations of its licence. The dscsystems and iec61850-rust images carry their `LICENSE` and `NOTICE` under `/usr/share/doc/iec61850-interop-<stack>/`. The libiec61850 and iec61850bean images do not yet carry licence texts.
