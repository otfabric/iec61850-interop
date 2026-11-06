/* SPDX-License-Identifier: MIT */
/*
 * ied_reporter.c — libIEC61850 IED client adapter for Phase 2C URCB testing.
 *
 * Phase 2C-b: libiec61850 IED client → go-iec61850 server (URCB reporting)
 *
 * Connects to the IED server at --host HOST --port PORT, enables URCB urcb01,
 * makes it send a report, waits for the report, emits it as JSON Lines to
 * stdout, then disables the URCB and disconnects (contract 1.3).
 *
 * Names are resolved from the server directory: the logical device is the
 * one named exactly "InteropLD", else the first whose name ends in
 * "InteropLD" (an IED-name-prefixed domain such as "InteropIEDInteropLD"),
 * else the first one listed. The URCB is LLN0.RP.urcb01 if present, else its
 * first indexed instance (urcb0101, IEC 61850-6). Line targets use the
 * resolved LD and the SCL name urcb01: "<LD>/LLN0.RP.urcb01".
 *
 * --trigger dchg (default): writes GGIO1.SPS1.stVal[ST] = !--sps1-initial to
 *   cause a data-change report (a server may refuse the ST write).
 * --trigger gi: writes the URCB's GI = true to cause a general-interrogation
 *   report of both dsInterop members.
 *
 * Operation sequence:
 *   1. get-rcb          — read URCB attributes from server
 *   2. enable-rcb       — write RptEna=true
 *   3. write (ST)       — <LD>/GGIO1.SPS1.stVal = !initial (dchg), or
 *      trigger-gi       — <LD>/LLN0.RP.urcb01 GI=true (gi)
 *   4. receive-report   — wait for InformationReport (10 s timeout)
 *   5. disable-rcb      — write RptEna=false
 *   6. conclude         — close connection
 *
 * JSON output format (one object per line to stdout):
 *   {"operation":"get-rcb","target":"...","ok":true,"rptID":"...","rptEna":false}
 *   {"operation":"enable-rcb","target":"...","ok":true}
 *   {"operation":"write","target":"...","ok":true}            (dchg)
 *   {"operation":"trigger-gi","target":"...","ok":true}       (gi)
 *   {"operation":"receive-report","ok":true,"rptID":"...","seqNum":N,
 *    "inclusion":[true,false],"values":[<v>],"reasons":["data-change"]}
 *    (gi: "inclusion":[true,true],"values":[false,1],"reasons":["gi","gi"])
 *   {"operation":"disable-rcb","target":"...","ok":true}
 *   {"operation":"conclude","ok":true}
 *   {"operation":"<op>","ok":false,"error":"<msg>"}
 *
 * Exit codes:
 *   0  — all operations completed (individual results in stdout)
 *   1  — argument / startup error
 *   2  — connection failure
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <semaphore.h>
#include <time.h>

#include "iec61850_client.h"
#include "jsonlines.h"
#include "ld_resolve.h"

/* -------------------------------------------------------------------------
 * Constants — fixture-specific
 * ---------------------------------------------------------------------- */

#define RCB_NAME    "urcb01"      /* SCL RCB name; instances are urcb0101, ... */
#define RCB_RPTID   "interop_urcb01"
#define DS_SIZE     2   /* dsInterop: SPS1.stVal, Mod.stVal */
#define REF_MAX     130 /* object reference buffer (MMS 64 + '/' + 64 + NUL) */

/* ALL_RCB_ATTRIBUTES was added to the public API in a later libiec61850 release.
 * At the pinned SHA we sum all known RCB_ELEMENT_* bits explicitly. */
#ifndef ALL_RCB_ATTRIBUTES
#define ALL_RCB_ATTRIBUTES \
    (RCB_ELEMENT_RPT_ID | RCB_ELEMENT_RPT_ENA | RCB_ELEMENT_RESV | \
     RCB_ELEMENT_DATSET | RCB_ELEMENT_CONF_REV | RCB_ELEMENT_OPT_FLDS | \
     RCB_ELEMENT_BUF_TM | RCB_ELEMENT_SQ_NUM  | RCB_ELEMENT_TRG_OPS | \
     RCB_ELEMENT_INTG_PD | RCB_ELEMENT_GI | RCB_ELEMENT_PURGE_BUF | \
     RCB_ELEMENT_ENTRY_ID | RCB_ELEMENT_TIME_OF_ENTRY | \
     RCB_ELEMENT_RESV_TMS | RCB_ELEMENT_OWNER)
#endif

/* -------------------------------------------------------------------------
 * Shared report state (written from report callback, read from main)
 * ---------------------------------------------------------------------- */

typedef struct {
    sem_t       sem;
    bool        received;
    char        rptID[128];
    uint32_t    seqNum;
    /* Dataset members in order */
    bool        inclusion[DS_SIZE];
    /* Boolean value for member 0 (SPS1.stVal) if included */
    bool        val0;
    bool        val0_present;
    /* Integer value for member 1 (Mod.stVal) if included */
    int32_t     val1;
    bool        val1_present;
    /* Reason-for-inclusion per included member */
    ReasonForInclusion reasons[DS_SIZE];
    int         reason_count;
} ReportState;

/* -------------------------------------------------------------------------
 * JSON helpers
 * ---------------------------------------------------------------------- */

static void emit_error(const char* op, const char* target, const char* msg)
{
    if (target)
        printf("{\"operation\":\"%s\",\"target\":\"%s\",\"ok\":false,\"error\":\"%s\"}\n",
               op, target, msg);
    else
        printf("{\"operation\":\"%s\",\"ok\":false,\"error\":\"%s\"}\n", op, msg);
    fflush(stdout);
}

static const char* ied_error_str(IedClientError err)
{
    switch (err) {
    case IED_ERROR_OK:                    return "ok";
    case IED_ERROR_NOT_CONNECTED:         return "not-connected";
    case IED_ERROR_ALREADY_CONNECTED:     return "already-connected";
    case IED_ERROR_CONNECTION_LOST:       return "connection-lost";
    case IED_ERROR_SERVICE_NOT_SUPPORTED: return "service-not-supported";
    case IED_ERROR_TIMEOUT:               return "timeout";
    case IED_ERROR_ACCESS_DENIED:         return "access-denied";
    case IED_ERROR_OBJECT_DOES_NOT_EXIST: return "object-does-not-exist";
    case IED_ERROR_OBJECT_EXISTS:         return "object-exists";
    case IED_ERROR_TYPE_INCONSISTENT:     return "type-inconsistent";
    default:                              return "unknown-error";
    }
}

static const char* reason_str(ReasonForInclusion r)
{
    switch (r) {
    case IEC61850_REASON_DATA_CHANGE:    return "data-change";
    case IEC61850_REASON_QUALITY_CHANGE: return "quality-change";
    case IEC61850_REASON_DATA_UPDATE:    return "data-update";
    case IEC61850_REASON_INTEGRITY:      return "integrity";
    case IEC61850_REASON_GI:             return "gi";
    default:                             return "unknown";
    }
}

/* -------------------------------------------------------------------------
 * Report callback
 * ---------------------------------------------------------------------- */

static void reportCallback(void* param, ClientReport report)
{
    ReportState* st = (ReportState*)param;

    /* Basic fields */
    const char* id = ClientReport_getRptId(report);
    strncpy(st->rptID, id ? id : "", sizeof(st->rptID) - 1);
    st->seqNum = ClientReport_getSeqNum(report);

    /* Dataset values */
    MmsValue* dsValues = ClientReport_getDataSetValues(report);
    int n = (dsValues && (MmsValue_getType(dsValues) == MMS_STRUCTURE ||
                          MmsValue_getType(dsValues) == MMS_ARRAY))
            ? MmsValue_getArraySize(dsValues)
            : 0;
    if (n > DS_SIZE) n = DS_SIZE;

    st->reason_count = 0;
    for (int i = 0; i < DS_SIZE; i++) {
        ReasonForInclusion reason = ClientReport_getReasonForInclusion(report, i);
        st->inclusion[i] = (reason != IEC61850_REASON_NOT_INCLUDED);
        if (st->inclusion[i] && st->reason_count < DS_SIZE) {
            st->reasons[st->reason_count++] = reason;
        }
        if (!st->inclusion[i] || i >= n)
            continue;

        MmsValue* mval = MmsValue_getElement(dsValues, i);
        if (!mval) continue;

        if (i == 0 && MmsValue_getType(mval) == MMS_BOOLEAN) {
            st->val0 = MmsValue_getBoolean(mval);
            st->val0_present = true;
        } else if (i == 1 && MmsValue_getType(mval) == MMS_INTEGER) {
            st->val1 = MmsValue_toInt32(mval);
            st->val1_present = true;
        }
    }

    st->received = true;
    sem_post(&st->sem);
}

/* -------------------------------------------------------------------------
 * Emit the received report as a JSON Line
 * ---------------------------------------------------------------------- */

static void emit_report(const ReportState* st)
{
    printf("{\"operation\":\"receive-report\",\"ok\":true"
           ",\"rptID\":\"%s\",\"seqNum\":%u"
           ",\"inclusion\":[",
           st->rptID, st->seqNum);

    for (int i = 0; i < DS_SIZE; i++) {
        if (i > 0) printf(",");
        printf("%s", st->inclusion[i] ? "true" : "false");
    }
    printf("],\"values\":[");

    bool first_val = true;
    if (st->inclusion[0]) {
        printf("%s", st->val0_present ? (st->val0 ? "true" : "false") : "null");
        first_val = false;
    }
    if (st->inclusion[1]) {
        if (!first_val) printf(",");
        if (st->val1_present)
            printf("%d", st->val1);
        else
            printf("null");
    }

    printf("],\"reasons\":[");
    for (int i = 0; i < st->reason_count; i++) {
        if (i > 0) printf(",");
        printf("\"%s\"", reason_str(st->reasons[i]));
    }
    printf("]}\n");
    fflush(stdout);
}

/* -------------------------------------------------------------------------
 * Name resolution from the server directory
 * ---------------------------------------------------------------------- */

/* Resolve the URCB instance in <ld>/LLN0: RCB_NAME if present, else the
 * lowest indexed instance RCB_NAME + two digits (urcb0101). Leaves RCB_NAME
 * when nothing matches or the directory cannot be read. */
static void resolve_rcb(IedConnection conn, const char* ld,
                        char* rcb, size_t rcbSize)
{
    IedClientError err = IED_ERROR_OK;
    char lnRef[REF_MAX];
    snprintf(rcb, rcbSize, "%s", RCB_NAME);
    snprintf(lnRef, sizeof(lnRef), "%s/LLN0", ld);

    LinkedList list = IedConnection_getLogicalNodeDirectory(conn, &err, lnRef,
                                                            ACSI_CLASS_URCB);
    if (err != IED_ERROR_OK || list == NULL) {
        fprintf(stderr, "ied-reporter: URCB directory of %s: %s\n",
                lnRef, ied_error_str(err));
        if (list) LinkedList_destroy(list);
        return;
    }

    size_t base = strlen(RCB_NAME);
    const char* indexed = NULL;
    bool exact = false;
    for (LinkedList e = LinkedList_getNext(list); e; e = LinkedList_getNext(e)) {
        const char* name = (const char*)e->data;
        if (!name) continue;
        if (strcmp(name, RCB_NAME) == 0) {
            exact = true;
            break;
        }
        if (strlen(name) == base + 2 && strncmp(name, RCB_NAME, base) == 0 &&
            name[base] >= '0' && name[base] <= '9' &&
            name[base + 1] >= '0' && name[base + 1] <= '9' &&
            (!indexed || strcmp(name, indexed) < 0))
            indexed = name;
    }
    if (!exact && indexed) snprintf(rcb, rcbSize, "%s", indexed);
    LinkedList_destroy(list);
}

/* -------------------------------------------------------------------------
 * main
 * ---------------------------------------------------------------------- */

int main(int argc, char** argv)
{
    jl_handle_meta_flags(argc, argv, "libiec61850");
    const char* host     = "localhost";
    int         port     = 1102;
    bool        initial  = true; /* initial value of SPS1.stVal from values.json */
    bool        trig_gi  = false; /* --trigger gi: GI report instead of dchg */

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--host") && i + 1 < argc)
            host = argv[++i];
        else if (!strcmp(argv[i], "--port") && i + 1 < argc)
            port = atoi(argv[++i]);
        else if (!strcmp(argv[i], "--sps1-initial") && i + 1 < argc)
            initial = (atoi(argv[++i]) != 0);
        else if (!strcmp(argv[i], "--trigger") && i + 1 < argc) {
            const char* t = argv[++i];
            if (!strcmp(t, "dchg"))
                trig_gi = false;
            else if (!strcmp(t, "gi"))
                trig_gi = true;
            else {
                fprintf(stderr, "ied-reporter: invalid --trigger %s (dchg|gi)\n", t);
                return 1;
            }
        } else if (!strcmp(argv[i], "--trigger")) {
            fprintf(stderr, "ied-reporter: --trigger needs a value (dchg|gi)\n");
            return 1;
        }
    }
    if (port <= 0 || port > 65535) {
        fprintf(stderr, "ied-reporter: invalid port %d\n", port);
        return 1;
    }

    /* ------------------------------------------------------------------
     * Connect
     * ---------------------------------------------------------------- */
    IedConnection conn = IedConnection_create();
    IedClientError err = IED_ERROR_OK;

    IedConnection_connect(conn, &err, host, port);
    if (err != IED_ERROR_OK) {
        fprintf(stderr, "ied-reporter: connect %s:%d: %s\n",
                host, port, ied_error_str(err));
        IedConnection_destroy(conn);
        return 2;
    }

    /* ------------------------------------------------------------------
     * Resolve names from the server directory
     * ---------------------------------------------------------------- */
    char ld[65], rcbName[65];
    char rcbRef[REF_MAX];    /* reference of the RCB instance on the server */
    char rcbTarget[REF_MAX]; /* line target: <LD>/LLN0.RP.urcb01 */
    char sps1Ref[REF_MAX];
    ld_resolve(conn, "ied-reporter", ld, sizeof(ld));
    resolve_rcb(conn, ld, rcbName, sizeof(rcbName));
    snprintf(rcbRef, sizeof(rcbRef), "%s/LLN0.RP.%s", ld, rcbName);
    snprintf(rcbTarget, sizeof(rcbTarget), "%s/LLN0.RP.%s", ld, RCB_NAME);
    snprintf(sps1Ref, sizeof(sps1Ref), "%s/GGIO1.SPS1.stVal", ld);

    /* ------------------------------------------------------------------
     * 1. get-rcb — read current URCB attributes
     * ---------------------------------------------------------------- */
    /* Pass NULL to allocate a new ClientReportControlBlock.
     * ALL_RCB_ATTRIBUTES is a bitmask for setRCBValues, not getRCBValues. */
    ClientReportControlBlock rcb =
        IedConnection_getRCBValues(conn, &err, rcbRef, NULL);
    if (err != IED_ERROR_OK) {
        emit_error("get-rcb", rcbTarget, ied_error_str(err));
        IedConnection_close(conn);
        IedConnection_destroy(conn);
        return 0;
    }
    printf("{\"operation\":\"get-rcb\",\"target\":\"%s\",\"ok\":true"
           ",\"rptID\":\"%s\",\"rptEna\":%s}\n",
           rcbTarget,
           ClientReportControlBlock_getRptId(rcb) ? ClientReportControlBlock_getRptId(rcb) : "",
           ClientReportControlBlock_getRptEna(rcb) ? "true" : "false");
    fflush(stdout);

    /* ------------------------------------------------------------------
     * 2. enable-rcb — install handler and set RptEna=true
     * ---------------------------------------------------------------- */
    ReportState state;
    memset(&state, 0, sizeof(state));
    sem_init(&state.sem, 0, 0);

    /* Reports are matched on RptID: use the one the server holds (an
     * indexed instance may carry its own), the fixture's as fallback. */
    const char* rptID = ClientReportControlBlock_getRptId(rcb);
    if (!rptID || !*rptID) rptID = RCB_RPTID;
    IedConnection_installReportHandler(conn, rcbRef, rptID,
                                       reportCallback, &state);

    ClientReportControlBlock_setRptEna(rcb, true);
    IedConnection_setRCBValues(conn, &err, rcb, RCB_ELEMENT_RPT_ENA, true);
    if (err != IED_ERROR_OK) {
        emit_error("enable-rcb", rcbTarget, ied_error_str(err));
        ClientReportControlBlock_destroy(rcb);
        sem_destroy(&state.sem);
        IedConnection_close(conn);
        IedConnection_destroy(conn);
        return 0;
    }
    printf("{\"operation\":\"enable-rcb\",\"target\":\"%s\",\"ok\":true}\n",
           rcbTarget);
    fflush(stdout);

    if (trig_gi) {
        /* --------------------------------------------------------------
         * 3. trigger-gi — write GI=true on the URCB
         * ------------------------------------------------------------ */
        IedConnection_triggerGIReport(conn, &err, rcbRef);
        if (err != IED_ERROR_OK) {
            emit_error("trigger-gi", rcbTarget, ied_error_str(err));
            /* continue; we still want to disable the URCB */
        } else {
            printf("{\"operation\":\"trigger-gi\",\"target\":\"%s\",\"ok\":true}\n",
                   rcbTarget);
            fflush(stdout);
        }
    } else {
        /* --------------------------------------------------------------
         * 3. write — toggle SPS1.stVal to trigger dchg
         * ------------------------------------------------------------ */
        char sps1Target[REF_MAX + 4];
        snprintf(sps1Target, sizeof(sps1Target), "%s[ST]", sps1Ref);
        bool new_val = !initial;
        IedConnection_writeBooleanValue(conn, &err, sps1Ref, IEC61850_FC_ST, new_val);
        if (err != IED_ERROR_OK) {
            emit_error("write", sps1Target, ied_error_str(err));
            /* continue; we still want to disable the URCB */
        } else {
            printf("{\"operation\":\"write\",\"target\":\"%s\",\"ok\":true}\n",
                   sps1Target);
            fflush(stdout);
        }
    }

    /* ------------------------------------------------------------------
     * 4. receive-report — wait up to 10 s for the dchg or GI report
     * ---------------------------------------------------------------- */
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_sec += 10;

    if (sem_timedwait(&state.sem, &ts) == 0 && state.received) {
        emit_report(&state);
    } else {
        emit_error("receive-report", NULL, trig_gi
                   ? "timeout-waiting-for-gi-report"
                   : "timeout-waiting-for-dchg-report");
    }

    /* ------------------------------------------------------------------
     * 5. disable-rcb
     * ---------------------------------------------------------------- */
    ClientReportControlBlock_setRptEna(rcb, false);
    IedConnection_setRCBValues(conn, &err, rcb, RCB_ELEMENT_RPT_ENA, true);
    if (err != IED_ERROR_OK) {
        emit_error("disable-rcb", rcbTarget, ied_error_str(err));
    } else {
        printf("{\"operation\":\"disable-rcb\",\"target\":\"%s\",\"ok\":true}\n",
               rcbTarget);
        fflush(stdout);
    }

    ClientReportControlBlock_destroy(rcb);
    sem_destroy(&state.sem);

    /* ------------------------------------------------------------------
     * 6. conclude
     * ---------------------------------------------------------------- */
    IedConnection_close(conn);
    printf("{\"operation\":\"conclude\",\"ok\":true}\n");
    fflush(stdout);

    IedConnection_destroy(conn);
    return 0;
}
