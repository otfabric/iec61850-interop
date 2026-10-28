/* SPDX-License-Identifier: MIT */
/*
 * goose_publisher.c — libIEC61850 GOOSE publisher adapter for iec61850-interop.
 *
 * Usage:
 *   libiec61850-goose-publisher --iface IF --gocb-ref REF --go-id ID
 *       --dataset REF --app-id 0xNNNN --dst MAC --vlan-id N --vlan-prio N
 *       --conf-rev N --min-time ms --max-time ms [--test] [--simulation]
 *       [--nds-com] --values JSON
 *
 * Lifecycle:
 *   1. Create a GoosePublisher on IF (raw socket, needs CAP_NET_RAW).
 *   2. Emit {"event":"ready",...}.
 *   3. Publish --values as the first state (stNum 1, sqNum 0).
 *   4. Retransmit: after a state change the intervals are MinTime, then
 *      doubling, up to MaxTime, which repeats — the schedule of
 *      dscsystems' goose.RetransFromSCL, so both adapters behave alike.
 *      timeAllowedToLive of each frame is twice the interval to the next.
 *   5. Read stdin JSON Lines: {"cmd":"set","values":[...]} is a state
 *      change (GoosePublisher_increaseStNum, sqNum 0, schedule restarts);
 *      {"cmd":"stop"} exits 0. EOF keeps publishing until SIGTERM/SIGINT.
 *
 * Every frame emits {"event":"published","stNum":N,"sqNum":N,
 * "timeAllowedToLive":ms}. The raw GoosePublisher API sends one frame per
 * GoosePublisher_publish call; the retransmission schedule is this
 * program's (libiec61850 runs one only inside IedServer).
 *
 * --test sets goosePdu field [7] (GoosePublisher_setSimulation: Edition 1
 * "test", Edition 2 ASN.1 "simulation"). --simulation sets the Edition 2
 * S bit of Reserved1, for which libiec61850 has no API: the publisher is
 * then bound to "lo", each frame is taken with GoosePublisher_publishAndDump,
 * gets the bit and the source MAC of IF, and is sent on IF by the adapter.
 *
 * Exit codes: 0 stop/signal, 1 argument or socket error.
 */

#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/select.h>
#include <unistd.h>

#include "goose_publisher.h"
#include "hal_ethernet.h"
#include "hal_time.h"

#include "goose_common.h"
#include "jsonlines.h"

#define MAX_STEPS 17
#define FRAME_MAX 1600

static volatile sig_atomic_t g_running = 1;
static void sig_handler(int sig) { (void) sig; g_running = 0; }

typedef struct {
    GoosePublisher pub;
    LinkedList     values;
    uint32_t       st_num, sq_num;      /* mirrors the publisher's counters */
    uint32_t       steps[MAX_STEPS];    /* retransmission intervals, ms */
    int            step_count;
    int            step;                /* index of the next interval */
    uint64_t       next_ms;             /* monotonic deadline of the next frame */
    /* --simulation path */
    EthernetSocket raw;
    uint8_t        src[6];
    bool           vlan;
} Publisher;

/* MinTime, 2*MinTime, ... (< MaxTime), MaxTime — goose.RetransFromSCL. */
static int build_schedule(uint32_t min_ms, uint32_t max_ms, uint32_t* out)
{
    int n = 0;
    if (min_ms == 0 && max_ms == 0) { min_ms = 4; max_ms = 1000; }
    if (min_ms == 0) min_ms = max_ms / 64 ? max_ms / 64 : 1;
    if (max_ms < min_ms) max_ms = min_ms;
    for (uint32_t d = min_ms; d < max_ms && n < MAX_STEPS - 1; d *= 2)
        out[n++] = d;
    out[n++] = max_ms;
    return n;
}

static void emit_published(uint32_t st, uint32_t sq, uint32_t tal)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("published"));
    json_object_object_add(o, "stNum", json_object_new_int64(st));
    json_object_object_add(o, "sqNum", json_object_new_int64(sq));
    json_object_object_add(o, "timeAllowedToLive", json_object_new_int64(tal));
    goose_emit(o);
}

/* Send the current state once and schedule the next frame. */
static void send_frame(Publisher* p)
{
    int idx = p->step < p->step_count ? p->step : p->step_count - 1;
    uint32_t interval = p->steps[idx];
    uint32_t tal = 2 * interval;
    GoosePublisher_setTimeAllowedToLive(p->pub, tal);

    int rc;
    if (p->raw) {
        uint8_t frame[FRAME_MAX];
        int32_t len = 0;
        rc = GoosePublisher_publishAndDump(p->pub, p->values,
                                           (char*) frame, &len, sizeof(frame));
        if (rc == 0) {
            memcpy(frame + 6, p->src, 6);
            int apdu = 12 + (p->vlan ? 4 : 0) + 2;
            frame[apdu + GOOSE_SIM_OCTET] |= GOOSE_SIM_MASK;
            Ethernet_sendPacket(p->raw, frame, len);
        }
    } else {
        rc = GoosePublisher_publish(p->pub, p->values);
    }

    if (rc != 0) {
        goose_emit_error("publish: data set does not fit a GOOSE frame");
    } else {
        emit_published(p->st_num, p->sq_num, tal);
        p->sq_num++;
        if (p->sq_num == 0) p->sq_num = 1;
    }
    p->step++;
    p->next_ms = Hal_getMonotonicTimeInMs() + interval;
}

static void state_change(Publisher* p, LinkedList values)
{
    if (p->values)
        LinkedList_destroyDeep(p->values, (LinkedListValueDeleteFunction) MmsValue_delete);
    p->values = values;
    GoosePublisher_increaseStNum(p->pub);
    p->st_num++;
    if (p->st_num == 0) p->st_num = 1;
    p->sq_num = 0;
    p->step = 0;
    send_frame(p);
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
        if (!values) {
            char msg[160];
            snprintf(msg, sizeof(msg), "set: %s", err);
            goose_emit_error(msg);
        } else {
            state_change(p, values);
        }
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
    if (*i + 1 >= argc) jl_fatal(1, "goose-publisher: %s needs a value", argv[*i]);
    return argv[++*i];
}

int main(int argc, char** argv)
{
    jl_handle_meta_flags(argc, argv, "libiec61850");

    const char *iface = "eth0", *gocb_ref = NULL, *go_id = NULL, *dataset = "",
               *values_json = NULL, *app_id_s = "0x0000", *dst_s = "01:0c:cd:01:00:00";
    long vlan_id = 0, vlan_prio = 4, conf_rev = 1, min_time = 10, max_time = 1000;
    bool test = false, simulation = false, nds_com = false;

    for (int i = 1; i < argc; i++) {
        const char* a = argv[i];
        if      (!strcmp(a, "--iface"))      iface = arg(argc, argv, &i);
        else if (!strcmp(a, "--gocb-ref"))   gocb_ref = arg(argc, argv, &i);
        else if (!strcmp(a, "--go-id"))      go_id = arg(argc, argv, &i);
        else if (!strcmp(a, "--dataset"))    dataset = arg(argc, argv, &i);
        else if (!strcmp(a, "--app-id"))     app_id_s = arg(argc, argv, &i);
        else if (!strcmp(a, "--dst"))        dst_s = arg(argc, argv, &i);
        else if (!strcmp(a, "--vlan-id"))    vlan_id = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--vlan-prio"))  vlan_prio = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--conf-rev"))   conf_rev = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--min-time"))   min_time = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--max-time"))   max_time = strtol(arg(argc, argv, &i), NULL, 0);
        else if (!strcmp(a, "--values"))     values_json = arg(argc, argv, &i);
        else if (!strcmp(a, "--test"))       test = true;
        else if (!strcmp(a, "--simulation")) simulation = true;
        else if (!strcmp(a, "--nds-com"))    nds_com = true;
        else jl_fatal(1, "goose-publisher: unknown argument %s", a);
    }
    if (!gocb_ref) jl_fatal(1, "goose-publisher: --gocb-ref is required");
    if (!values_json) jl_fatal(1, "goose-publisher: --values is required");
    if (vlan_id < 0 || vlan_id > 4095 || vlan_prio < 0 || vlan_prio > 7 ||
        conf_rev < 0 || min_time < 0 || max_time < 0)
        jl_fatal(1, "goose-publisher: --vlan-id 0..4095, --vlan-prio 0..7, times and conf-rev >= 0");

    Publisher p;
    memset(&p, 0, sizeof(p));
    p.step_count = build_schedule((uint32_t) min_time, (uint32_t) max_time, p.steps);

    struct json_object* jvalues = json_tokener_parse(values_json);
    const char* err = NULL;
    p.values = goose_values_from_json(jvalues, &err);
    json_object_put(jvalues);
    if (!p.values) jl_fatal(1, "goose-publisher: --values: %s", err ? err : "not JSON");

    CommParameters params;
    memset(&params, 0, sizeof(params));
    params.vlanId = (uint16_t) vlan_id;
    params.vlanPriority = (uint8_t) vlan_prio;
    if (!goose_parse_appid(app_id_s, &params.appId))
        jl_fatal(1, "goose-publisher: --app-id %s is not a 16-bit number", app_id_s);
    if (!goose_parse_mac(dst_s, params.dstAddress))
        jl_fatal(1, "goose-publisher: --dst %s is not a MAC address", dst_s);

    Ethernet_getInterfaceMACAddress(iface, p.src);
    p.vlan = true;
    if (simulation) {
        p.raw = Ethernet_createSocket(iface, params.dstAddress);
        if (!p.raw) jl_fatal(1, "goose-publisher: raw socket on %s failed (needs CAP_NET_RAW)", iface);
    }
    /* Always 802.1Q-tagged, the libiec61850 default (GoosePublisher_create). */
    p.pub = GoosePublisher_createEx(&params, simulation ? "lo" : iface, true);
    if (!p.pub) jl_fatal(1, "goose-publisher: GoosePublisher on %s failed (needs CAP_NET_RAW)", iface);

    GoosePublisher_setGoCbRef(p.pub, (char*) gocb_ref);
    GoosePublisher_setGoID(p.pub, (char*) (go_id ? go_id : gocb_ref));
    GoosePublisher_setDataSetRef(p.pub, (char*) dataset);
    GoosePublisher_setConfRev(p.pub, (uint32_t) conf_rev);
    GoosePublisher_setSimulation(p.pub, test);
    GoosePublisher_setNeedsCommission(p.pub, nds_com);
    /* State 1 starts now: increaseStNum stamps t; stNum 0 -> 1. */
    GoosePublisher_setStNum(p.pub, 0);
    GoosePublisher_increaseStNum(p.pub);
    p.st_num = 1;
    p.sq_num = 0;

    signal(SIGTERM, sig_handler);
    signal(SIGINT, sig_handler);
    signal(SIGPIPE, SIG_IGN);

    goose_emit_ready("publisher", iface, p.src, gocb_ref);
    send_frame(&p);

    char line[65536];
    size_t used = 0;
    bool stdin_open = true;
    while (g_running) {
        uint64_t now = Hal_getMonotonicTimeInMs();
        if (now >= p.next_ms) {
            send_frame(&p);
            continue;
        }
        uint64_t wait = p.next_ms - now;
        struct timeval tv = { (time_t)(wait / 1000), (suseconds_t)((wait % 1000) * 1000) };
        fd_set rd;
        FD_ZERO(&rd);
        if (stdin_open) FD_SET(STDIN_FILENO, &rd);
        int n = select(stdin_open ? STDIN_FILENO + 1 : 0, &rd, NULL, NULL, &tv);
        if (n <= 0 || !FD_ISSET(STDIN_FILENO, &rd)) continue;

        ssize_t got = read(STDIN_FILENO, line + used, sizeof(line) - 1 - used);
        if (got <= 0) {
            if (got < 0 && errno == EINTR) continue;
            stdin_open = false;
            jl_log("goose-publisher: stdin closed; publishing until SIGTERM");
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

    GoosePublisher_destroy(p.pub);
    if (p.raw) Ethernet_destroySocket(p.raw);
    LinkedList_destroyDeep(p.values, (LinkedListValueDeleteFunction) MmsValue_delete);
    return 0;
}
