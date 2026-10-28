/* SPDX-License-Identifier: MIT */
/*
 * sv_publisher.c — libIEC61850 Sampled Values publisher adapter for
 * iec61850-interop (contract: docs/sv.md).
 *
 * Usage:
 *   libiec61850-sv-publisher --iface IF --app-id 0xNNNN --dst MAC
 *       --vlan-id N --vlan-prio N --sv-id ID --dataset REF --conf-rev N
 *       --smp-rate N --smp-mod samples-per-period|samples-per-second|seconds-per-sample
 *       --no-asdu N [--smp-synch 0|1|2] [--simulation] [--profile 9-2LE|generic]
 *       [--frequency HZ] [--report-every N] --values JSON
 *
 * Lifecycle:
 *   1. Create an SVPublisher on IF (raw socket, needs CAP_NET_RAW) with
 *      --no-asdu ASDUs whose seqData members follow the types of --values.
 *   2. Emit {"event":"ready",...}.
 *   3. Publish one frame every noASDU samples, paced against absolute
 *      CLOCK_MONOTONIC deadlines; each ASDU is the next smpCnt, wrapping at
 *      the samples per second (65536 for seconds-per-sample).
 *   4. Read stdin JSON Lines: {"cmd":"set","values":[...]} replaces the
 *      values from the next frame on; {"cmd":"stop"} emits
 *      {"event":"stopped",...} and exits 0. EOF keeps publishing until
 *      SIGTERM/SIGINT, which also emit stopped.
 *
 * Emits {"event":"published","reason":R,"frames":F,"samples":S,"smpCnt":N}
 * for the first frame, the first frame after a set and each frame that
 * takes the sample total past a multiple of --report-every.
 *
 * The 9-2LE profile leaves out datSet, refrTm, smpRate and smpMod; the
 * generic profile sends all four. libiec61850 encodes smpMod in two
 * octets (SVPublisher_ASDU_setSmpMod).
 *
 * --simulation sets the Edition 2 S bit of Reserved1, for which
 * libiec61850 has no API: the publisher prebuilds the frame header once,
 * so the adapter sets the bit in that buffer after setup.
 *
 * Exit codes: 0 stop/signal, 1 argument or socket error.
 */

#define _GNU_SOURCE   /* ppoll */
#include <errno.h>
#include <poll.h>
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <time.h>
#include <unistd.h>

#include "sv_publisher.h"
#include "hal_ethernet.h"
#include "mms_value_internal.h"   /* float format width (private) */

#include "sv_common.h"
#include "jsonlines.h"

#define MAX_MEMBERS 64
#define MAX_ASDU    16

static volatile sig_atomic_t g_running = 1;
static void sig_handler(int sig) { (void) sig; g_running = 0; }

typedef struct {
    SvType type;
    int    index[MAX_ASDU];   /* offset of the member in each ASDU's seqData */
} Member;

typedef struct {
    SVPublisher      pub;
    SVPublisher_ASDU asdu[MAX_ASDU];
    int              no_asdu;
    Member           members[MAX_MEMBERS];
    int              member_count;
    bool             generic;
    uint32_t         wrap;
    uint32_t         smp_cnt;      /* smpCnt of the next ASDU */
    uint64_t         frames, samples;
    long             report_every;
    bool             after_set;
} Publisher;

static uint64_t mono_ns(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t) ts.tv_sec * 1000000000ULL + (uint64_t) ts.tv_nsec;
}

static uint64_t real_ns(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (uint64_t) ts.tv_sec * 1000000000ULL + (uint64_t) ts.tv_nsec;
}

static bool member_type(const MmsValue* v, SvType* out)
{
    switch (MmsValue_getType(v)) {
    case MMS_INTEGER:    *out = SV_INT32; return true;
    case MMS_UNSIGNED:   *out = SV_INT32U; return true;
    case MMS_FLOAT:      *out = v->value.floatingPoint.formatWidth == 64 ? SV_FLOAT64 : SV_FLOAT32; return true;
    case MMS_BOOLEAN:    *out = SV_BOOLEAN; return true;
    case MMS_BIT_STRING: *out = SV_QUALITY; return true;
    case MMS_UTC_TIME:   *out = SV_TIMESTAMP; return true;
    default:             return false;
    }
}

/* Check values against the member layout (or define it, when layout is
 * true). NULL on success, else a static error string. */
static const char* check_values(Publisher* p, LinkedList values, bool layout)
{
    int n = LinkedList_size(values);
    if (n > MAX_MEMBERS) return "too many values";
    if (!layout && n != p->member_count) return "value count differs from --values";
    int i = 0;
    for (LinkedList e = LinkedList_getNext(values); e; e = LinkedList_getNext(e), i++) {
        SvType t;
        if (!member_type((MmsValue*) LinkedList_getData(e), &t))
            return "type cannot be carried in SV (integer, unsigned, float32, float64, boolean, bit-string, utc-time)";
        if (layout) p->members[i].type = t;
        else if (p->members[i].type != t) return "value types differ from --values";
    }
    if (layout) p->member_count = n;
    return NULL;
}

static void write_values(Publisher* p, LinkedList values)
{
    int i = 0;
    for (LinkedList e = LinkedList_getNext(values); e; e = LinkedList_getNext(e), i++) {
        MmsValue* v = (MmsValue*) LinkedList_getData(e);
        for (int a = 0; a < p->no_asdu; a++) {
            SVPublisher_ASDU asdu = p->asdu[a];
            int idx = p->members[i].index[a];
            switch (p->members[i].type) {
            case SV_INT32:   SVPublisher_ASDU_setINT32(asdu, idx, MmsValue_toInt32(v)); break;
            case SV_INT32U:  SVPublisher_ASDU_setINT32(asdu, idx, (int32_t) MmsValue_toUint32(v)); break;
            case SV_FLOAT32: SVPublisher_ASDU_setFLOAT(asdu, idx, MmsValue_toFloat(v)); break;
            case SV_FLOAT64: SVPublisher_ASDU_setFLOAT64(asdu, idx, MmsValue_toDouble(v)); break;
            case SV_BOOLEAN: SVPublisher_ASDU_setINT8(asdu, idx, MmsValue_getBoolean(v) ? 1 : 0); break;
            case SV_QUALITY: SVPublisher_ASDU_setQuality(asdu, idx, sv_quality_from_value(v)); break;
            case SV_TIMESTAMP: {
                Timestamp ts;
                Timestamp_clearFlags(&ts);
                Timestamp_setTimeInMilliseconds(&ts, MmsValue_getUtcTimeInMs(v));
                SVPublisher_ASDU_setTimestamp(asdu, idx, ts);
                break;
            }
            }
        }
    }
}

static void emit_published(Publisher* p, const char* reason, uint32_t smp_cnt)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("published"));
    json_object_object_add(o, "reason", json_object_new_string(reason));
    json_object_object_add(o, "frames", json_object_new_int64((int64_t) p->frames));
    json_object_object_add(o, "samples", json_object_new_int64((int64_t) p->samples));
    json_object_object_add(o, "smpCnt", json_object_new_int64(smp_cnt));
    goose_emit(o);
}

static void emit_stopped(Publisher* p)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("stopped"));
    json_object_object_add(o, "frames", json_object_new_int64((int64_t) p->frames));
    json_object_object_add(o, "samples", json_object_new_int64((int64_t) p->samples));
    goose_emit(o);
}

static void send_frame(Publisher* p)
{
    uint64_t now = p->generic ? real_ns() : 0;
    uint32_t last = 0;
    for (int a = 0; a < p->no_asdu; a++) {
        SVPublisher_ASDU_setSmpCnt(p->asdu[a], (uint16_t) p->smp_cnt);
        if (p->generic) SVPublisher_ASDU_setRefrTmNs(p->asdu[a], now);
        last = p->smp_cnt;
        p->smp_cnt = (p->smp_cnt + 1) % p->wrap;
    }
    SVPublisher_publish(p->pub);

    uint64_t before = p->samples;
    p->frames++;
    p->samples += (uint64_t) p->no_asdu;
    if (p->frames == 1)
        emit_published(p, "start", last);
    else if (p->after_set)
        emit_published(p, "set", last);
    else if (p->samples / (uint64_t) p->report_every != before / (uint64_t) p->report_every)
        emit_published(p, "periodic", last);
    p->after_set = false;
}

/* One stdin line. Returns false on {"cmd":"stop"}. */
static bool handle_line(Publisher* p, const char* line)
{
    struct json_object* cmd = json_tokener_parse(line);
    if (!cmd) {
        goose_emit_error("stdin: not JSON");
        return true;
    }
    struct json_object *jc = NULL, *jv = NULL;
    const char* c = json_object_object_get_ex(cmd, "cmd", &jc) ? json_object_get_string(jc) : "";
    bool keep = true;
    if (!strcmp(c, "set")) {
        const char* err = NULL;
        json_object_object_get_ex(cmd, "values", &jv);
        LinkedList values = goose_values_from_json(jv, &err);
        if (values && !(err = check_values(p, values, false))) {
            write_values(p, values);
            p->after_set = true;
        } else {
            char msg[200];
            snprintf(msg, sizeof(msg), "set: %s", err ? err : "values");
            goose_emit_error(msg);
        }
        if (values)
            LinkedList_destroyDeep(values, (LinkedListValueDeleteFunction) MmsValue_delete);
    } else if (!strcmp(c, "stop")) {
        keep = false;
    } else {
        char msg[160];
        snprintf(msg, sizeof(msg), "stdin: unknown cmd \"%.100s\"", c);
        goose_emit_error(msg);
    }
    json_object_put(cmd);
    return keep;
}

static const char* arg(int argc, char** argv, int* i)
{
    if (*i + 1 >= argc) jl_fatal(1, "sv-publisher: %s needs a value", argv[*i]);
    return argv[++*i];
}

int main(int argc, char** argv)
{
    jl_handle_meta_flags(argc, argv, "libiec61850");

    const char *iface = "eth0", *sv_id = NULL, *dataset = NULL, *values_json = NULL,
               *app_id_s = "0x4000", *dst_s = "01:0c:cd:04:00:00",
               *smp_mod_s = "samples-per-period", *profile = "9-2LE";
    long vlan_id = 0, vlan_prio = 4, conf_rev = 1, smp_rate = 80, no_asdu = 1,
         smp_synch = 0, frequency = 50, report_every = 1000;
    bool simulation = false;

    for (int i = 1; i < argc; i++) {
        const char* a = argv[i];
        if      (!strcmp(a, "--iface"))        iface = arg(argc, argv, &i);
        else if (!strcmp(a, "--app-id"))       app_id_s = arg(argc, argv, &i);
        else if (!strcmp(a, "--dst"))          dst_s = arg(argc, argv, &i);
        else if (!strcmp(a, "--vlan-id"))      vlan_id = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--vlan-prio"))    vlan_prio = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--sv-id"))        sv_id = arg(argc, argv, &i);
        else if (!strcmp(a, "--dataset"))      dataset = arg(argc, argv, &i);
        else if (!strcmp(a, "--conf-rev"))     conf_rev = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--smp-rate"))     smp_rate = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--smp-mod"))      smp_mod_s = arg(argc, argv, &i);
        else if (!strcmp(a, "--no-asdu"))      no_asdu = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--smp-synch"))    smp_synch = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--profile"))      profile = arg(argc, argv, &i);
        else if (!strcmp(a, "--frequency"))    frequency = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--report-every")) report_every = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--values"))       values_json = arg(argc, argv, &i);
        else if (!strcmp(a, "--simulation"))   simulation = true;
        else jl_fatal(1, "sv-publisher: unknown argument %s", a);
    }
    if (!sv_id) jl_fatal(1, "sv-publisher: --sv-id is required");
    if (!values_json) jl_fatal(1, "sv-publisher: --values is required");
    uint8_t smp_mod;
    if (!sv_smp_mod_from_name(smp_mod_s, &smp_mod))
        jl_fatal(1, "sv-publisher: --smp-mod %s is not samples-per-period, samples-per-second or seconds-per-sample", smp_mod_s);
    bool generic = !strcmp(profile, "generic");
    if (!generic && strcmp(profile, "9-2LE"))
        jl_fatal(1, "sv-publisher: --profile %s is not 9-2LE or generic", profile);
    if (vlan_id < 0 || vlan_id > 4095 || vlan_prio < 0 || vlan_prio > 7 || conf_rev < 0 ||
        smp_rate < 1 || smp_rate > 65535 || no_asdu < 1 || no_asdu > MAX_ASDU ||
        smp_synch < 0 || smp_synch > 255 || frequency < 1 || report_every < 1)
        jl_fatal(1, "sv-publisher: --vlan-id 0..4095, --vlan-prio 0..7, --smp-rate 1..65535, "
                    "--no-asdu 1..%d, --smp-synch 0..255, --frequency and --report-every >= 1", MAX_ASDU);

    Publisher p;
    memset(&p, 0, sizeof(p));
    p.no_asdu = (int) no_asdu;
    p.generic = generic;
    p.report_every = report_every;

    struct json_object* jvalues = json_tokener_parse(values_json);
    const char* err = NULL;
    LinkedList values = goose_values_from_json(jvalues, &err);
    json_object_put(jvalues);
    if (!values) jl_fatal(1, "sv-publisher: --values: %s", err ? err : "not JSON");
    if ((err = check_values(&p, values, true)))
        jl_fatal(1, "sv-publisher: --values: %s", err);
    if (!generic) {
        bool ok = p.member_count == 16;
        for (int i = 0; ok && i < 16; i++)
            ok = p.members[i].type == (i % 2 ? SV_QUALITY : SV_INT32);
        if (!ok) jl_fatal(1, "sv-publisher: --values: 9-2LE needs 16 values, integer and bit-string alternating");
    }

    /* Samples per second; smpCnt wraps there (65536 for seconds-per-sample). */
    double rate = smp_mod == 0 ? (double) smp_rate * (double) frequency
                : smp_mod == 1 ? (double) smp_rate : 1.0 / (double) smp_rate;
    p.wrap = smp_mod == 2 ? 65536u : (uint32_t) (rate > 65536 ? 65536 : rate);
    uint64_t period_ns = (uint64_t) ((double) no_asdu * 1e9 / rate);

    CommParameters params;
    memset(&params, 0, sizeof(params));
    params.vlanId = (uint16_t) vlan_id;
    params.vlanPriority = (uint8_t) vlan_prio;
    if (!goose_parse_appid(app_id_s, &params.appId))
        jl_fatal(1, "sv-publisher: --app-id %s is not a 16-bit number", app_id_s);
    if (!goose_parse_mac(dst_s, params.dstAddress))
        jl_fatal(1, "sv-publisher: --dst %s is not a MAC address", dst_s);

    /* Always 802.1Q-tagged, as the GOOSE publisher. */
    p.pub = SVPublisher_createEx(&params, iface, true);
    if (!p.pub) jl_fatal(1, "sv-publisher: SVPublisher on %s failed (needs CAP_NET_RAW)", iface);

    for (int a = 0; a < p.no_asdu; a++) {
        p.asdu[a] = SVPublisher_addASDU(p.pub, sv_id, generic ? dataset : NULL, (uint32_t) conf_rev);
        for (int i = 0; i < p.member_count; i++) {
            switch (p.members[i].type) {
            case SV_INT32:
            case SV_INT32U:    p.members[i].index[a] = SVPublisher_ASDU_addINT32(p.asdu[a]); break;
            case SV_FLOAT32:   p.members[i].index[a] = SVPublisher_ASDU_addFLOAT(p.asdu[a]); break;
            case SV_FLOAT64:   p.members[i].index[a] = SVPublisher_ASDU_addFLOAT64(p.asdu[a]); break;
            case SV_BOOLEAN:   p.members[i].index[a] = SVPublisher_ASDU_addINT8(p.asdu[a]); break;
            case SV_QUALITY:   p.members[i].index[a] = SVPublisher_ASDU_addQuality(p.asdu[a]); break;
            case SV_TIMESTAMP: p.members[i].index[a] = SVPublisher_ASDU_addTimestamp(p.asdu[a]); break;
            }
        }
        if (generic) {
            SVPublisher_ASDU_enableRefrTm(p.asdu[a]);
            SVPublisher_ASDU_setSmpRate(p.asdu[a], (uint16_t) smp_rate);
            SVPublisher_ASDU_setSmpMod(p.asdu[a], smp_mod);
        }
    }
    SVPublisher_setupComplete(p.pub);
    for (int a = 0; a < p.no_asdu; a++)
        SVPublisher_ASDU_setSmpSynch(p.asdu[a], (uint16_t) smp_synch);
    write_values(&p, values);
    LinkedList_destroyDeep(values, (LinkedListValueDeleteFunction) MmsValue_delete);

    if (simulation) {
        /* The frame buffer is the first member of struct sSVPublisher
         * (sv_publisher.c); Reserved1 follows the 802.1Q tag, EtherType,
         * APPID and Length: octet 12 + 4 + 2 + SV_SIM_OCTET. */
        uint8_t* frame = *(uint8_t**) p.pub;
        frame[12 + 4 + 2 + SV_SIM_OCTET] |= SV_SIM_MASK;
    }

    uint8_t src[6];
    Ethernet_getInterfaceMACAddress(iface, src);

    signal(SIGTERM, sig_handler);
    signal(SIGINT, sig_handler);
    signal(SIGPIPE, SIG_IGN);
    /* Wake-ups to the microsecond: a 4000 frames/s stream has 250 us. */
    prctl(PR_SET_TIMERSLACK, 1UL, 0, 0, 0);
    /* Stay on one CPU: a veth queues each frame on the sending CPU, so a
     * thread that moves between two frames can reorder them. */
    cpu_set_t cpus;
    CPU_ZERO(&cpus);
    CPU_SET(sched_getcpu(), &cpus);
    sched_setaffinity(0, sizeof(cpus), &cpus);

    sv_emit_ready("publisher", iface, src, sv_id);

    char line[65536];
    size_t used = 0;
    bool stdin_open = true;
    uint64_t next = mono_ns();
    while (g_running) {
        uint64_t now = mono_ns();
        if (now >= next) {
            send_frame(&p);
            next += period_ns;
            if (now > next + 1000000000ULL) {
                jl_log("sv-publisher: more than 1 s behind schedule, resynchronising");
                next = now + period_ns;
            }
        }
        /* Sleep until the next frame, or until stdin has a line. */
        struct pollfd pf = { STDIN_FILENO, POLLIN, 0 };
        uint64_t wait_ns = next > mono_ns() ? next - mono_ns() : 0;
        struct timespec ts = { (time_t) (wait_ns / 1000000000ULL), (long) (wait_ns % 1000000000ULL) };
        int n = ppoll(stdin_open ? &pf : NULL, stdin_open ? 1 : 0, &ts, NULL);
        if (n <= 0 || !(pf.revents & (POLLIN | POLLHUP))) continue;

        ssize_t got = read(STDIN_FILENO, line + used, sizeof(line) - 1 - used);
        if (got <= 0) {
            if (got < 0 && errno == EINTR) continue;
            stdin_open = false;
            jl_log("sv-publisher: stdin closed; publishing until SIGTERM");
            continue;
        }
        used += (size_t) got;
        line[used] = '\0';
        char* start = line;
        char* nl;
        while ((nl = strchr(start, '\n')) != NULL) {
            *nl = '\0';
            if (*start && !handle_line(&p, start)) g_running = 0;
            start = nl + 1;
        }
        used = strlen(start);
        memmove(line, start, used + 1);
        if (used == sizeof(line) - 1) {
            goose_emit_error("stdin: line too long");
            used = 0;
        }
    }

    emit_stopped(&p);
    SVPublisher_destroy(p.pub);
    return 0;
}
