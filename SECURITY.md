# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in `mms-interop`, please report it responsibly.

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please email **security@otfabric.io**, or open a private advisory on GitHub (the [Security](https://github.com/otfabric/mms-interop/security) tab → **Advisories** → **Report a vulnerability**), with a description, steps to reproduce and the affected image tag or digest.

Vulnerabilities in the bundled implementations themselves (libiec61850, IEC61850bean) belong upstream; report them there, and tell us so that the pin can be moved.

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest release | Yes |
| Older releases | No; pull the latest images |

## Security Considerations

### These are test fixtures, not products

The images run **unauthenticated, unencrypted MMS and IEC 61850 servers** that accept reads, writes and control operations from anyone who can reach them, and clients (`*-ied-client`, `*-ied-controller`, `*-ied-reporter`, `libiec61850-mms-client`) that write values and operate controls on whatever host they are given. That is their purpose.

- **Never attach a server to a substation, plant or other production network**
- **Never point a client at a real IED**: the controller adapters operate controllable objects (direct, SBO and SBOw), and the client adapters write values
- The adapters use plain ISO-on-TCP. There is no TLS and no ACSE authentication
- `docker-compose.yml` and `scripts/smoke-test.sh` publish the server port on all host interfaces. Run them on a host whose network you trust, or change the mapping to `127.0.0.1`. The compose service is for manual inspection only and also runs read-only, with all capabilities dropped except `NET_BIND_SERVICE` and `no-new-privileges`

### Image hardening

- Processes run as a non-root user (uid 10001)
- Base images are pinned by digest
- The runtime images contain the adapter programs, their runtime libraries and the fixtures; build tools stay in the build stages

### Supply chain

- libiec61850 is cloned from GitHub during the image build and verified against the pinned commit hash
- IEC61850bean and the adapter's other Java dependencies come from Maven Central at the versions in `adapters/iec61850bean/pom.xml`
- Consumers should pin images by digest, taken from the `manifest.json` of a release
