/* SPDX-License-Identifier: MIT */
/*
 * goose_subscriber.c — libIEC61850 GOOSE subscriber adapter for iec61850-interop.
 *
 * Usage:
 *   libiec61850-goose-subscriber --iface IF [--app-id 0xNNNN]
 *       [--gocb-ref REF] [--dst MAC]
 *
 * Opens a raw socket on IF (needs CAP_NET_RAW), emits {"event":"ready"},
 * then one {"event":"goose",...} line per received frame that passes the
 * filters. The adapter reads frames itself and hands each one to
 * GooseReceiver_handleMessage with a single observer GooseSubscriber, so
 * libiec61850 decodes every message while the adapter still sees the
 * Ethernet header: source and destination MAC, and the Edition 2 S bit of
 * Reserved1, which the GooseReceiver skips. goosePdu field [7] is reported
 * as "test" (GooseSubscriber_isTest).
 *
 * Supervision: each gocbRef is supervised with the timeAllowedToLive of
 * its last frame; when it passes without a frame the command emits
 * {"event":"supervision","gocbRef":REF,"state":"timeout"}, and the next
 * frame of that gocbRef emits "state":"ok" before its goose event.
 *
 * --transport udp (contract 1.4, docs/routable.md) receives R-GOOSE on an
 * RSession (rudp.c): libiec61850 verifies and decodes each SPDU, and each
 * GOOSE payload it accepts goes through the same GooseReceiver as a frame
 * with zero MACs; the goose event carries "from", the sender's ADDR:PORT.
 * Each SPDU libiec61850 refuses is a reject event. --iface, --dst are not
 * used.
 *
 * Exit codes: 0 on SIGTERM/SIGINT, 1 argument or socket error.
 */

#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "goose_receiver.h"
#include "goose_subscriber.h"
#include "goose_receiver_internal.h"  /* sGooseSubscriber.timestamp (private) */
#include "hal_ethernet.h"
#include "hal_time.h"

#include "goose_common.h"
#include "jsonlines.h"
#include "rudp.h"

#define MAX_STREAMS 64
#define FRAME_MAX   2048

static volatile sig_atomic_t g_running = 1;
static void sig_handler(int sig) { (void) sig; g_running = 0; }

typedef struct {
    char     gocb_ref[130];
    uint64_t deadline_ms;   /* 0: not supervised */
    bool     timed_out;
} Stream;

static Stream      g_streams[MAX_STREAMS];
static int         g_stream_count;
static const char* g_gocb_filter;

/* Ethernet header of the frame being decoded. */
static uint8_t g_dst[6], g_src[6];
static bool    g_simulation;
static const char* g_from;      /* udp: the sender of the SPDU */

static GooseReceiver g_receiver;
static uint16_t g_app_id;
static bool     g_have_app_id, g_have_dst;
static uint8_t  g_dst_filter[6];

static void emit_supervision(const char* ref, const char* state)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("supervision"));
    json_object_object_add(o, "gocbRef", json_object_new_string(ref));
    json_object_object_add(o, "state", json_object_new_string(state));
    goose_emit(o);
}

static Stream* stream_for(const char* ref)
{
    for (int i = 0; i < g_stream_count; i++)
        if (!strcmp(g_streams[i].gocb_ref, ref)) return &g_streams[i];
    if (g_stream_count == MAX_STREAMS) return NULL;
    Stream* s = &g_streams[g_stream_count++];
    memset(s, 0, sizeof(*s));
    snprintf(s->gocb_ref, sizeof(s->gocb_ref), "%s", ref);
    return s;
}

static void check_supervision(void)
{
    uint64_t now = Hal_getMonotonicTimeInMs();
    for (int i = 0; i < g_stream_count; i++) {
        Stream* s = &g_streams[i];
        if (s->deadline_ms && !s->timed_out && now >= s->deadline_ms) {
            s->timed_out = true;
            emit_supervision(s->gocb_ref, "timeout");
        }
    }
}

static void listener(GooseSubscriber sub, void* param)
{
    (void) param;
    const char* ref = GooseSubscriber_getGoCbRef(sub);
    if (g_gocb_filter && strcmp(ref, g_gocb_filter) != 0)
        return;

    uint32_t tal = GooseSubscriber_getTimeAllowedToLive(sub);
    Stream* s = stream_for(ref);
    if (s) {
        if (s->timed_out) {
            s->timed_out = false;
            emit_supervision(ref, "ok");
        }
        s->deadline_ms = tal ? Hal_getMonotonicTimeInMs() + tal : 0;
    }

    char mac[18], t[48], appid[8];
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("goose"));
    json_object_object_add(o, "gocbRef", json_object_new_string(ref));
    json_object_object_add(o, "datSet", json_object_new_string(GooseSubscriber_getDataSet(sub)));
    json_object_object_add(o, "goID", json_object_new_string(GooseSubscriber_getGoId(sub)));
    snprintf(appid, sizeof(appid), "0x%04x", GooseSubscriber_getAppId(sub) & 0xffff);
    json_object_object_add(o, "appID", json_object_new_string(appid));
    goose_format_mac(g_dst, mac);
    json_object_object_add(o, "dst", json_object_new_string(mac));
    goose_format_mac(g_src, mac);
    json_object_object_add(o, "src", json_object_new_string(mac));
    json_object_object_add(o, "stNum", json_object_new_int64(GooseSubscriber_getStNum(sub)));
    json_object_object_add(o, "sqNum", json_object_new_int64(GooseSubscriber_getSqNum(sub)));
    json_object_object_add(o, "timeAllowedToLive", json_object_new_int64(tal));
    json_object_object_add(o, "confRev", json_object_new_int64(GooseSubscriber_getConfRev(sub)));
    json_object_object_add(o, "test", json_object_new_boolean(GooseSubscriber_isTest(sub)));
    json_object_object_add(o, "simulation", json_object_new_boolean(g_simulation));
    json_object_object_add(o, "ndsCom", json_object_new_boolean(GooseSubscriber_needsCommission(sub)));
    if (g_from)
        json_object_object_add(o, "from", json_object_new_string(g_from));

    MmsValue* values = GooseSubscriber_getDataSetValues(sub);
    int n = values ? MmsValue_getArraySize(values) : 0;
    json_object_object_add(o, "numDatSetEntries", json_object_new_int(n));

    /* The timestamp is read from its raw octets for nanosecond precision;
     * GooseSubscriber_getTimestamp only offers milliseconds. */
    goose_format_utc_ns(MmsValue_getUtcTimeBuffer(sub->timestamp), t);
    json_object_object_add(o, "t", json_object_new_string(t));

    struct json_object* arr = json_object_new_array();
    for (int i = 0; i < n; i++)
        json_object_array_add(arr, goose_value_to_json(MmsValue_getElement(values, i)));
    json_object_object_add(o, "values", arr);
    goose_emit(o);
}

/* One Ethernet frame (from the raw socket, or rebuilt from an SPDU). */
static void handle_frame(uint8_t* frame, int n)
{
    int off = 12;
    if (n < 14) return;
    if (frame[12] == 0x81 && frame[13] == 0x00) off += 4;
    if (n < off + 10 || frame[off] != 0x88 || frame[off + 1] != 0xb8) return;
    int apdu = off + 2;
    uint16_t frame_app_id = (uint16_t)(frame[apdu] << 8 | frame[apdu + 1]);
    if (g_have_app_id && frame_app_id != g_app_id) return;
    if (g_have_dst && !g_from && memcmp(frame, g_dst_filter, 6) != 0) return;
    memcpy(g_dst, frame, 6);
    memcpy(g_src, frame + 6, 6);
    g_simulation = (frame[apdu + GOOSE_SIM_OCTET] & GOOSE_SIM_MASK) != 0;
    GooseReceiver_handleMessage(g_receiver, frame, n);
}

static void handle_udp_frame(void* ctx, uint8_t* frame, int len, const char* from)
{
    (void) ctx;
    g_from = from;
    handle_frame(frame, len);
    g_from = NULL;
}

static const char* arg(int argc, char** argv, int* i)
{
    if (*i + 1 >= argc) jl_fatal(1, "goose-subscriber: %s needs a value", argv[*i]);
    return argv[++*i];
}

int main(int argc, char** argv)
{
    jl_handle_meta_flags(argc, argv, "libiec61850");

    const char* iface = "eth0";
    RudpArgs ra;
    rudp_defaults(&ra);

    for (int i = 1; i < argc; i++) {
        const char* a = argv[i];
        if (rudp_arg(&ra, argc, argv, &i, "goose-subscriber")) {
            continue;
        } else if (!strcmp(a, "--iface")) {
            iface = arg(argc, argv, &i);
        } else if (!strcmp(a, "--app-id")) {
            const char* v = arg(argc, argv, &i);
            if (!goose_parse_appid(v, &g_app_id))
                jl_fatal(1, "goose-subscriber: --app-id %s is not a 16-bit number", v);
            g_have_app_id = true;
        } else if (!strcmp(a, "--gocb-ref")) {
            g_gocb_filter = arg(argc, argv, &i);
        } else if (!strcmp(a, "--dst")) {
            const char* v = arg(argc, argv, &i);
            if (!goose_parse_mac(v, g_dst_filter))
                jl_fatal(1, "goose-subscriber: --dst %s is not a MAC address", v);
            g_have_dst = true;
        } else {
            jl_fatal(1, "goose-subscriber: unknown argument %s", a);
        }
    }

    g_receiver = GooseReceiver_createEx(NULL);
    GooseSubscriber sub = GooseSubscriber_create("", NULL);
    GooseSubscriber_setObserver(sub);
    GooseSubscriber_setListener(sub, listener, NULL);
    GooseReceiver_addSubscriber(g_receiver, sub);

    signal(SIGTERM, sig_handler);
    signal(SIGINT, sig_handler);
    signal(SIGPIPE, SIG_IGN);

    if (rudp_is_udp(&ra, "goose-subscriber")) {
        char label[300];
        RSession session = rudp_open_receiver(&ra, "goose-subscriber", label, sizeof(label));
        uint8_t zero[6] = {0};
        goose_emit_ready("subscriber", label, zero, g_gocb_filter);
        while (g_running) {
            check_supervision();
            if (!rudp_poll(session, 10, handle_udp_frame, NULL))
                jl_fatal(1, "goose-subscriber: receive failed");
        }
        RSession_destroy(session);
        GooseReceiver_destroy(g_receiver);
        return 0;
    }

    EthernetSocket sock = Ethernet_createSocket(iface, NULL);
    if (!sock) jl_fatal(1, "goose-subscriber: raw socket on %s failed (needs CAP_NET_RAW)", iface);
    Ethernet_setProtocolFilter(sock, 0x88b8);

    /* Ethernet_receivePacket binds the socket to IF on its first call.
     * Until then an ETH_P_ALL socket queues frames from every interface
     * of the namespace (with --network host: each bridge port as well), so
     * bind now and drop whatever arrived before. */
    uint8_t frame[FRAME_MAX];
    while (Ethernet_receivePacket(sock, frame, sizeof(frame)) > 0)
        ;

    uint8_t own[6];
    Ethernet_getInterfaceMACAddress(iface, own);
    goose_emit_ready("subscriber", iface, own, g_gocb_filter);

    EthernetHandleSet handles = EthernetHandleSet_new();
    EthernetHandleSet_addSocket(handles, sock);

    while (g_running) {
        check_supervision();
        if (EthernetHandleSet_waitReady(handles, 10) <= 0)
            continue;
        int n;
        while ((n = Ethernet_receivePacket(sock, frame, sizeof(frame))) > 0)
            handle_frame(frame, n);
    }

    EthernetHandleSet_destroy(handles);
    GooseReceiver_destroy(g_receiver);
    Ethernet_destroySocket(sock);
    return 0;
}
