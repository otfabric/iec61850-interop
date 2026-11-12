/* SPDX-License-Identifier: MIT */
/*
 * rudp.h — the UDP transport of the libiec61850 GOOSE and SV commands
 * (--transport udp, contract 1.4, docs/routable.md): R-GOOSE and R-SV,
 * the session protocol of IEC TR 61850-90-5, through libiec61850's
 * RSession (src/r_session, built with CONFIG_IEC61850_R_GOOSE and
 * CONFIG_IEC61850_R_SMV).
 *
 * libiec61850 encodes, signs, verifies and decodes every SPDU. The adapter
 * only peeks at each received datagram before libiec61850 reads it, for
 * what RSession does not report: the sender's address, the simulation
 * flag of each payload, and why an SPDU libiec61850 refused was refused
 * (the reason of the reject event is this module's classification;
 * libiec61850 returns one error code for every refusal).
 */
#ifndef RUDP_H
#define RUDP_H

#include <stdbool.h>
#include <stdint.h>

#include "r_session.h"

typedef struct {
    const char* transport;      /* "ethernet" or "udp" */
    const char* remote;         /* publisher: ADDR[:PORT] */
    const char* listen;         /* subscriber: [ADDR]:PORT */
    const char* group;          /* subscriber: multicast ADDR[:PORT] */
    const char* mcast_iface;
    const char* key_hex;
    const char* mac;
    long        ttl;
    long        key_id;
    long        version;
    bool        allow_unsecured;
} RudpArgs;

/* Defaults of docs/routable.md. */
void rudp_defaults(RudpArgs* a);

/* Consume argv[*i] (and its value) when it is a transport flag; returns
 * false when it is not one. */
bool rudp_arg(RudpArgs* a, int argc, char** argv, int* i, const char* prog);

/* true for --transport udp; exits 1 for a value other than ethernet/udp. */
bool rudp_is_udp(const RudpArgs* a, const char* prog);

/* {"event":"error","code":"unsupported",...} for libiec61850-PROG, exit 4. */
void rudp_unsupported(const char* prog, const char* reason);

/* A send-only session to --remote; label is "udp:" + --remote. Exits 1 on
 * an argument error, 4 (unsupported event) on what libiec61850 cannot do. */
RSession rudp_open_sender(const RudpArgs* a, const char* prog, char* label, int label_size);

/* A receive-only session on --listen / --group, started (bound). */
RSession rudp_open_receiver(const RudpArgs* a, const char* prog, char* label, int label_size);

/* One GOOSE or SV payload of an accepted SPDU, rebuilt as the Ethernet
 * frame libiec61850's L2 receivers take: zero MACs, no VLAN tag, the
 * EtherType, APPID, length, Reserved 1 (simulation bit) and Reserved 2,
 * then the APDU. from is the sender's ADDR:PORT. */
typedef void (*RudpFrameHandler)(void* ctx, uint8_t* frame, int len, const char* from);

/* Wait up to timeout_ms for a datagram and let libiec61850 read it; every
 * payload it accepts goes to handler, every SPDU it refuses is reported as
 * a reject event. Returns false on a socket error. */
bool rudp_poll(RSession s, int timeout_ms, RudpFrameHandler handler, void* ctx);

#endif /* RUDP_H */
