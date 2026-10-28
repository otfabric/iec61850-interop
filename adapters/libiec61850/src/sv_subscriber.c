/* SPDX-License-Identifier: MIT */
/*
 * sv_subscriber.c — libIEC61850 Sampled Values subscriber adapter for
 * iec61850-interop (contract: docs/sv.md).
 *
 * Usage:
 *   libiec61850-sv-subscriber --iface IF [--app-id 0xNNNN] [--sv-id ID]
 *       [--dst MAC] [--sample-every N] [--fixture PATH]
 *
 * Reads SV frames from a raw socket on IF, hands each to
 * SVReceiver_handleL2Message and reports, for every ASDU the library
 * decodes whose smpCnt is a multiple of --sample-every,
 * {"event":"sv",...}. seqData is decoded with the members of the
 * fixture stream (sv.json) whose svID matches. Once per second it emits
 * {"event":"stats",...} with the loss counts of docs/sv.md.
 *
 * The adapter reads the frame itself (as the GOOSE subscriber does) for
 * what the SVSubscriber API does not give: the Ethernet addresses, the S
 * bit of Reserved1, noASDU and the position of an ASDU in its frame.
 * libiec61850 drops an ASDU whose smpMod is not one octet long (its own
 * publisher writes two): such ASDUs are never reported or counted.
 *
 * Exit codes: 0 on signal, 1 argument or socket error.
 */

#include <signal.h>
#include <sys/socket.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "sv_subscriber.h"
#include "hal_ethernet.h"
#include "hal_time.h"

#include "sv_common.h"
#include "jsonlines.h"

#define MAX_STREAMS  64
#define MAX_FIXTURES 16
#define MAX_MEMBERS  64
#define FRAME_MAX    2048

static volatile sig_atomic_t g_running = 1;
static void sig_handler(int sig) { (void) sig; g_running = 0; }

/* A stream of the fixture: how to decode its seqData. */
typedef struct {
    char     sv_id[130];
    SvType   types[MAX_MEMBERS];
    int      count;
    uint32_t wrap;
} FixtureStream;

/* Loss accounting per (APPID, svID). */
typedef struct {
    uint16_t app_id;
    char     sv_id[130];
    bool     has_last;
    uint32_t last, max_seen, wrap;   /* wrap 0: learned from max_seen */
    const FixtureStream* fixture;
} Stream;

static FixtureStream g_fixtures[MAX_FIXTURES];
static int           g_fixture_count;
static Stream        g_streams[MAX_STREAMS];
static int           g_stream_count;
static uint64_t      g_received, g_lost, g_duplicates, g_out_of_order;

static const char* g_sv_filter;
static long        g_every = 1;

/* The frame being decoded. */
static uint8_t  g_dst[6], g_src[6];
static uint16_t g_app_id;
static bool     g_simulation;
static int      g_no_asdu, g_asdu_index;

static void load_fixture(const char* path)
{
    struct json_object* root = json_object_from_file(path);
    struct json_object* streams = NULL;
    if (!root || !json_object_object_get_ex(root, "streams", &streams)) {
        jl_log("sv-subscriber: no fixture streams in %s; seqData is reported raw", path);
        if (root) json_object_put(root);
        return;
    }
    json_object_object_foreach(streams, name, s) {
        (void) name;
        if (g_fixture_count == MAX_FIXTURES) break;
        FixtureStream* f = &g_fixtures[g_fixture_count];
        struct json_object *jid = NULL, *jmembers = NULL, *jwrap = NULL;
        if (!json_object_object_get_ex(s, "svID", &jid) ||
            !json_object_object_get_ex(s, "members", &jmembers))
            continue;
        snprintf(f->sv_id, sizeof(f->sv_id), "%s", json_object_get_string(jid));
        f->wrap = json_object_object_get_ex(s, "smpCntWrap", &jwrap) ? (uint32_t) json_object_get_int(jwrap) : 0;
        f->count = 0;
        bool ok = true;
        for (size_t i = 0; i < json_object_array_length(jmembers) && f->count < MAX_MEMBERS; i++) {
            struct json_object *m = json_object_array_get_idx(jmembers, i), *jt = NULL;
            if (!json_object_object_get_ex(m, "type", &jt) ||
                !sv_type_from_name(json_object_get_string(jt), &f->types[f->count++]))
                ok = false;
        }
        if (ok) g_fixture_count++;
    }
    json_object_put(root);
}

static const FixtureStream* fixture_for(const char* sv_id)
{
    for (int i = 0; i < g_fixture_count; i++)
        if (!strcmp(g_fixtures[i].sv_id, sv_id)) return &g_fixtures[i];
    return NULL;
}

static Stream* stream_for(uint16_t app_id, const char* sv_id)
{
    for (int i = 0; i < g_stream_count; i++)
        if (g_streams[i].app_id == app_id && !strcmp(g_streams[i].sv_id, sv_id)) return &g_streams[i];
    if (g_stream_count == MAX_STREAMS) return NULL;
    Stream* s = &g_streams[g_stream_count++];
    memset(s, 0, sizeof(*s));
    s->app_id = app_id;
    snprintf(s->sv_id, sizeof(s->sv_id), "%s", sv_id);
    s->fixture = fixture_for(sv_id);
    s->wrap = s->fixture ? s->fixture->wrap : 0;
    return s;
}

/* The counting rules of docs/sv.md. */
static void count(Stream* s, uint32_t smp_cnt)
{
    g_received++;
    if (!s->has_last) {
        s->has_last = true;
        s->last = s->max_seen = smp_cnt;
        return;
    }
    if (smp_cnt > s->max_seen) s->max_seen = smp_cnt;
    int64_t d = (int64_t) smp_cnt - (int64_t) s->last;
    if (d == 1) {
        s->last = smp_cnt;
    } else if (d == 0) {
        g_duplicates++;
    } else if (d > 1) {
        g_lost += (uint64_t) (d - 1);
        s->last = smp_cnt;
    } else {
        uint32_t wrap = s->wrap ? s->wrap : s->max_seen + 1;
        if ((uint32_t) (s->last - smp_cnt) > wrap / 2) {
            g_lost += (uint64_t) (wrap - 1 - s->last) + smp_cnt;
            s->last = smp_cnt;
        } else {
            g_out_of_order++;
        }
    }
}

static struct json_object* decode_values(SVSubscriber_ASDU asdu, const FixtureStream* f)
{
    int size = SVSubscriber_ASDU_getDataSize(asdu);
    int need = 0;
    for (int i = 0; i < f->count; i++) need += sv_type_size(f->types[i]);
    if (need > size) return NULL;

    struct json_object* arr = json_object_new_array();
    int off = 0;
    for (int i = 0; i < f->count; i++) {
        MmsValue* v = NULL;
        switch (f->types[i]) {
        case SV_INT32:   v = MmsValue_newIntegerFromInt32(SVSubscriber_ASDU_getINT32(asdu, off)); break;
        case SV_INT32U:  v = MmsValue_newUnsignedFromUint32(SVSubscriber_ASDU_getINT32U(asdu, off)); break;
        case SV_FLOAT32: v = MmsValue_newFloat(SVSubscriber_ASDU_getFLOAT32(asdu, off)); break;
        case SV_FLOAT64: v = MmsValue_newDouble(SVSubscriber_ASDU_getFLOAT64(asdu, off)); break;
        case SV_BOOLEAN: v = MmsValue_newBoolean(SVSubscriber_ASDU_getINT8U(asdu, off) != 0); break;
        case SV_QUALITY: v = sv_quality_to_value(SVSubscriber_ASDU_getQuality(asdu, off)); break;
        case SV_TIMESTAMP: {
            Timestamp ts = SVSubscriber_ASDU_getTimestamp(asdu, off);
            v = MmsValue_newUtcTimeByMsTime(Timestamp_getTimeInMs(&ts));
            break;
        }
        }
        json_object_array_add(arr, goose_value_to_json(v));
        MmsValue_delete(v);
        off += sv_type_size(f->types[i]);
    }
    return arr;
}

static void listener(SVSubscriber sub, void* param, SVSubscriber_ASDU asdu)
{
    (void) sub;
    (void) param;
    int index = g_asdu_index++;
    const char* sv_id = SVSubscriber_ASDU_getSvId(asdu);
    if (!sv_id) sv_id = "";
    if (g_sv_filter && strcmp(sv_id, g_sv_filter) != 0)
        return;

    uint32_t smp_cnt = SVSubscriber_ASDU_getSmpCnt(asdu);
    Stream* s = stream_for(g_app_id, sv_id);
    if (s) count(s, smp_cnt);
    if (smp_cnt % (uint32_t) g_every != 0)
        return;

    char mac[18], appid[8], t[48];
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("sv"));
    snprintf(appid, sizeof(appid), "0x%04x", g_app_id);
    json_object_object_add(o, "appID", json_object_new_string(appid));
    goose_format_mac(g_dst, mac);
    json_object_object_add(o, "dst", json_object_new_string(mac));
    goose_format_mac(g_src, mac);
    json_object_object_add(o, "src", json_object_new_string(mac));
    json_object_object_add(o, "svID", json_object_new_string(sv_id));
    json_object_object_add(o, "datSet", SVSubscriber_ASDU_hasDatSet(asdu)
        ? json_object_new_string(SVSubscriber_ASDU_getDatSet(asdu)) : NULL);
    json_object_object_add(o, "smpCnt", json_object_new_int64(smp_cnt));
    json_object_object_add(o, "confRev", json_object_new_int64(SVSubscriber_ASDU_getConfRev(asdu)));
    if (SVSubscriber_ASDU_hasRefrTm(asdu)) {
        sv_format_ns(SVSubscriber_ASDU_getRefrTmAsNs(asdu), t);
        json_object_object_add(o, "refrTm", json_object_new_string(t));
    } else {
        json_object_object_add(o, "refrTm", NULL);
    }
    json_object_object_add(o, "smpSynch", json_object_new_int(SVSubscriber_ASDU_getSmpSynch(asdu)));
    json_object_object_add(o, "smpRate", SVSubscriber_ASDU_hasSmpRate(asdu)
        ? json_object_new_int(SVSubscriber_ASDU_getSmpRate(asdu)) : NULL);
    const char* mod = SVSubscriber_ASDU_hasSmpMod(asdu)
        ? sv_smp_mod_name(SVSubscriber_ASDU_getSmpMod(asdu)) : NULL;
    json_object_object_add(o, "smpMod", mod ? json_object_new_string(mod) : NULL);
    json_object_object_add(o, "simulation", json_object_new_boolean(g_simulation));
    json_object_object_add(o, "asduIndex", json_object_new_int(index));
    json_object_object_add(o, "noASDU", json_object_new_int(g_no_asdu));

    struct json_object* values = s && s->fixture ? decode_values(asdu, s->fixture) : NULL;
    json_object_object_add(o, "values", values);
    if (!values) {
        int size = SVSubscriber_ASDU_getDataSize(asdu);
        char* hex = malloc((size_t) size * 2 + 1);
        for (int i = 0; i < size; i++)
            sprintf(hex + 2 * i, "%02x", SVSubscriber_ASDU_getINT8U(asdu, i));
        hex[size * 2] = '\0';
        json_object_object_add(o, "seqData", json_object_new_string(hex));
        free(hex);
    }
    goose_emit(o);
}

static void emit_stats(void)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("stats"));
    json_object_object_add(o, "received", json_object_new_int64((int64_t) g_received));
    json_object_object_add(o, "lost", json_object_new_int64((int64_t) g_lost));
    json_object_object_add(o, "duplicates", json_object_new_int64((int64_t) g_duplicates));
    json_object_object_add(o, "outOfOrder", json_object_new_int64((int64_t) g_out_of_order));
    goose_emit(o);
}

/* BER length at buf[*pos]; -1 when malformed. */
static int ber_length(const uint8_t* buf, int len, int* pos)
{
    if (*pos >= len) return -1;
    uint8_t b = buf[(*pos)++];
    if (b < 0x80) return b;
    int n = b & 0x7f, v = 0;
    if (n > 2 || *pos + n > len) return -1;
    while (n--) v = (v << 8) | buf[(*pos)++];
    return v;
}

/* noASDU of the savPdu at apdu[0..len); 0 when it cannot be read. */
static int read_no_asdu(const uint8_t* apdu, int len)
{
    int pos = 0;
    if (len < 4 || apdu[pos++] != 0x60 || ber_length(apdu, len, &pos) < 0) return 0;
    if (pos >= len || apdu[pos++] != 0x80) return 0;
    int n = ber_length(apdu, len, &pos), v = 0;
    if (n < 1 || n > 4 || pos + n > len) return 0;
    while (n--) v = (v << 8) | apdu[pos++];
    return v;
}

static const char* arg(int argc, char** argv, int* i)
{
    if (*i + 1 >= argc) jl_fatal(1, "sv-subscriber: %s needs a value", argv[*i]);
    return argv[++*i];
}

int main(int argc, char** argv)
{
    jl_handle_meta_flags(argc, argv, "libiec61850");

    const char *iface = "eth0", *fixture = SV_FIXTURE_PATH;
    uint16_t app_id = 0;
    bool have_app_id = false, have_dst = false;
    uint8_t dst[6];

    for (int i = 1; i < argc; i++) {
        const char* a = argv[i];
        if (!strcmp(a, "--iface")) {
            iface = arg(argc, argv, &i);
        } else if (!strcmp(a, "--app-id")) {
            const char* v = arg(argc, argv, &i);
            if (!goose_parse_appid(v, &app_id))
                jl_fatal(1, "sv-subscriber: --app-id %s is not a 16-bit number", v);
            have_app_id = true;
        } else if (!strcmp(a, "--sv-id")) {
            g_sv_filter = arg(argc, argv, &i);
        } else if (!strcmp(a, "--dst")) {
            const char* v = arg(argc, argv, &i);
            if (!goose_parse_mac(v, dst))
                jl_fatal(1, "sv-subscriber: --dst %s is not a MAC address", v);
            have_dst = true;
        } else if (!strcmp(a, "--sample-every")) {
            g_every = strtol(arg(argc, argv, &i), NULL, 0);
            if (g_every < 1) jl_fatal(1, "sv-subscriber: --sample-every must be >= 1");
        } else if (!strcmp(a, "--fixture")) {
            fixture = arg(argc, argv, &i);
        } else {
            jl_fatal(1, "sv-subscriber: unknown argument %s", a);
        }
    }
    load_fixture(fixture);

    EthernetSocket sock = Ethernet_createSocket(iface, NULL);
    if (!sock) jl_fatal(1, "sv-subscriber: raw socket on %s failed (needs CAP_NET_RAW)", iface);
    Ethernet_setProtocolFilter(sock, 0x88ba);

    /* The default receive buffer holds well under 100 ms of a 4000 frames/s
     * stream; ask for 32 MiB (the kernel caps it at net.core.rmem_max). The
     * descriptor is the first member of struct sEthernetSocket
     * (hal/ethernet/linux/ethernet_linux.c). */
    int rcvbuf = 32 << 20;
    setsockopt(*(int*) sock, SOL_SOCKET, SO_RCVBUF, &rcvbuf, sizeof(rcvbuf));

    /* Bind now and drop what an unbound ETH_P_ALL socket queued from other
     * interfaces (see goose_subscriber.c). */
    static uint8_t frame[FRAME_MAX];
    while (Ethernet_receivePacket(sock, frame, sizeof(frame)) > 0)
        ;

    /* SVSubscriber matches one APPID: one per APPID seen, created on demand. */
    SVReceiver receiver = SVReceiver_create();
    SVReceiver_disableDestAddrCheck(receiver);
    uint16_t known[MAX_STREAMS];
    int known_count = 0;

    signal(SIGTERM, sig_handler);
    signal(SIGINT, sig_handler);
    signal(SIGPIPE, SIG_IGN);

    uint8_t own[6];
    Ethernet_getInterfaceMACAddress(iface, own);
    sv_emit_ready("subscriber", iface, own, g_sv_filter);
    uint64_t next_stats = Hal_getMonotonicTimeInMs() + 1000;

    EthernetHandleSet handles = EthernetHandleSet_new();
    EthernetHandleSet_addSocket(handles, sock);

    while (g_running) {
        uint64_t now = Hal_getMonotonicTimeInMs();
        if (now >= next_stats) {
            emit_stats();
            next_stats += 1000;
        }
        if (EthernetHandleSet_waitReady(handles, 10) <= 0)
            continue;
        int n;
        while ((n = Ethernet_receivePacket(sock, frame, sizeof(frame))) > 0) {
            int off = 12;
            if (n < 14) continue;
            if (frame[12] == 0x81 && frame[13] == 0x00) off += 4;
            if (n < off + 10 || frame[off] != 0x88 || frame[off + 1] != 0xba) continue;
            int hdr = off + 2;
            uint16_t frame_app_id = (uint16_t) (frame[hdr] << 8 | frame[hdr + 1]);
            if (have_app_id && frame_app_id != app_id) continue;
            if (have_dst && memcmp(frame, dst, 6) != 0) continue;

            bool registered = false;
            for (int i = 0; i < known_count && !registered; i++) registered = known[i] == frame_app_id;
            if (!registered && known_count < MAX_STREAMS) {
                SVSubscriber sub = SVSubscriber_create(NULL, frame_app_id);
                SVSubscriber_setListener(sub, listener, NULL);
                SVReceiver_addSubscriber(receiver, sub);
                known[known_count++] = frame_app_id;
            }

            memcpy(g_dst, frame, 6);
            memcpy(g_src, frame + 6, 6);
            g_app_id = frame_app_id;
            g_simulation = (frame[hdr + SV_SIM_OCTET] & SV_SIM_MASK) != 0;
            g_no_asdu = read_no_asdu(frame + hdr + 8, n - hdr - 8);
            g_asdu_index = 0;
            SVReceiver_handleL2Message(receiver, frame, n);
        }
    }

    EthernetHandleSet_destroy(handles);
    SVReceiver_destroy(receiver);
    Ethernet_destroySocket(sock);
    return 0;
}
