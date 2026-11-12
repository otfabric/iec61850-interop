/* SPDX-License-Identifier: MIT */
/*
 * rudp.c — the UDP transport of the libiec61850 GOOSE and SV commands;
 * see rudp.h.
 *
 * What libiec61850 at the pinned revision does, and so what this
 * transport does (docs/routable.md, "libiec61850"):
 *
 * - It sends session protocol version 2 only: struct sRSession has a
 *   protocolVersion field, but no function sets it. --session-version 1
 *   is unsupported (exit 4).
 * - It signs with HMAC-SHA256-128 and -256. With HMAC-SHA256-80 it sends
 *   the SPDU without a signature and refuses every SPDU it receives.
 * - It refuses version 2 SPDUs without a key. In version 1 the received
 *   algorithm octets must equal the ones configured (RSession_setSecurity).
 * - It keeps no replay window.
 */

#include "rudp.h"

#include <arpa/inet.h>
#include <errno.h>
#include <net/if.h>
#include <netinet/in.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>

#include "r_session_internal.h"   /* RSession_receiveMessage, RSession_getSocket */

#include "goose_common.h"
#include "jsonlines.h"

#define RUDP_DEFAULT_PORT 102
#define RUDP_MAX_ELEMENTS 32

/* The receiver's key, for the classification of refusals. */
static long g_key_id;
static int  g_sig;

void rudp_defaults(RudpArgs* a)
{
    memset(a, 0, sizeof(*a));
    a->transport = "ethernet";
    a->mac = "hmac-sha256-128";
    a->version = 2;
    a->ttl = 1;
}

static const char* value(int argc, char** argv, int* i, const char* prog)
{
    if (*i + 1 >= argc) jl_fatal(1, "%s: %s needs a value", prog, argv[*i]);
    return argv[++*i];
}

bool rudp_arg(RudpArgs* a, int argc, char** argv, int* i, const char* prog)
{
    const char* f = argv[*i];
    if      (!strcmp(f, "--transport"))       a->transport = value(argc, argv, i, prog);
    else if (!strcmp(f, "--remote"))          a->remote = value(argc, argv, i, prog);
    else if (!strcmp(f, "--listen"))          a->listen = value(argc, argv, i, prog);
    else if (!strcmp(f, "--group"))           a->group = value(argc, argv, i, prog);
    else if (!strcmp(f, "--mcast-iface"))     a->mcast_iface = value(argc, argv, i, prog);
    else if (!strcmp(f, "--key"))             a->key_hex = value(argc, argv, i, prog);
    else if (!strcmp(f, "--mac"))             a->mac = value(argc, argv, i, prog);
    else if (!strcmp(f, "--ttl"))             a->ttl = strtol(value(argc, argv, i, prog), NULL, 0);
    else if (!strcmp(f, "--key-id"))          a->key_id = strtol(value(argc, argv, i, prog), NULL, 0);
    else if (!strcmp(f, "--session-version")) a->version = strtol(value(argc, argv, i, prog), NULL, 0);
    else if (!strcmp(f, "--allow-unsecured")) a->allow_unsecured = true;
    else return false;
    return true;
}

bool rudp_is_udp(const RudpArgs* a, const char* prog)
{
    if (!strcmp(a->transport, "udp")) return true;
    if (!strcmp(a->transport, "ethernet")) return false;
    jl_fatal(1, "%s: --transport %s: want ethernet or udp", prog, a->transport);
    return false;
}

void rudp_unsupported(const char* prog, const char* reason)
{
    char cmd[64];
    snprintf(cmd, sizeof(cmd), "libiec61850-%s", prog);
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("error"));
    json_object_object_add(o, "code", json_object_new_string("unsupported"));
    json_object_object_add(o, "command", json_object_new_string(cmd));
    json_object_object_add(o, "reason", json_object_new_string(reason));
    goose_emit(o);
    jl_fatal(4, "%s: unsupported: %s", prog, reason);
}

/* "ADDR:PORT", "ADDR", ":PORT" -> host (empty for none) and port. */
static void split_addr(const char* s, char* host, int host_size, int* port, const char* prog, const char* flag)
{
    const char* colon = strrchr(s, ':');
    *port = RUDP_DEFAULT_PORT;
    int n = colon ? (int) (colon - s) : (int) strlen(s);
    if (n >= host_size) jl_fatal(1, "%s: %s %s: address too long", prog, flag, s);
    memcpy(host, s, (size_t) n);
    host[n] = '\0';
    if (colon) {
        char* end = NULL;
        long p = strtol(colon + 1, &end, 10);
        if (!*(colon + 1) || *end || p < 1 || p > 65535)
            jl_fatal(1, "%s: %s %s: bad port", prog, flag, s);
        *port = (int) p;
    }
}

static RSignatureAlgorithm sig_algorithm(const char* mac, const char* prog)
{
    if (!strcasecmp(mac, "hmac-sha256-80"))  return R_SESSION_SIG_ALGO_HMAC_SHA256_80;
    if (!strcasecmp(mac, "hmac-sha256-128")) return R_SESSION_SIG_ALGO_HMAC_SHA256_128;
    if (!strcasecmp(mac, "hmac-sha256-256")) return R_SESSION_SIG_ALGO_HMAC_SHA256_256;
    jl_fatal(1, "%s: --mac %s: want hmac-sha256-80, hmac-sha256-128 or hmac-sha256-256", prog, mac);
    return R_SESSION_SIG_ALGO_NONE;
}

/* --key-id, --key and --mac into the session. */
static void add_key(RSession s, const RudpArgs* a, const char* prog)
{
    RSignatureAlgorithm sig = sig_algorithm(a->mac, prog);
    g_key_id = a->key_id;
    g_sig = (int) sig;
    if (a->key_id == 0) {
        if (a->key_hex) jl_fatal(1, "%s: --key without --key-id", prog);
        RSession_setSecurity(s, R_SESSION_SEC_ALGO_NONE, R_SESSION_SIG_ALGO_NONE);
        return;
    }
    if (a->key_id < 0 || a->key_id > 0xFFFFFFFFL)
        jl_fatal(1, "%s: --key-id %ld out of range", prog, a->key_id);
    const char* h = a->key_hex ? a->key_hex : "";
    size_t n = strlen(h);
    if (n % 2 || n / 2 < 16 || n / 2 > 64)
        jl_fatal(1, "%s: --key: want 16 to 64 octets of hex key material", prog);
    uint8_t key[64];
    for (size_t i = 0; i < n / 2; i++) {
        unsigned int b;
        if (sscanf(h + 2 * i, "%2x", &b) != 1) jl_fatal(1, "%s: --key: not hex", prog);
        key[i] = (uint8_t) b;
    }
    if (RSession_addKey(s, (uint32_t) a->key_id, key, (int) (n / 2), R_SESSION_SEC_ALGO_NONE, sig) != R_SESSION_ERROR_OK ||
        RSession_setActiveKey(s, (uint32_t) a->key_id) != R_SESSION_ERROR_OK)
        jl_fatal(1, "%s: RSession_addKey failed", prog);
    /* Version 1 carries the algorithms on the wire; the receiver compares
     * them with these. Version 2 takes them from the key. */
    RSession_setSecurity(s, R_SESSION_SEC_ALGO_NONE, sig);
}

/* The descriptor of the session's UDP socket: the first member of struct
 * sUdpSocket (hal/socket/linux/socket_linux.c), as sv_subscriber.c reads
 * that of an EthernetSocket. */
static int session_fd(RSession s)
{
    void* sock = RSession_getSocket(s);
    return sock ? *(int*) sock : -1;
}

static struct ip_mreqn iface_mreq(const char* iface, const char* prog)
{
    struct ip_mreqn m;
    memset(&m, 0, sizeof(m));
    m.imr_ifindex = (int) if_nametoindex(iface);
    if (m.imr_ifindex == 0) jl_fatal(1, "%s: --mcast-iface %s: no such interface", prog, iface);
    return m;
}

RSession rudp_open_sender(const RudpArgs* a, const char* prog, char* label, int label_size)
{
    if (!a->remote) jl_fatal(1, "%s: --transport udp needs --remote", prog);
    if (a->version != 1 && a->version != 2)
        jl_fatal(1, "%s: --session-version %ld: want 1 or 2", prog, a->version);
    sig_algorithm(a->mac, prog);
    if (a->version == 1)
        rudp_unsupported(prog, "libiec61850 sends session protocol version 2 only (no API sets the version)");

    char host[256];
    int port;
    split_addr(a->remote, host, sizeof(host), &port, prog, "--remote");
    if (!host[0]) jl_fatal(1, "%s: --remote %s: want ADDR[:PORT]", prog, a->remote);

    RSession s = RSession_create();
    if (!s) jl_fatal(1, "%s: RSession_create failed", prog);
    add_key(s, a, prog);
    RSession_setRemoteAddress(s, host, port);
    if (RSession_setMulticastTtl(s, a->ttl > 0 ? (int) a->ttl : 1) != R_SESSION_ERROR_OK)
        jl_fatal(1, "%s: --ttl %ld: RSession_setMulticastTtl failed", prog, a->ttl);
    if (a->mcast_iface) {
        struct ip_mreqn m = iface_mreq(a->mcast_iface, prog);
        if (setsockopt(session_fd(s), IPPROTO_IP, IP_MULTICAST_IF, &m, sizeof(m)) != 0)
            jl_fatal(1, "%s: --mcast-iface %s: %s", prog, a->mcast_iface, strerror(errno));
    }
    snprintf(label, (size_t) label_size, "udp:%s", a->remote);
    return s;
}

RSession rudp_open_receiver(const RudpArgs* a, const char* prog, char* label, int label_size)
{
    char host[256] = "", ghost[256] = "";
    int port = RUDP_DEFAULT_PORT, gport = RUDP_DEFAULT_PORT;
    sig_algorithm(a->mac, prog);
    if (a->group) {
        split_addr(a->group, ghost, sizeof(ghost), &gport, prog, "--group");
        port = gport;
    }
    if (a->listen)
        split_addr(a->listen, host, sizeof(host), &port, prog, "--listen");

    RSession s = RSession_create();
    if (!s) jl_fatal(1, "%s: RSession_create failed", prog);
    add_key(s, a, prog);
    RSession_setLocalAddress(s, host[0] ? host : NULL, port);
    if (RSession_start(s) != R_SESSION_ERROR_OK)
        jl_fatal(1, "%s: cannot bind %s:%d (RSession_start)", prog, host[0] ? host : "0.0.0.0", port);
    int fd = session_fd(s);
    if (a->group) {
        if (a->mcast_iface) {
            struct ip_mreqn m = iface_mreq(a->mcast_iface, prog);
            if (inet_pton(AF_INET, ghost, &m.imr_multiaddr) != 1 ||
                setsockopt(fd, IPPROTO_IP, IP_ADD_MEMBERSHIP, &m, sizeof(m)) != 0)
                jl_fatal(1, "%s: --group %s on %s: %s", prog, a->group, a->mcast_iface, strerror(errno));
        } else if (RSession_addMulticastGroup(s, ghost) != R_SESSION_ERROR_OK) {
            jl_fatal(1, "%s: --group %s: RSession_addMulticastGroup failed", prog, a->group);
        }
    }
    /* An R-SV stream is thousands of datagrams a second; ask for 32 MiB
     * (the kernel caps it at net.core.rmem_max). */
    int rcvbuf = 32 << 20;
    setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &rcvbuf, sizeof(rcvbuf));

    if (a->group) snprintf(label, (size_t) label_size, "udp:%s", a->group);
    else snprintf(label, (size_t) label_size, "udp:%s:%d", host, port);
    return s;
}

/* ------------------------------------------------------------------------
 * Receive
 * ---------------------------------------------------------------------- */

typedef struct {
    uint8_t  type;        /* 0x81 GOOSE, 0x82 SV */
    bool     simulation;
} Element;

/* The adapter's reading of a datagram (fixed field positions of the
 * session header, as libiec61850 parses it). */
typedef struct {
    bool     header_ok;   /* the header and payload elements are within the datagram */
    int      version;
    uint32_t key_id;
    uint8_t  sig_algo;    /* version 1 only */
    Element  elements[RUDP_MAX_ELEMENTS];
    int      count;
    char     detail[96];
} Peek;

static uint32_t be32(const uint8_t* b) { return (uint32_t) b[0] << 24 | (uint32_t) b[1] << 16 | (uint32_t) b[2] << 8 | b[3]; }

static void peek_spdu(const uint8_t* b, int n, Peek* p)
{
    memset(p, 0, sizeof(*p));
    if (n < 26 || b[0] != 0x01 || b[1] != 0x40 || b[4] != 0x80) {
        snprintf(p->detail, sizeof(p->detail), "not a session protocol SPDU");
        return;
    }
    if (b[2] != 0xa1 && b[2] != 0xa2) {
        snprintf(p->detail, sizeof(p->detail), "SI 0x%02x", b[2]);
        return;
    }
    p->version = b[14] << 8 | b[15];
    int pos;
    if (p->version == 1) {
        if (n < 32) { snprintf(p->detail, sizeof(p->detail), "truncated header"); return; }
        p->sig_algo = b[23];
        p->key_id = be32(b + 24);
        pos = 28;
    } else if (p->version == 2) {
        p->key_id = be32(b + 22);
        pos = 27 + b[26];
    } else {
        snprintf(p->detail, sizeof(p->detail), "session protocol version %d", p->version);
        return;
    }
    if (pos + 4 > n) { snprintf(p->detail, sizeof(p->detail), "truncated header"); return; }
    uint32_t plen = be32(b + pos);
    pos += 4;
    if (plen > (uint32_t) (n - pos)) { snprintf(p->detail, sizeof(p->detail), "payload length %u past the datagram", plen); return; }
    int end = pos + (int) plen;
    while (pos < end) {
        if (pos + 6 > end) { snprintf(p->detail, sizeof(p->detail), "truncated payload element"); return; }
        int len = b[pos + 4] << 8 | b[pos + 5];
        if (len > end - pos - 6) { snprintf(p->detail, sizeof(p->detail), "APDU length %d past the payload", len); return; }
        if ((b[pos] == 0x81 || b[pos] == 0x82) && p->count < RUDP_MAX_ELEMENTS) {
            p->elements[p->count].type = b[pos];
            p->elements[p->count].simulation = b[pos + 1] != 0;
            p->count++;
        }
        pos += 6 + len;
    }
    p->header_ok = true;
}

typedef struct {
    RudpFrameHandler handler;
    void*            ctx;
    const Peek*      peek;
    int              index;
    uint8_t          si;
    const char*      from;
} Delivery;

static void deliver(void* parameter, uint16_t app_id, uint8_t* apdu, int size)
{
    Delivery* d = (Delivery*) parameter;
    static uint8_t frame[65536 + 22];
    if (size < 0 || size > 65535 - 8) return;
    uint8_t type = d->si == 0xa2 ? 0x82 : 0x81;
    bool sim = false;
    if (d->index < d->peek->count) {
        type = d->peek->elements[d->index].type;
        sim = d->peek->elements[d->index].simulation;
    }
    d->index++;
    memset(frame, 0, 12);
    frame[12] = 0x88;
    frame[13] = type == 0x82 ? 0xba : 0xb8;
    int len = 8 + size;
    frame[14] = (uint8_t) (app_id >> 8);
    frame[15] = (uint8_t) app_id;
    frame[16] = (uint8_t) (len >> 8);
    frame[17] = (uint8_t) len;
    frame[18] = sim ? 0x80 : 0;
    frame[19] = frame[20] = frame[21] = 0;
    memcpy(frame + 22, apdu, (size_t) size);
    d->handler(d->ctx, frame, 22 + size, d->from);
}

static void emit_reject(const char* from, const char* reason, const char* error)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("reject"));
    json_object_object_add(o, "from", json_object_new_string(from));
    json_object_object_add(o, "reason", json_object_new_string(reason));
    json_object_object_add(o, "error", json_object_new_string(error));
    goose_emit(o);
}

/* Why libiec61850 refused the SPDU, from the adapter's reading of it. */
static void classify(Peek* p, const char* from)
{
    char err[200];
    const char* reason;
    if (!p->header_ok) {
        reason = "malformed";
    } else if (p->key_id == 0) {
        reason = "unsecured";
        snprintf(p->detail, sizeof(p->detail), "no key");
    } else if ((long) p->key_id != g_key_id) {
        reason = "unknown-key";
        snprintf(p->detail, sizeof(p->detail), "key ID %u", p->key_id);
    } else if (p->version == 1 && p->sig_algo != g_sig) {
        reason = "authentication";
        snprintf(p->detail, sizeof(p->detail), "signature algorithm %u, configured %d", p->sig_algo, g_sig);
    } else {
        reason = "authentication";
        snprintf(p->detail, sizeof(p->detail), "signature of key ID %u", p->key_id);
    }
    snprintf(err, sizeof(err), "libiec61850 refused the SPDU (RSession_receiveMessage: invalid message); %s", p->detail);
    emit_reject(from, reason, err);
}

bool rudp_poll(RSession s, int timeout_ms, RudpFrameHandler handler, void* ctx)
{
    static uint8_t buf[65536];
    int fd = session_fd(s);
    struct pollfd pfd = { fd, POLLIN, 0 };
    int r = poll(&pfd, 1, timeout_ms);
    if (r < 0) return errno == EINTR;
    if (r == 0) return true;

    for (int i = 0; i < 256; i++) {
        struct sockaddr_in from;
        socklen_t from_len = sizeof(from);
        ssize_t n = recvfrom(fd, buf, sizeof(buf), MSG_PEEK | MSG_DONTWAIT, (struct sockaddr*) &from, &from_len);
        if (n < 0) return errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR;
        char addr[INET_ADDRSTRLEN + 8], ip[INET_ADDRSTRLEN] = "";
        inet_ntop(AF_INET, &from.sin_addr, ip, sizeof(ip));
        snprintf(addr, sizeof(addr), "%s:%u", ip, ntohs(from.sin_port));

        Peek p;
        peek_spdu(buf, (int) n, &p);
        Delivery d = { handler, ctx, &p, 0, n > 2 ? buf[2] : 0, addr };
        /* libiec61850 reads the datagram (the peeked one) and decides. */
        RSessionError e = RSession_receiveMessage(s, deliver, &d);
        if (e == R_SESSION_ERROR_INVALID_MESSAGE)
            classify(&p, addr);
        else if (e != R_SESSION_ERROR_OK)
            return false;
    }
    return true;
}
