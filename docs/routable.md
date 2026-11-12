# R-GOOSE and R-SV (UDP transport)

Contract 1.4 (minor) adds a second transport to the GOOSE and Sampled
Values commands: `--transport udp` sends and receives R-GOOSE and R-SV,
the GOOSE and SV APDUs carried in the session protocol of IEC TR
61850-90-5 over UDP, signed with an HMAC. Everything else about the
commands (the fixtures, the stdin commands, the events and their fields)
is as in [goose.md](goose.md) and [sv.md](sv.md); this page lists what
the UDP transport changes.

| Image | `--transport udp` |
|---|---|
| `iec61850-interop-libiec61850` | yes, through [libiec61850](https://github.com/mz-automation/libiec61850)'s `RSession`, with the limits below |
| `iec61850-interop-dscsystems` | yes, through [dscsystems/go-iec61850](https://github.com/dscsystems/go-iec61850) `rsession` |
| the others | no |

An image whose commands take it lists the transports in `--capabilities`:
`"transports":["ethernet","udp"]`. An image without the field has the
Ethernet transport only.

## Flags

Shared by `*-goose-publisher`, `*-goose-subscriber`, `*-sv-publisher` and
`*-sv-subscriber`:

| Flag | Default | Meaning |
|---|---|---|
| `--transport ethernet\|udp` | `ethernet` | `udp` selects R-GOOSE / R-SV |
| `--key-id N` | `0` | key identifier; `0` sends unsecured |
| `--key HEX` | | key material, at least 16 octets |
| `--mac ALG` | `hmac-sha256-128` | `hmac-sha256-80`, `hmac-sha256-128` or `hmac-sha256-256` |
| `--mcast-iface IF` | system choice | interface to join `--group` and send multicast on |

Publishers:

| Flag | Default | Meaning |
|---|---|---|
| `--remote ADDR:PORT` | (required) | where SPDUs are sent, unicast or multicast; port 102 when omitted |
| `--session-version 1\|2` | `2` | session protocol version sent |
| `--ttl N` | `1` | multicast hop limit |

Subscribers:

| Flag | Default | Meaning |
|---|---|---|
| `--listen ADDR:PORT` | `:102` when there is no `--group` | local address to receive on |
| `--group ADDR[:PORT]` | | multicast group to join |
| `--allow-unsecured` | off | accept SPDUs that carry no key |

A subscriber accepts both session protocol versions. With
`--transport udp`, `--iface`, `--dst`, `--vlan-id` and `--vlan-prio` are
accepted and not used: an SPDU carries no Ethernet addresses and no VLAN
tag.

## Events

- `ready`: `address` and `iface` are `udp:` followed by the remote
  (publisher) or local (subscriber) address; `src` is the zero MAC.
- `goose`, `sv`: `dst` and `src` are the zero MAC. A `goose` event carries
  `from`, the sender's `address:port`.
- `reject` (new): one per SPDU the subscriber refuses,
  `{"event":"reject","from":ADDR,"reason":R,"error":TEXT}`, with `R` one of
  `unknown-key` (no key of that ID), `authentication` (the MAC does not
  verify, or the algorithm does not match the key), `unsecured` (no key,
  without `--allow-unsecured`), `replay` (a number the sender's replay
  window has seen) and `malformed`. A refused SPDU produces no `goose` or
  `sv` event.

## Checks

`scripts/goose-crosscheck.sh` and `scripts/sv-crosscheck.sh` with
`TRANSPORT=udp`: the publisher sends to the subscriber's container on port
102 with key 1, HMAC-SHA256-128, after a rogue publisher with the same key
ID and other key material, whose SPDUs the subscriber must all refuse with
`reason` `authentication`. `scripts/interop.sh` runs them between every
pair of adapters with the udp transport.

## libiec61850

The image builds libiec61850 (pinned revision) with Mbed TLS v3.6.0 in
`third_party/mbedtls`, which turns on `CONFIG_IEC61850_R_GOOSE` and
`CONFIG_IEC61850_R_SMV`. The publishers use `GoosePublisher_createRemote`
and `SVPublisher_createRemote` on an `RSession`; the subscribers hand each
received datagram to `RSession_receiveMessage` and each payload it accepts
to the same `GooseReceiver` and `SVReceiver` as an Ethernet frame. The
adapter peeks at each datagram first, for what `RSession` does not
report: the sender (`from`), the simulation octet of each payload, and the
`reason` of a refusal. libiec61850 returns one error for every refusal, so
the reason is the adapter's reading of the refused SPDU (no key:
`unsecured`; another key ID: `unknown-key`; otherwise `authentication`,
or `malformed` when the header or payload lengths run past the datagram).
libiec61850 decides what is accepted.

What libiec61850 does, at the pinned revision (`fixtures/capabilities.json`
`knownLimitations`):

| | |
|---|---|
| `--session-version 1` on a publisher | unsupported (exit 4): `RSession` sends version 2 only, no function sets the version. Subscribers accept both versions |
| `--mac hmac-sha256-80` | the encoder sends no signature, the decoder refuses every SPDU |
| `--mac hmac-sha256-256` on a publisher | the signature length octet is 16 before a 32-octet MAC; a receiver that checks it refuses the SPDU |
| APDU length | written two octets too long; libiec61850 refuses the SPDUs of its own publisher as `malformed`, so libiec61850 → libiec61850 is a known failure in `scripts/interop.sh` |
| replay | no replay window: a replayed SPDU is accepted, no `reject` of reason `replay` |
| `--allow-unsecured` | no effect on version 2: an SPDU without a key is always refused |
| `--simulation` on the GOOSE publisher | only together with `--test`: libiec61850 writes goosePdu field [7] into the simulation octet of the payload |
| `--simulation` on the SV publisher | sets the simulation flag of `struct sSVPublisher`, which has no API |

## What this is not

- No AES-GMAC or AES-GCM in the contract: the flags name the HMACs only.
- No key distribution (IEC 62351-9 GDOI): the key is given on the command
  line.
- No layer-2 IEC 62351-6 authentication of Ethernet GOOSE and SV.
