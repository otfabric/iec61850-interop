/* SPDX-License-Identifier: MIT */
/*
 * mms_server.c — libIEC61850 generic MMS server for iec61850-interop.
 *
 * Phase 0 finding: the generic MMS server requires private libiec61850
 * headers (see SPIKE.md).  Include paths are set by CMake.
 *
 * Lifecycle:
 *   1. Parse --fixture, --port and --filestore arguments.
 *   2. Load fixture JSON.
 *   3. Build MmsDevice → MmsDomain(s) → MmsVariableSpecification(s) and
 *      journals backed by an in-memory LogStorage.
 *   4. Create MmsServer, install write handler, set identity and VMD status.
 *   5. Pre-populate value cache (MmsServer_insertIntoCache).
 *   6. Write the fixture files into the file store and point the server at it.
 *   7. Start listening (threadless).
 *   8. Emit {"event":"ready","address":"0.0.0.0:<port>"} on stdout.
 *   9. Wait for SIGTERM/SIGINT.
 *  10. Stop and clean up.
 *
 * Services exercised beyond plain read/write:
 *   - alternate access writes: array element (array_idx) and structure
 *     component ("name$component") are applied to the cached value;
 *   - writing interop/report-trigger sends the fixture's informationReports
 *     to the writing connection;
 *   - ReadJournal over the fixture journals;
 *   - file services (directory, open/read/close, delete, rename, obtain)
 *     over the fixture files.
 *
 * Exit codes (REQ-3.6):
 *   0  — clean shutdown
 *   1  — startup / argument error
 *   3  — fixture load error
 */

#define _XOPEN_SOURCE 500 /* nftw, FTW_DEPTH, FTW_PHYS (glibc) */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>
#include <errno.h>
#include <ftw.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <unistd.h>

/* Public libiec61850 headers (installed) */
#include "mms_value.h"
#include "mms_server.h"
#include "mms_types.h"
#include "logging_api.h"
#include "linked_list.h"

/* Private libiec61850 headers (source-tree, see SPIKE.md) */
#include "mms_server_libinternal.h"   /* MmsServer_create, start/stop, handlers */
#include "mms_device_model.h"         /* MmsDevice_create, MmsDomain_create, struct sMmsDomain */

/* From the private mms_server_connection.h, which cannot be included here
 * because it pulls in the build-tree stack_config.h. */
void MmsServerConnection_sendInformationReportVMDSpecific(MmsServerConnection self,
        char* itemId, LinkedList values, bool handlerMode);

#include "fixture.h"
#include "jsonlines.h"

/* -------------------------------------------------------------------------
 * Global state
 * ---------------------------------------------------------------------- */

typedef struct {
    char*    domain_name;
    char*    var_name;
    bool     writable;
} WriteEntry;

static WriteEntry* g_entries  = NULL;
static int         g_nentries = 0;
static MmsServer   g_server   = NULL;
static MmsDevice*  g_device   = NULL;
static FixtureDef* g_fixture  = NULL;
static char        g_filestore[512];

static volatile sig_atomic_t g_running = 1;

static const char* REPORT_TRIGGER = "report-trigger";

/* -------------------------------------------------------------------------
 * Signal handler
 * ---------------------------------------------------------------------- */

static void on_signal(int sig)
{
    (void)sig;
    g_running = 0;
}

/* -------------------------------------------------------------------------
 * InformationReport
 * ---------------------------------------------------------------------- */

static MmsDomain* find_domain(const char* name)
{
    for (int i = 0; i < g_device->domainCount; i++) {
        if (strcmp(MmsDomain_getName(g_device->domains[i]), name) == 0)
            return g_device->domains[i];
    }
    return NULL;
}

/* split "domain/item" into its parts; returns false when malformed */
static bool split_ref(const char* ref, char* dom, size_t dom_len, char* item, size_t item_len)
{
    const char* slash = strchr(ref, '/');
    if (!slash || (size_t)(slash - ref) >= dom_len || strlen(slash + 1) >= item_len)
        return false;
    memcpy(dom, ref, slash - ref);
    dom[slash - ref] = 0;
    strcpy(item, slash + 1);
    return true;
}

/* Only named-list reports are sent. libiec61850 v1.6.1 under-sizes
 * listOfVariable reports with domain-specific names by 2 bytes
 * (mms_information_report.c omits the [1] domain-specific wrapper from the
 * length calculation), so conforming decoders reject them. See
 * knownLimitations in capabilities.json. */
static void send_reports(MmsServerConnection conn)
{
    for (int r = 0; r < g_fixture->report_count; r++) {
        ReportDef* rd = &g_fixture->reports[r];
        if (!rd->vmd_list) {
            jl_log("skipping listOfVariable informationReport %d (libiec61850 length bug)", r);
            continue;
        }
        LinkedList values = LinkedList_create();

        for (int i = 0; i < rd->var_count; i++) {
            char dom[65], item[65];
            if (!split_ref(rd->variables[i], dom, sizeof(dom), item, sizeof(item)))
                continue;
            MmsDomain* d = find_domain(dom);
            MmsValue* v = d ? MmsServer_getValueFromCache(g_server, d, item) : NULL;
            if (v)
                LinkedList_add(values, v); /* not owned: values stay in the cache */
        }

        MmsServerConnection_sendInformationReportVMDSpecific(conn, rd->vmd_list, values, true);
        jl_log("sent informationReport %d (%s)", r, rd->vmd_list);

        LinkedList_destroyStatic(values);
    }
}

/* -------------------------------------------------------------------------
 * Write handler
 *
 * Called by MmsServer for every confirmed Write request. var_id is
 * "name" or "name$component" (component alternate access); array_idx is
 * >= 0 for single array element alternate access. Index-range writes are
 * applied to the cache by libiec61850 itself without calling this handler.
 * ---------------------------------------------------------------------- */

static MmsDataAccessError write_handler(
    void*              param,
    MmsDomain*         domain,
    const char*        var_id,
    int                array_idx,
    const char*        component_id,
    MmsValue*          value,
    MmsServerConnection conn)
{
    (void)param; (void)component_id;

    const char* dom_name = MmsDomain_getName(domain);

    char base[130];
    snprintf(base, sizeof(base), "%s", var_id);
    char* dollar = strchr(base, '$');
    if (dollar) *dollar = 0;

    for (int i = 0; i < g_nentries; i++) {
        if (strcmp(g_entries[i].domain_name, dom_name) != 0 ||
            strcmp(g_entries[i].var_name, base) != 0)
            continue;

        if (!g_entries[i].writable)
            return DATA_ACCESS_ERROR_OBJECT_ACCESS_DENIED;

        MmsValue* cached = MmsServer_getValueFromCache(g_server, domain, var_id);
        if (!cached)
            return DATA_ACCESS_ERROR_OBJECT_UNDEFINED;

        if (array_idx >= 0) {
            cached = MmsValue_getElement(cached, array_idx);
            if (!cached)
                return DATA_ACCESS_ERROR_INVALID_ADDRESS;
        }

        if (!MmsValue_update(cached, value))
            return DATA_ACCESS_ERROR_TYPE_INCONSISTENT;

        if (strcmp(base, REPORT_TRIGGER) == 0)
            send_reports(conn);

        return DATA_ACCESS_ERROR_SUCCESS;
    }
    return DATA_ACCESS_ERROR_OBJECT_UNDEFINED;
}

/* -------------------------------------------------------------------------
 * In-memory journal storage
 *
 * libiec61850 reads journals through the LogStorage interface; this
 * read-only implementation serves the fixture entries. Each entry yields
 * one data variable; the journal service adds the ReasonCode variable.
 * ---------------------------------------------------------------------- */

static uint8_t g_encode_buf[256];

static void emit_entry(JournalEntryDef* e, LogEntryCallback entryCallback,
                       LogEntryDataCallback entryDataCallback, void* parameter)
{
    if (entryCallback && !entryCallback(parameter, e->time_ms, e->entry_id, true))
        return;
    int size = MmsValue_encodeMmsData(e->value, g_encode_buf, 0, true);
    if (entryDataCallback) {
        entryDataCallback(parameter, e->tag, g_encode_buf, size, e->reason_code, true);
        entryDataCallback(parameter, NULL, NULL, 0, 0, false);
    }
}

static bool mem_get_entries(LogStorage self, uint64_t start, uint64_t end,
                            LogEntryCallback entryCallback, LogEntryDataCallback entryDataCallback,
                            void* parameter)
{
    JournalDef* j = (JournalDef*)self->instanceData;
    for (int i = 0; i < j->entry_count; i++) {
        JournalEntryDef* e = &j->entries[i];
        if (e->time_ms >= start && e->time_ms <= end)
            emit_entry(e, entryCallback, entryDataCallback, parameter);
    }
    if (entryCallback)
        entryCallback(parameter, 0, 0, false);
    return true;
}

static bool mem_get_entries_after(LogStorage self, uint64_t start, uint64_t entry_id,
                                  LogEntryCallback entryCallback, LogEntryDataCallback entryDataCallback,
                                  void* parameter)
{
    JournalDef* j = (JournalDef*)self->instanceData;
    bool found = false;
    for (int i = 0; i < j->entry_count; i++) {
        JournalEntryDef* e = &j->entries[i];
        if (found && e->time_ms >= start)
            emit_entry(e, entryCallback, entryDataCallback, parameter);
        if (e->entry_id == entry_id)
            found = true;
    }
    if (entryCallback)
        entryCallback(parameter, 0, 0, false);
    return true;
}

static LogStorage mem_log_storage(JournalDef* j)
{
    LogStorage s = (LogStorage)calloc(1, sizeof(struct sLogStorage));
    s->instanceData    = j;
    s->getEntries      = mem_get_entries;
    s->getEntriesAfter = mem_get_entries_after;
    return s;
}

/* -------------------------------------------------------------------------
 * File store
 * ---------------------------------------------------------------------- */

static int mkdirs(char* path)
{
    for (char* p = path + 1; *p; p++) {
        if (*p == '/') {
            *p = 0;
            if (mkdir(path, 0755) != 0 && errno != EEXIST) { *p = '/'; return -1; }
            *p = '/';
        }
    }
    return 0;
}

static bool populate_filestore(FixtureDef* f, const char* dir)
{
    for (int i = 0; i < f->file_count; i++) {
        FileDef* fd = &f->files[i];
        char path[1024];
        snprintf(path, sizeof(path), "%s%s", dir, fd->name);
        if (mkdirs(path) != 0) return false;

        FILE* fp = fopen(path, "wb");
        if (!fp) return false;
        if (fd->size > 0 && fwrite(fd->data, 1, fd->size, fp) != (size_t)fd->size) {
            fclose(fp);
            return false;
        }
        fclose(fp);

        if (fd->last_modified_ms) {
            struct timeval tv[2];
            tv[0].tv_sec  = tv[1].tv_sec  = (time_t)(fd->last_modified_ms / 1000);
            tv[0].tv_usec = tv[1].tv_usec = 0;
            utimes(path, tv);
        }
        jl_log("  file %s (%d bytes)", fd->name, fd->size);
    }
    return true;
}

static int rm_entry(const char* path, const struct stat* sb, int flag, struct FTW* ftw)
{
    (void)sb; (void)flag; (void)ftw;
    return remove(path);
}

/* -------------------------------------------------------------------------
 * Model construction helpers
 * ---------------------------------------------------------------------- */

static MmsVariableSpecification* clone_spec(const MmsVariableSpecification* src)
{
    MmsVariableSpecification* s =
        (MmsVariableSpecification*)calloc(1, sizeof(MmsVariableSpecification));
    if (!s) return NULL;
    *s = *src;
    s->name = src->name ? strdup(src->name) : NULL;
    return s;
}

static MmsDevice* build_device(FixtureDef* f)
{
    MmsDevice* dev = MmsDevice_create(NULL);
    if (!dev) return NULL;

    dev->domainCount = f->domain_count;
    dev->domains     = (MmsDomain**)calloc(f->domain_count, sizeof(MmsDomain*));
    if (!dev->domains) return NULL;

    for (int di = 0; di < f->domain_count; di++) {
        DomainDef* dd = &f->domains[di];

        MmsDomain* dom = MmsDomain_create(dd->name);
        if (!dom) return NULL;

        dom->namedVariablesCount = dd->var_count;
        dom->namedVariables = (MmsVariableSpecification**)
            calloc(dd->var_count, sizeof(MmsVariableSpecification*));
        if (!dom->namedVariables) return NULL;

        for (int vi = 0; vi < dd->var_count; vi++) {
            dom->namedVariables[vi] = clone_spec(dd->vars[vi].spec);
        }

        for (int ji = 0; ji < dd->journal_count; ji++) {
            MmsDomain_addJournal(dom, dd->journals[ji].name);
            MmsJournal journal = MmsDomain_getJournal(dom, dd->journals[ji].name);
            journal->logStorage = mem_log_storage(&dd->journals[ji]);
            jl_log("  journal %s/%s (%d entries)", dd->name, dd->journals[ji].name,
                   dd->journals[ji].entry_count);
        }

        dev->domains[di] = dom;
    }
    return dev;
}

/* -------------------------------------------------------------------------
 * Entry point
 * ---------------------------------------------------------------------- */

int main(int argc, char* argv[])
{
    jl_handle_meta_flags(argc, argv, "libiec61850");
    const char* fixture_path  = NULL;
    const char* filestore_arg = NULL;
    int         port          = 1102;

    /* argument parsing */
    for (int i = 1; i < argc - 1; i++) {
        if (strcmp(argv[i], "--fixture")   == 0) fixture_path  = argv[i + 1];
        if (strcmp(argv[i], "--port")      == 0) port = atoi(argv[i + 1]);
        if (strcmp(argv[i], "--filestore") == 0) filestore_arg = argv[i + 1];
    }

    if (!fixture_path) {
        fixture_path = "/fixtures/mms/interop.json";
    }

    /* load fixture */
    FixtureDef* fix = fixture_load(fixture_path);
    if (!fix) {
        jl_fatal(3, "error: fixture load failed: %s", fixture_path);
    }
    g_fixture = fix;

    jl_log("loaded fixture %s: %d domain(s), %d file(s)", fix->fixture_revision,
           fix->domain_count, fix->file_count);

    /* build device model */
    MmsDevice* dev = build_device(fix);
    if (!dev) {
        jl_fatal(1, "error: failed to build MMS device model");
    }
    g_device = dev;

    /* create server */
    g_server = MmsServer_create(dev, NULL);
    if (!g_server) {
        jl_fatal(1, "error: MmsServer_create failed");
    }

    /* install write handler */
    MmsServer_installWriteHandler(g_server, write_handler, NULL);

    /* set server identity and VMD status */
    MmsServer_setServerIdentity(g_server,
        fix->vendor, fix->model, fix->revision);
    MmsServer_setVMDStatus(g_server, fix->status_logical, fix->status_physical);

    /* file store: a fresh directory per run so delete/rename never leak */
    if (filestore_arg) {
        snprintf(g_filestore, sizeof(g_filestore), "%s/", filestore_arg);
        mkdir(filestore_arg, 0755);
    } else {
        char tmpl[] = "/tmp/mms-filestore-XXXXXX";
        if (!mkdtemp(tmpl))
            jl_fatal(1, "error: mkdtemp failed: %s", strerror(errno));
        snprintf(g_filestore, sizeof(g_filestore), "%s/", tmpl);
    }
    if (!populate_filestore(fix, g_filestore))
        jl_fatal(1, "error: cannot populate file store %s", g_filestore);
    MmsServer_setFilestoreBasepath(g_server, g_filestore);
    jl_log("file store at %s", g_filestore);

    /* pre-populate value cache and build write-entry table */
    int total_vars = 0;
    for (int di = 0; di < fix->domain_count; di++)
        total_vars += fix->domains[di].var_count;

    g_entries  = (WriteEntry*)calloc(total_vars, sizeof(WriteEntry));
    g_nentries = 0;

    for (int di = 0; di < fix->domain_count; di++) {
        DomainDef* dd  = &fix->domains[di];
        MmsDomain* dom = dev->domains[di];

        for (int vi = 0; vi < dd->var_count; vi++) {
            VarDef* vd = &dd->vars[vi];

            /* insert a CLONE into the server cache */
            MmsValue* cached_val = MmsValue_clone(vd->value);
            MmsServer_insertIntoCache(g_server, dom, vd->name, cached_val);

            /* keep reference for write handler */
            g_entries[g_nentries].domain_name = dd->name;
            g_entries[g_nentries].var_name    = vd->name;
            g_entries[g_nentries].writable    = vd->writable;
            g_nentries++;

            jl_log("  registered %s/%s (%s)", dd->name, vd->name,
                   vd->writable ? "rw" : "ro");
        }
    }

    /* signals */
    signal(SIGTERM, on_signal);
    signal(SIGINT,  on_signal);

    /* start listening (threadless mode — runs in the main thread).
     * IsoServer_startListeningThreadless() binds and listens synchronously;
     * MmsServer_isRunning() reports success immediately after the call.
     * MmsServer_waitReady() is a select/poll for INCOMING activity — it must
     * NOT be used as a startup check (see SPIKE.md — Threading model). */
    MmsServer_startListeningThreadless(g_server, port);

    if (!MmsServer_isRunning(g_server)) {
        jl_fatal(1, "error: server failed to start on port %d", port);
    }

    /* emit readiness event */
    char addr_buf[64];
    snprintf(addr_buf, sizeof(addr_buf), "0.0.0.0:%d", port);
    jl_ready(addr_buf, fix->fixture_revision, "libiec61850", "");

    jl_log("listening on port %d", port);

    /* main loop: wait up to 50 ms for incoming activity, then service it.
     * The short wait keeps server-initiated work (ObtainFile transfers)
     * moving between client requests. */
    while (g_running) {
        MmsServer_waitReady(g_server, 50);
        MmsServer_handleIncomingMessages(g_server);
        MmsServer_handleBackgroundTasks(g_server);
    }

    jl_log("shutting down");
    MmsServer_stopListeningThreadless(g_server);
    MmsServer_destroy(g_server);

    if (!filestore_arg)
        nftw(g_filestore, rm_entry, 16, FTW_DEPTH | FTW_PHYS);

    free(g_entries);
    fixture_free(fix);

    return 0;
}
