/* SPDX-License-Identifier: MIT */
/*
 * mms_client_services.c — fixture mms-v2 service sequence for the
 * libiec61850 MMS client adapter.
 *
 * Every operation emits one JSON line:
 *
 *   {"operation":"<op>","target":"<target>","ok":true,"value":<json>}
 *   {"operation":"<op>","target":"<target>","ok":false,"error":"<reason>"}
 *
 * Values use a generic MmsValue → JSON mapping (see sb_value). The
 * operation/target pairs are part of the adapter output contract and are
 * asserted by consumer test suites.
 */

#include <inttypes.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "linked_list.h"
#include "mms_client_connection.h"
#include "mms_value.h"

#include "jsonlines.h"
#include "mms_client_services.h"

/* -------------------------------------------------------------------------
 * String builder
 * ---------------------------------------------------------------------- */

typedef struct {
    char*  buf;
    size_t len;
    size_t cap;
} SB;

static void sb_init(SB* sb)
{
    sb->cap = 1024;
    sb->len = 0;
    sb->buf = malloc(sb->cap);
    sb->buf[0] = 0;
}

static void sb_printf(SB* sb, const char* fmt, ...)
{
    for (;;) {
        va_list ap;
        va_start(ap, fmt);
        int n = vsnprintf(sb->buf + sb->len, sb->cap - sb->len, fmt, ap);
        va_end(ap);
        if (n < 0)
            return;
        if ((size_t)n < sb->cap - sb->len) {
            sb->len += (size_t)n;
            return;
        }
        sb->cap = (sb->cap + (size_t)n) * 2;
        sb->buf = realloc(sb->buf, sb->cap);
    }
}

static void sb_string(SB* sb, const char* s)
{
    sb_printf(sb, "\"");
    for (const unsigned char* p = (const unsigned char*)(s ? s : ""); *p; p++) {
        if (*p == '"' || *p == '\\')
            sb_printf(sb, "\\%c", *p);
        else if (*p < 0x20)
            sb_printf(sb, "\\u%04x", *p);
        else
            sb_printf(sb, "%c", *p);
    }
    sb_printf(sb, "\"");
}

static void sb_hex(SB* sb, const uint8_t* data, int len)
{
    sb_printf(sb, "\"");
    for (int i = 0; i < len; i++)
        sb_printf(sb, "%02x", data[i]);
    sb_printf(sb, "\"");
}

static const char* dae_name(MmsDataAccessError e)
{
    switch (e) {
    case DATA_ACCESS_ERROR_OBJECT_INVALIDATED:            return "object-invalidated";
    case DATA_ACCESS_ERROR_HARDWARE_FAULT:                return "hardware-fault";
    case DATA_ACCESS_ERROR_TEMPORARILY_UNAVAILABLE:       return "temporarily-unavailable";
    case DATA_ACCESS_ERROR_OBJECT_ACCESS_DENIED:          return "object-access-denied";
    case DATA_ACCESS_ERROR_OBJECT_UNDEFINED:              return "object-undefined";
    case DATA_ACCESS_ERROR_INVALID_ADDRESS:               return "invalid-address";
    case DATA_ACCESS_ERROR_TYPE_UNSUPPORTED:              return "type-unsupported";
    case DATA_ACCESS_ERROR_TYPE_INCONSISTENT:             return "type-inconsistent";
    case DATA_ACCESS_ERROR_OBJECT_ATTRIBUTE_INCONSISTENT: return "object-attribute-inconsistent";
    case DATA_ACCESS_ERROR_OBJECT_ACCESS_UNSUPPORTED:     return "object-access-unsupported";
    case DATA_ACCESS_ERROR_OBJECT_NONE_EXISTENT:          return "object-non-existent";
    case DATA_ACCESS_ERROR_OBJECT_VALUE_INVALID:          return "object-value-invalid";
    case DATA_ACCESS_ERROR_SUCCESS:                       return "success";
    default:                                              return "unknown";
    }
}

static const char* mms_err_name(MmsError e)
{
    switch (e) {
    case MMS_ERROR_NONE:                          return "none";
    case MMS_ERROR_CONNECTION_LOST:               return "connection-lost";
    case MMS_ERROR_SERVICE_TIMEOUT:               return "service-timeout";
    case MMS_ERROR_PARSING_RESPONSE:              return "parsing-response";
    case MMS_ERROR_REJECT_INVALID_PDU:            return "reject-invalid-pdu";
    case MMS_ERROR_REJECT_UNRECOGNIZED_SERVICE:   return "reject-unrecognized-service";
    case MMS_ERROR_ACCESS_OBJECT_NON_EXISTENT:    return "object-non-existent";
    case MMS_ERROR_ACCESS_OBJECT_ACCESS_DENIED:   return "object-access-denied";
    case MMS_ERROR_DEFINITION_OBJECT_UNDEFINED:   return "object-undefined";
    case MMS_ERROR_FILE_FILE_NON_EXISTENT:        return "file-non-existent";
    case MMS_ERROR_FILE_OTHER:                    return "file-other";
    case MMS_ERROR_SERVICE_OTHER:                 return "service-other";
    default:                                      return "mms-error";
    }
}

/* MmsValue → JSON. Integers are numbers, octet strings hex strings, bit
 * strings "0101" strings (bit 0 first), times milliseconds since the
 * epoch, arrays and structures JSON arrays, and per-variable data access
 * errors {"error":"<name>"}. */
static void sb_value(SB* sb, const MmsValue* v)
{
    if (!v) {
        sb_printf(sb, "null");
        return;
    }
    switch (MmsValue_getType(v)) {
    case MMS_BOOLEAN:
        sb_printf(sb, MmsValue_getBoolean(v) ? "true" : "false");
        break;
    case MMS_INTEGER:
        sb_printf(sb, "%" PRId64, MmsValue_toInt64(v));
        break;
    case MMS_UNSIGNED:
        sb_printf(sb, "%" PRIu32, MmsValue_toUint32(v));
        break;
    case MMS_FLOAT:
        if (MmsValue_toDouble(v) == (double)MmsValue_toFloat(v))
            sb_printf(sb, "%.9g", (double)MmsValue_toFloat(v));
        else
            sb_printf(sb, "%.17g", MmsValue_toDouble(v));
        break;
    case MMS_VISIBLE_STRING:
    case MMS_STRING:
        sb_string(sb, MmsValue_toString((MmsValue*)v));
        break;
    case MMS_OCTET_STRING:
        sb_hex(sb, MmsValue_getOctetStringBuffer((MmsValue*)v), MmsValue_getOctetStringSize(v));
        break;
    case MMS_BIT_STRING: {
        int n = MmsValue_getBitStringSize(v);
        sb_printf(sb, "\"");
        for (int i = 0; i < n; i++)
            sb_printf(sb, "%c", MmsValue_getBitStringBit(v, i) ? '1' : '0');
        sb_printf(sb, "\"");
        break;
    }
    case MMS_UTC_TIME:
        sb_printf(sb, "%" PRIu64, MmsValue_getUtcTimeInMs(v));
        break;
    case MMS_BINARY_TIME:
        sb_printf(sb, "%" PRIu64, MmsValue_getBinaryTimeAsUtcMs(v));
        break;
    case MMS_ARRAY:
    case MMS_STRUCTURE: {
        int n = MmsValue_getArraySize(v);
        sb_printf(sb, "[");
        for (int i = 0; i < n; i++) {
            if (i)
                sb_printf(sb, ",");
            sb_value(sb, MmsValue_getElement((MmsValue*)v, i));
        }
        sb_printf(sb, "]");
        break;
    }
    case MMS_DATA_ACCESS_ERROR:
        sb_printf(sb, "{\"error\":\"%s\"}", dae_name(MmsValue_getDataAccessError(v)));
        break;
    default:
        sb_printf(sb, "null");
        break;
    }
}

static const char* spec_type_name(MmsType t)
{
    switch (t) {
    case MMS_BOOLEAN:        return "boolean";
    case MMS_INTEGER:        return "integer";
    case MMS_UNSIGNED:       return "unsigned";
    case MMS_FLOAT:          return "float32";
    case MMS_VISIBLE_STRING: return "visible-string";
    case MMS_STRING:         return "mms-string";
    case MMS_OCTET_STRING:   return "octet-string";
    case MMS_BIT_STRING:     return "bit-string";
    case MMS_UTC_TIME:       return "utc-time";
    case MMS_BINARY_TIME:    return "binary-time";
    case MMS_ARRAY:          return "array";
    case MMS_STRUCTURE:      return "structure";
    default:                 return "unknown";
    }
}

static void sb_spec(SB* sb, MmsVariableSpecification* spec)
{
    sb_printf(sb, "{\"type\":\"%s\"", spec_type_name(spec->type));
    if (spec->type == MMS_ARRAY) {
        sb_printf(sb, ",\"count\":%d,\"element\":", spec->typeSpec.array.elementCount);
        sb_spec(sb, spec->typeSpec.array.elementTypeSpec);
    } else if (spec->type == MMS_STRUCTURE) {
        sb_printf(sb, ",\"components\":[");
        for (int i = 0; i < spec->typeSpec.structure.elementCount; i++) {
            MmsVariableSpecification* c = spec->typeSpec.structure.elements[i];
            if (i)
                sb_printf(sb, ",");
            sb_printf(sb, "{\"name\":");
            sb_string(sb, c->name);
            sb_printf(sb, ",\"spec\":");
            sb_spec(sb, c);
            sb_printf(sb, "}");
        }
        sb_printf(sb, "]");
    }
    sb_printf(sb, "}");
}

static void sb_names(SB* sb, LinkedList names)
{
    sb_printf(sb, "[");
    int first = 1;
    for (LinkedList e = LinkedList_getNext(names); e; e = LinkedList_getNext(e)) {
        if (!first)
            sb_printf(sb, ",");
        sb_string(sb, (const char*)e->data);
        first = 0;
    }
    sb_printf(sb, "]");
}

/* -------------------------------------------------------------------------
 * JSON line output
 * ---------------------------------------------------------------------- */

static pthread_mutex_t g_out_mu = PTHREAD_MUTEX_INITIALIZER;

static void emit_line(SB* line)
{
    pthread_mutex_lock(&g_out_mu);
    fputs(line->buf, stdout);
    fputs("\n", stdout);
    fflush(stdout);
    pthread_mutex_unlock(&g_out_mu);
    free(line->buf);
}

static void begin(SB* sb, const char* op, const char* target, int ok)
{
    sb_init(sb);
    sb_printf(sb, "{\"operation\":");
    sb_string(sb, op);
    sb_printf(sb, ",\"target\":");
    sb_string(sb, target);
    sb_printf(sb, ",\"ok\":%s", ok ? "true" : "false");
}

static void emit_ok(const char* op, const char* target)
{
    SB sb;
    begin(&sb, op, target, 1);
    sb_printf(&sb, "}");
    emit_line(&sb);
}

static void emit_fail(const char* op, const char* target, const char* reason)
{
    SB sb;
    begin(&sb, op, target, 0);
    sb_printf(&sb, ",\"error\":");
    sb_string(&sb, reason);
    sb_printf(&sb, "}");
    emit_line(&sb);
}

static void emit_value(const char* op, const char* target, const MmsValue* v)
{
    SB sb;
    begin(&sb, op, target, 1);
    sb_printf(&sb, ",\"value\":");
    sb_value(&sb, v);
    sb_printf(&sb, "}");
    emit_line(&sb);
}

static void emit_names(const char* target, LinkedList names)
{
    SB sb;
    begin(&sb, "get-name-list", target, 1);
    sb_printf(&sb, ",\"names\":");
    sb_names(&sb, names);
    sb_printf(&sb, "}");
    emit_line(&sb);
}

/* Emits a read result: a fail line for service errors and per-variable
 * data access errors, otherwise the value. Frees v. */
static void emit_read(const char* op, const char* target, MmsError err, MmsValue* v)
{
    if (err != MMS_ERROR_NONE || !v)
        emit_fail(op, target, mms_err_name(err));
    else if (MmsValue_getType(v) == MMS_DATA_ACCESS_ERROR)
        emit_fail(op, target, dae_name(MmsValue_getDataAccessError(v)));
    else
        emit_value(op, target, v);
    if (v)
        MmsValue_delete(v);
}

static void emit_write(const char* op, const char* target, MmsError err, MmsDataAccessError dae)
{
    if (err == MMS_ERROR_NONE && dae == DATA_ACCESS_ERROR_SUCCESS)
        emit_ok(op, target);
    else if (dae >= 0)
        emit_fail(op, target, dae_name(dae));
    else
        emit_fail(op, target, mms_err_name(err));
}

static void emit_access_results(const char* op, const char* target, MmsError err, LinkedList results)
{
    if (err != MMS_ERROR_NONE) {
        emit_fail(op, target, mms_err_name(err));
    } else {
        const char* failure = NULL;
        for (LinkedList e = LinkedList_getNext(results); e; e = LinkedList_getNext(e)) {
            MmsValue* r = (MmsValue*)e->data;
            if (r && MmsValue_getType(r) == MMS_DATA_ACCESS_ERROR &&
                MmsValue_getDataAccessError(r) != DATA_ACCESS_ERROR_SUCCESS)
                failure = dae_name(MmsValue_getDataAccessError(r));
        }
        if (failure)
            emit_fail(op, target, failure);
        else
            emit_ok(op, target);
    }
    if (results)
        LinkedList_destroyDeep(results, (LinkedListValueDeleteFunction)MmsValue_delete);
}

/* -------------------------------------------------------------------------
 * InformationReport reception
 * ---------------------------------------------------------------------- */

static pthread_mutex_t g_report_mu = PTHREAD_MUTEX_INITIALIZER;
static int g_report_count;

/* Reports arrive on the connection thread. Named-list reports carry all
 * values in one call; listOfVariable reports produce one call per
 * variable (target "<domain>/<item>"). The handler owns value. */
static void report_handler(void* param, char* domainName, char* name, MmsValue* value, bool isList)
{
    (void)param;
    char target[160];
    if (isList)
        snprintf(target, sizeof(target), "list:%s%s%s", domainName ? domainName : "",
                 domainName ? "/" : "", name);
    else
        snprintf(target, sizeof(target), "var:%s%s%s", domainName ? domainName : "",
                 domainName ? "/" : "", name);
    emit_value("information-report", target, value);
    if (value)
        MmsValue_delete(value);

    pthread_mutex_lock(&g_report_mu);
    g_report_count++;
    pthread_mutex_unlock(&g_report_mu);
}

static int report_count(void)
{
    pthread_mutex_lock(&g_report_mu);
    int n = g_report_count;
    pthread_mutex_unlock(&g_report_mu);
    return n;
}

void services_install_report_handler(MmsConnection conn)
{
    MmsConnection_setInformationReportHandler(conn, report_handler, NULL);
}

/* -------------------------------------------------------------------------
 * Service groups
 * ---------------------------------------------------------------------- */

static LinkedList member_specs(void)
{
    LinkedList specs = LinkedList_create();
    LinkedList_add(specs, MmsVariableAccessSpecification_create(strdup("interop"), strdup("boolean")));
    LinkedList_add(specs, MmsVariableAccessSpecification_create(strdup("interop"), strdup("integer")));
    return specs;
}

static void free_specs(LinkedList specs)
{
    LinkedList_destroyDeep(specs, (LinkedListValueDeleteFunction)MmsVariableAccessSpecification_destroy);
}

static void emit_nvl_directory(const char* target, MmsError err, LinkedList specs, bool deletable)
{
    if (err != MMS_ERROR_NONE || !specs) {
        emit_fail("get-nvl-attrs", target, mms_err_name(err));
        if (specs)
            free_specs(specs);
        return;
    }
    SB sb;
    begin(&sb, "get-nvl-attrs", target, 1);
    sb_printf(&sb, ",\"value\":{\"deletable\":%s,\"variables\":[", deletable ? "true" : "false");
    int first = 1;
    for (LinkedList e = LinkedList_getNext(specs); e; e = LinkedList_getNext(e)) {
        MmsVariableAccessSpecification* s = (MmsVariableAccessSpecification*)e->data;
        char ref[160];
        snprintf(ref, sizeof(ref), "%s%s%s", s->domainId ? s->domainId : "", s->domainId ? "/" : "", s->itemId);
        if (!first)
            sb_printf(&sb, ",");
        sb_string(&sb, ref);
        first = 0;
    }
    sb_printf(&sb, "]}}");
    emit_line(&sb);
    free_specs(specs);
}

static LinkedList two_values(MmsValue* a, MmsValue* b)
{
    LinkedList l = LinkedList_create();
    LinkedList_add(l, a);
    LinkedList_add(l, b);
    return l;
}

static void svc_status(MmsConnection conn)
{
    MmsError err = MMS_ERROR_NONE;
    const char* targets[] = {"", "extended"};
    for (int i = 0; i < 2; i++) {
        int logical = -1, physical = -1;
        MmsConnection_getServerStatus(conn, &err, &logical, &physical, i == 1);
        if (err != MMS_ERROR_NONE) {
            emit_fail("status", targets[i], mms_err_name(err));
            continue;
        }
        SB sb;
        begin(&sb, "status", targets[i], 1);
        sb_printf(&sb, ",\"value\":{\"logical\":%d,\"physical\":%d}}", logical, physical);
        emit_line(&sb);
    }
}

static void svc_name_lists(MmsConnection conn)
{
    MmsError err = MMS_ERROR_NONE;

    LinkedList l = MmsConnection_getDomainJournals(conn, &err, "interop");
    if (err != MMS_ERROR_NONE || !l)
        emit_fail("get-name-list", "interop/journals", mms_err_name(err));
    else
        emit_names("interop/journals", l);
    if (l)
        LinkedList_destroy(l);

    l = MmsConnection_getVMDVariableNames(conn, &err);
    if (err != MMS_ERROR_NONE || !l)
        emit_fail("get-name-list", "vmd/variables", mms_err_name(err));
    else
        emit_names("vmd/variables", l);
    if (l)
        LinkedList_destroy(l);

    /* Expected to fail: the server must reject an unknown domain */
    err = MMS_ERROR_NONE;
    l = MmsConnection_getDomainVariableNames(conn, &err, "unknown-domain");
    if (err != MMS_ERROR_NONE || !l)
        emit_fail("get-name-list", "unknown-domain/variables", mms_err_name(err));
    else
        emit_names("unknown-domain/variables", l);
    if (l)
        LinkedList_destroy(l);
}

static void svc_type_specs(MmsConnection conn)
{
    const char* items[] = {"array-rw", "structure-rw"};
    for (int i = 0; i < 2; i++) {
        MmsError err = MMS_ERROR_NONE;
        char target[64];
        snprintf(target, sizeof(target), "interop/%s", items[i]);
        MmsVariableSpecification* spec = MmsConnection_getVariableAccessAttributes(conn, &err, "interop", items[i]);
        if (err != MMS_ERROR_NONE || !spec) {
            emit_fail("get-var-access-attr", target, mms_err_name(err));
            continue;
        }
        SB sb;
        begin(&sb, "get-var-access-attr", target, 1);
        sb_printf(&sb, ",\"type\":\"%s\",\"value\":", spec_type_name(spec->type));
        sb_spec(&sb, spec);
        sb_printf(&sb, "}");
        emit_line(&sb);
        MmsVariableSpecification_destroy(spec);
    }
}

static void svc_alternate_access(MmsConnection conn)
{
    MmsError err = MMS_ERROR_NONE;
    MmsValue* v;

    v = MmsConnection_readArrayElements(conn, &err, "interop", "array", 1, 3);
    emit_read("read-array-elements", "interop/array[1:4]", err, v);

    v = MmsConnection_readArrayElements(conn, &err, "interop", "array", 2, 0);
    emit_read("read-array-elements", "interop/array[2]", err, v);

    v = MmsConnection_readVariableComponent(conn, &err, "interop", "structure-rw", "count");
    emit_read("read-component", "interop/structure-rw$count", err, v);

    MmsValue* elem = MmsValue_newIntegerFromInt32(21);
    MmsDataAccessError dae = MmsConnection_writeArrayElements(conn, &err, "interop", "array-rw", 1, 0, elem);
    MmsValue_delete(elem);
    emit_write("write-array-elements", "interop/array-rw[1]", err, dae);

    MmsValue* range = MmsValue_createEmptyArray(2);
    MmsValue_setElement(range, 0, MmsValue_newIntegerFromInt32(33));
    MmsValue_setElement(range, 1, MmsValue_newIntegerFromInt32(44));
    dae = MmsConnection_writeArrayElements(conn, &err, "interop", "array-rw", 2, 2, range);
    MmsValue_delete(range);
    emit_write("write-array-elements", "interop/array-rw[2:4]", err, dae);

    MmsValue* count = MmsValue_newIntegerFromInt32(99);
    dae = MmsConnection_writeVariableComponent(conn, &err, "interop", "structure-rw", "count", count);
    MmsValue_delete(count);
    emit_write("write-component", "interop/structure-rw$count", err, dae);

    v = MmsConnection_readVariable(conn, &err, "interop", "array-rw");
    emit_read("read", "interop/array-rw", err, v);
    v = MmsConnection_readVariable(conn, &err, "interop", "structure-rw");
    emit_read("read", "interop/structure-rw", err, v);
}

static void svc_named_variable_lists(MmsConnection conn)
{
    MmsError err = MMS_ERROR_NONE;
    bool deletable = false;
    LinkedList l;

    /* Domain scope */
    LinkedList specs = member_specs();
    MmsConnection_defineNamedVariableList(conn, &err, "interop", "svclist", specs);
    free_specs(specs);
    if (err != MMS_ERROR_NONE)
        emit_fail("define-nvl", "interop/svclist", mms_err_name(err));
    else
        emit_ok("define-nvl", "interop/svclist");

    l = MmsConnection_getDomainVariableListNames(conn, &err, "interop");
    if (err != MMS_ERROR_NONE || !l)
        emit_fail("get-name-list", "interop/nvl", mms_err_name(err));
    else
        emit_names("interop/nvl", l);
    if (l)
        LinkedList_destroy(l);

    l = MmsConnection_readNamedVariableListDirectory(conn, &err, "interop", "svclist", &deletable);
    emit_nvl_directory("interop/svclist", err, l, deletable);

    MmsValue* v = MmsConnection_readNamedVariableListValues(conn, &err, "interop", "svclist", true);
    emit_read("read-nvl", "interop/svclist?spec", err, v);

    LinkedList results = NULL;
    LinkedList vals = two_values(MmsValue_newBoolean(true), MmsValue_newIntegerFromInt32(77));
    MmsConnection_writeNamedVariableList(conn, &err, false, "interop", "svclist", vals, &results);
    LinkedList_destroyDeep(vals, (LinkedListValueDeleteFunction)MmsValue_delete);
    emit_access_results("write-nvl", "interop/svclist", err, results);

    v = MmsConnection_readNamedVariableListValues(conn, &err, "interop", "svclist", false);
    emit_read("read-nvl", "interop/svclist", err, v);

    if (MmsConnection_deleteNamedVariableList(conn, &err, "interop", "svclist") && err == MMS_ERROR_NONE)
        emit_ok("delete-nvl", "interop/svclist");
    else
        emit_fail("delete-nvl", "interop/svclist", mms_err_name(err));

    /* VMD scope */
    specs = member_specs();
    MmsConnection_defineNamedVariableList(conn, &err, NULL, "vmdlist", specs);
    free_specs(specs);
    if (err != MMS_ERROR_NONE)
        emit_fail("define-nvl", "vmd/vmdlist", mms_err_name(err));
    else
        emit_ok("define-nvl", "vmd/vmdlist");

    l = MmsConnection_readNamedVariableListDirectory(conn, &err, NULL, "vmdlist", &deletable);
    emit_nvl_directory("vmd/vmdlist", err, l, deletable);

    v = MmsConnection_readNamedVariableListValues(conn, &err, NULL, "vmdlist", false);
    emit_read("read-nvl", "vmd/vmdlist", err, v);

    if (MmsConnection_deleteNamedVariableList(conn, &err, NULL, "vmdlist") && err == MMS_ERROR_NONE)
        emit_ok("delete-nvl", "vmd/vmdlist");
    else
        emit_fail("delete-nvl", "vmd/vmdlist", mms_err_name(err));

    /* Server-defined list behind the fixture's named-list InformationReport */
    l = MmsConnection_readNamedVariableListDirectory(conn, &err, NULL, "interop-report", &deletable);
    emit_nvl_directory("vmd/interop-report", err, l, deletable);
    v = MmsConnection_readNamedVariableListValues(conn, &err, NULL, "interop-report", false);
    emit_read("read-nvl", "vmd/interop-report", err, v);

    /* Association scope */
    specs = member_specs();
    MmsConnection_defineNamedVariableListAssociationSpecific(conn, &err, "aalist", specs);
    free_specs(specs);
    if (err != MMS_ERROR_NONE)
        emit_fail("define-nvl", "aa/aalist", mms_err_name(err));
    else
        emit_ok("define-nvl", "aa/aalist");

    l = MmsConnection_getVariableListNamesAssociationSpecific(conn, &err);
    if (err != MMS_ERROR_NONE || !l)
        emit_fail("get-name-list", "aa/nvl", mms_err_name(err));
    else
        emit_names("aa/nvl", l);
    if (l)
        LinkedList_destroy(l);

    l = MmsConnection_readNamedVariableListDirectoryAssociationSpecific(conn, &err, "aalist", &deletable);
    emit_nvl_directory("aa/aalist", err, l, deletable);

    v = MmsConnection_readNamedVariableListValuesAssociationSpecific(conn, &err, "aalist", false);
    emit_read("read-nvl", "aa/aalist", err, v);

    results = NULL;
    vals = two_values(MmsValue_newBoolean(true), MmsValue_newIntegerFromInt32(-123));
    MmsConnection_writeNamedVariableList(conn, &err, true, NULL, "aalist", vals, &results);
    LinkedList_destroyDeep(vals, (LinkedListValueDeleteFunction)MmsValue_delete);
    emit_access_results("write-nvl", "aa/aalist", err, results);

    if (MmsConnection_deleteAssociationSpecificNamedVariableList(conn, &err, "aalist") && err == MMS_ERROR_NONE)
        emit_ok("delete-nvl", "aa/aalist");
    else
        emit_fail("delete-nvl", "aa/aalist", mms_err_name(err));
}

/* --- File services ------------------------------------------------------ */

typedef struct {
    SB   sb;
    int  count;
    char last[256];
} DirCollector;

static void dir_handler(void* param, char* filename, uint32_t size, uint64_t lastModified)
{
    DirCollector* dc = (DirCollector*)param;
    snprintf(dc->last, sizeof(dc->last), "%s", filename);
    if (dc->count++)
        sb_printf(&dc->sb, ",");
    sb_printf(&dc->sb, "{\"name\":");
    sb_string(&dc->sb, filename);
    sb_printf(&dc->sb, ",\"size\":%" PRIu32 ",\"lastModified\":%" PRIu64 "}", size, lastModified);
}

static void file_directory(MmsConnection conn, const char* spec, const char* target)
{
    MmsError err = MMS_ERROR_NONE;
    DirCollector dc;
    begin(&dc.sb, "file-directory", target, 1);
    sb_printf(&dc.sb, ",\"value\":[");
    dc.count = 0;
    dc.last[0] = 0;

    char after[256] = "";
    bool more = true;
    while (more && err == MMS_ERROR_NONE) {
        more = MmsConnection_getFileDirectory(conn, &err, spec, after[0] ? after : NULL,
                                              dir_handler, &dc);
        if (more && strcmp(after, dc.last) == 0)
            break; /* no progress */
        snprintf(after, sizeof(after), "%s", dc.last);
    }
    if (err != MMS_ERROR_NONE) {
        free(dc.sb.buf);
        emit_fail("file-directory", target, mms_err_name(err));
        return;
    }
    sb_printf(&dc.sb, "]}");
    emit_line(&dc.sb);
}

typedef struct {
    uint8_t* data;
    size_t   len;
    size_t   cap;
} ByteBuf;

static void file_read_handler(void* param, int32_t frsmId, uint8_t* buffer, uint32_t n)
{
    (void)frsmId;
    ByteBuf* b = (ByteBuf*)param;
    if (b->len + n > b->cap) {
        b->cap = (b->len + n) * 2;
        b->data = realloc(b->data, b->cap);
    }
    memcpy(b->data + b->len, buffer, n);
    b->len += n;
}

static uint64_t fnv1a64(const uint8_t* d, size_t n)
{
    uint64_t h = 0xcbf29ce484222325ULL;
    for (size_t i = 0; i < n; i++) {
        h ^= d[i];
        h *= 0x100000001b3ULL;
    }
    return h;
}

/* Opens, reads to the end and closes a file. Emits "file-read" with
 * {size, lastModified, length, fnv1a, data(hex, small files only)} and
 * "file-close". */
static void file_read(MmsConnection conn, const char* name, uint32_t position, const char* target)
{
    MmsError err = MMS_ERROR_NONE;
    uint32_t size = 0;
    uint64_t lastModified = 0;

    int32_t frsm = MmsConnection_fileOpen(conn, &err, name, position, &size, &lastModified);
    if (err != MMS_ERROR_NONE) {
        emit_fail("file-read", target, mms_err_name(err));
        return;
    }

    ByteBuf b = {NULL, 0, 0};
    bool more = true;
    while (more && err == MMS_ERROR_NONE)
        more = MmsConnection_fileRead(conn, &err, frsm, file_read_handler, &b);

    if (err != MMS_ERROR_NONE) {
        emit_fail("file-read", target, mms_err_name(err));
    } else {
        SB sb;
        begin(&sb, "file-read", target, 1);
        sb_printf(&sb, ",\"value\":{\"size\":%" PRIu32 ",\"lastModified\":%" PRIu64
                       ",\"length\":%zu,\"fnv1a\":\"%016" PRIx64 "\"",
                  size, lastModified, b.len, fnv1a64(b.data, b.len));
        if (b.len <= 256) {
            sb_printf(&sb, ",\"data\":");
            sb_hex(&sb, b.data, (int)b.len);
        }
        sb_printf(&sb, "}}");
        emit_line(&sb);
    }
    free(b.data);

    err = MMS_ERROR_NONE;
    MmsConnection_fileClose(conn, &err, frsm);
    if (err != MMS_ERROR_NONE)
        emit_fail("file-close", target, mms_err_name(err));
    else
        emit_ok("file-close", target);
}

static void svc_files(MmsConnection conn)
{
    MmsError err = MMS_ERROR_NONE;

    file_directory(conn, NULL, "");
    file_directory(conn, "COMTRADE", "COMTRADE");
    /* Name of the zero-size entry a per-level server lists for the subdirectory */
    file_directory(conn, "COMTRADE\\", "COMTRADE\\");

    file_read(conn, "REC01.CFG", 0, "REC01.CFG");
    file_read(conn, "REC01.DAT", 4, "REC01.DAT@4");
    file_read(conn, "LARGE.BIN", 0, "LARGE.BIN");
    file_read(conn, "NOTIME.TXT", 0, "NOTIME.TXT");
    file_read(conn, "NO-SUCH.FILE", 0, "NO-SUCH.FILE");

    MmsConnection_fileDelete(conn, &err, "DELETE.ME");
    if (err != MMS_ERROR_NONE)
        emit_fail("file-delete", "DELETE.ME", mms_err_name(err));
    else
        emit_ok("file-delete", "DELETE.ME");

    err = MMS_ERROR_NONE;
    MmsConnection_fileRename(conn, &err, "RENAME.ME", "RENAMED.ME");
    if (err != MMS_ERROR_NONE)
        emit_fail("file-rename", "RENAME.ME->RENAMED.ME", mms_err_name(err));
    else
        emit_ok("file-rename", "RENAME.ME->RENAMED.ME");

    err = MMS_ERROR_NONE;
    MmsConnection_obtainFile(conn, &err, "REC01.CFG", "OBTAINED.CFG");
    if (err != MMS_ERROR_NONE)
        emit_fail("obtain-file", "REC01.CFG->OBTAINED.CFG", mms_err_name(err));
    else
        emit_ok("obtain-file", "REC01.CFG->OBTAINED.CFG");

    file_directory(conn, NULL, "after-changes");
}

/* --- Journals ----------------------------------------------------------- */

static void emit_journal(const char* target, MmsError err, LinkedList entries, bool more)
{
    if (err != MMS_ERROR_NONE || !entries) {
        emit_fail("read-journal", target, mms_err_name(err));
        if (entries)
            LinkedList_destroyDeep(entries, (LinkedListValueDeleteFunction)MmsJournalEntry_destroy);
        return;
    }
    SB sb;
    begin(&sb, "read-journal", target, 1);
    sb_printf(&sb, ",\"value\":{\"moreFollows\":%s,\"entries\":[", more ? "true" : "false");
    int first = 1;
    for (LinkedList e = LinkedList_getNext(entries); e; e = LinkedList_getNext(e)) {
        MmsJournalEntry je = (MmsJournalEntry)e->data;
        if (!first)
            sb_printf(&sb, ",");
        first = 0;
        sb_printf(&sb, "{\"id\":");
        sb_value(&sb, MmsJournalEntry_getEntryID(je));
        sb_printf(&sb, ",\"time\":");
        sb_value(&sb, MmsJournalEntry_getOccurenceTime(je));
        sb_printf(&sb, ",\"variables\":[");
        int vfirst = 1;
        for (LinkedList v = LinkedList_getNext(MmsJournalEntry_getJournalVariables(je)); v; v = LinkedList_getNext(v)) {
            MmsJournalVariable jv = (MmsJournalVariable)v->data;
            if (!vfirst)
                sb_printf(&sb, ",");
            vfirst = 0;
            sb_printf(&sb, "{\"tag\":");
            sb_string(&sb, MmsJournalVariable_getTag(jv));
            sb_printf(&sb, ",\"value\":");
            sb_value(&sb, MmsJournalVariable_getValue(jv));
            sb_printf(&sb, "}");
        }
        sb_printf(&sb, "]}");
    }
    sb_printf(&sb, "]}}");
    emit_line(&sb);
    LinkedList_destroyDeep(entries, (LinkedListValueDeleteFunction)MmsJournalEntry_destroy);
}

static MmsValue* binary_time(uint64_t ms)
{
    MmsValue* t = MmsValue_newBinaryTime(false);
    MmsValue_setBinaryTime(t, ms);
    return t;
}

static void svc_journals(MmsConnection conn)
{
    /* Fixture log1 entries 1..4 are at 2024-01-01T00:00:01..04Z. */
    const uint64_t t1 = 1704067201000ULL;
    const uint64_t t4 = 1704067204000ULL;
    MmsError err = MMS_ERROR_NONE;
    bool more = false;

    MmsValue* start = binary_time(t1);
    MmsValue* stop  = binary_time(t4);
    LinkedList entries = MmsConnection_readJournalTimeRange(conn, &err, "interop", "log1", start, stop, &more);
    emit_journal("interop/log1", err, entries, more);
    MmsValue_delete(start);
    MmsValue_delete(stop);

    /* start after entry 2 (2024-01-01T00:00:02Z, id 2 as 8 LE bytes) */
    uint8_t id2[8] = {2, 0, 0, 0, 0, 0, 0, 0};
    MmsValue* after = binary_time(t1 + 1000);
    MmsValue* entry = MmsValue_newOctetString(8, 8);
    MmsValue_setOctetString(entry, id2, 8);
    err = MMS_ERROR_NONE;
    entries = MmsConnection_readJournalStartAfter(conn, &err, "interop", "log1", after, entry, &more);
    emit_journal("interop/log1@after-2", err, entries, more);
    MmsValue_delete(after);
    MmsValue_delete(entry);

    start = binary_time(t1);
    stop  = binary_time(t4);
    err = MMS_ERROR_NONE;
    entries = MmsConnection_readJournalTimeRange(conn, &err, "interop", "nolog", start, stop, &more);
    emit_journal("interop/nolog", err, entries, more);
    MmsValue_delete(start);
    MmsValue_delete(stop);
}

/* --- InformationReport -------------------------------------------------- */

static void svc_reports(MmsConnection conn)
{
    MmsError err = MMS_ERROR_NONE;
    int before = report_count();

    MmsValue* one = MmsValue_newIntegerFromInt32(1);
    MmsDataAccessError dae = MmsConnection_writeVariable(conn, &err, "interop", "report-trigger", one);
    MmsValue_delete(one);
    emit_write("write", "interop/report-trigger", err, dae);

    /* Wait up to 3 s for reports, then 300 ms of quiet after the last. */
    int last = before;
    int quiet = 0;
    for (int waited = 0; waited < 3000 && quiet < 300; waited += 50) {
        usleep(50 * 1000);
        int n = report_count();
        if (n != last) {
            last = n;
            quiet = 0;
        } else if (n > before) {
            quiet += 50;
        }
    }

    /* The association must survive any unconfirmed PDUs sent along. */
    MmsValue* v = MmsConnection_readVariable(conn, &err, "interop", "boolean");
    emit_read("read", "interop/boolean#after-reports", err, v);
}

/* -------------------------------------------------------------------------
 * Entry points
 * ---------------------------------------------------------------------- */

void run_services(MmsConnection conn)
{
    svc_status(conn);
    svc_name_lists(conn);
    svc_type_specs(conn);
    svc_alternate_access(conn);
    svc_named_variable_lists(conn);
    svc_files(conn);
    svc_journals(conn);
    svc_reports(conn);
}

static void on_connection_lost(MmsConnection conn, void* parameter)
{
    (void)conn;
    *(volatile bool*)parameter = true;
}

void run_server_abort(const char* host, int port)
{
    MmsError err = MMS_ERROR_NONE;
    volatile bool lost = false;
    MmsConnection conn = MmsConnection_create();
    MmsConnection_setRequestTimeout(conn, 3000);
    MmsConnection_setConnectionLostHandler(conn, on_connection_lost, (void*)&lost);
    MmsConnection_connect(conn, &err, host, port);
    if (err != MMS_ERROR_NONE) {
        emit_fail("server-abort", "", mms_err_name(err));
        MmsConnection_destroy(conn);
        return;
    }

    /* Writing -1 to report-trigger asks the server to abort the association.
     * libiec61850 completes calls outstanding at connection loss with
     * SERVICE_TIMEOUT, so the lost-connection callback is the real signal. */
    MmsValue* trigger = MmsValue_newIntegerFromInt32(-1);
    MmsDataAccessError dae = MmsConnection_writeVariable(conn, &err, "interop", "report-trigger", trigger);
    MmsValue_delete(trigger);
    (void)dae;

    if (lost && err != MMS_ERROR_NONE)
        emit_ok("server-abort", "");
    else if (!lost)
        emit_fail("server-abort", "", "connection-not-lost");
    else
        emit_fail("server-abort", "", "write-succeeded");
    MmsConnection_destroy(conn);
}

void run_abort(const char* host, int port)
{
    MmsError err = MMS_ERROR_NONE;
    MmsConnection conn = MmsConnection_create();
    MmsConnection_connect(conn, &err, host, port);
    if (err != MMS_ERROR_NONE) {
        emit_fail("abort", "", mms_err_name(err));
        MmsConnection_destroy(conn);
        return;
    }
    MmsValue* v = MmsConnection_readVariable(conn, &err, "interop", "boolean");
    emit_read("read", "interop/boolean#before-abort", err, v);

    err = MMS_ERROR_NONE;
    MmsConnection_abort(conn, &err);
    if (err != MMS_ERROR_NONE)
        emit_fail("abort", "", mms_err_name(err));
    else
        emit_ok("abort", "");
    MmsConnection_destroy(conn);
}
