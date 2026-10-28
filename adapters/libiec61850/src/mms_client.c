/* SPDX-License-Identifier: MIT */
/*
 * mms_client.c — libIEC61850 MMS client adapter for iec61850-interop.
 *
 * Connects to a host:port MMS server, executes the Phase 1A + Phase 1C
 * operation sequence, and emits one JSON Line per operation to stdout.
 *
 * Phase 1A operations (full_sequence=1):
 *   identify, get-name-list (domains+variables), read bool/int/float32,
 *   write float32, read float32 (verify), write octet-string (read-only),
 *   read bool (server still alive), conclude
 *
 * Phase 1C additions (full_sequence=1):
 *   Step 5 — remaining scalar types:
 *     read unsigned, read+write visible-string (verify), read octet-string
 *     (value assertion), read bit-string, read utc-time, read array,
 *     read structure
 *   Step 6 — data-model operations:
 *     get-var-access-attr, read-multiple, write-multiple, NVL define/read/delete
 *   Step 7 — negative tests:
 *     read unknown domain, read unknown variable, write wrong type
 *
 * Fixture mms-v2 services (full_sequence=1, mms_client_services.c):
 *   status, name lists (journals, VMD variables, domain and association
 *   NVLs), type specs, alternate access reads/writes, NVLs in domain, VMD
 *   and association scope (define, attributes, read, write, delete), file
 *   directory/open/read/close/delete/rename/obtain, journal reads,
 *   InformationReport reception.
 *
 * Reconnect verification (full_sequence=0):
 *   read bool + conclude only.
 *
 * Abort verification (connection 3):
 *   read bool + abort.
 *
 * Server abort verification (connection 4):
 *   write -1 to report-trigger; the server aborts the association.
 *
 * JSON output format is documented in PLAN.md §Adapter output contract.
 *
 * Exit codes:
 *   0  — sequence completed (individual result ok flags determine test outcome)
 *   1  — startup/argument error
 *   2  — connection failure
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <inttypes.h>

#include "mms_client_connection.h"
#include "mms_value.h"
#include "linked_list.h"

#include "jsonlines.h"
#include "mms_client_services.h"

/* -------------------------------------------------------------------------
 * JSON-Lines helpers
 * ---------------------------------------------------------------------- */

static void jl_ok(const char* op)
{
    printf("{\"operation\":\"%s\",\"ok\":true}\n", op);
    fflush(stdout);
}

static void jl_ok_target(const char* op, const char* target)
{
    printf("{\"operation\":\"%s\",\"target\":\"%s\",\"ok\":true}\n", op, target);
    fflush(stdout);
}

static void jl_fail(const char* op, const char* error)
{
    printf("{\"operation\":\"%s\",\"ok\":false,\"error\":\"%s\"}\n", op, error);
    fflush(stdout);
}

static void jl_fail_target(const char* op, const char* target, const char* error)
{
    printf("{\"operation\":\"%s\",\"target\":\"%s\",\"ok\":false,\"error\":\"%s\"}\n",
           op, target, error);
    fflush(stdout);
}

static void jl_identify(const char* vendor, const char* model,
                        const char* revision)
{
    printf("{\"operation\":\"identify\",\"ok\":true,"
           "\"value\":{\"vendor\":\"%s\",\"model\":\"%s\",\"revision\":\"%s\"}}\n",
           vendor ? vendor : "", model ? model : "", revision ? revision : "");
    fflush(stdout);
}

static void jl_namelist(const char* target, LinkedList names)
{
    printf("{\"operation\":\"get-name-list\"");
    if (target && target[0])
        printf(",\"target\":\"%s\"", target);
    printf(",\"ok\":true,\"names\":[");
    int first = 1;
    LinkedList elem = LinkedList_getNext(names);
    while (elem) {
        const char* name = (const char*) elem->data;
        if (!first) printf(",");
        printf("\"%s\"", name);
        first = 0;
        elem = LinkedList_getNext(elem);
    }
    printf("]}\n");
    fflush(stdout);
}

static void jl_read_bool(const char* target, bool value)
{
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,"
           "\"value\":%s}\n", target, value ? "true" : "false");
    fflush(stdout);
}

static void jl_read_int(const char* target, int32_t value)
{
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,"
           "\"value\":%" PRId32 "}\n", target, value);
    fflush(stdout);
}

static void jl_read_uint(const char* target, uint32_t value)
{
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,"
           "\"value\":%" PRIu32 "}\n", target, value);
    fflush(stdout);
}

static void jl_read_float(const char* target, float value)
{
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,"
           "\"value\":%g}\n", target, (double)value);
    fflush(stdout);
}

static void jl_read_string(const char* target, const char* value)
{
    /* Emit a JSON-safe string — control characters and quotes are escaped. */
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,\"value\":\"", target);
    for (const char* p = value; *p; p++) {
        if (*p == '"')       fputs("\\\"", stdout);
        else if (*p == '\\') fputs("\\\\", stdout);
        else                 putchar((unsigned char)*p);
    }
    printf("\"}\n");
    fflush(stdout);
}

static void jl_read_bytes(const char* target, const uint8_t* buf, int len)
{
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,\"value\":\"",
           target);
    for (int i = 0; i < len; i++)
        printf("%02x", buf[i]);
    printf("\"}\n");
    fflush(stdout);
}

static void jl_read_bits(const char* target, MmsValue* val)
{
    int bits = MmsValue_getBitStringSize(val);
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,\"value\":\"",
           target);
    for (int i = 0; i < bits; i++)
        putchar(MmsValue_getBitStringBit(val, i) ? '1' : '0');
    printf("\"}\n");
    fflush(stdout);
}

static void jl_read_utctime(const char* target, MmsValue* val)
{
    uint64_t ms = MmsValue_getUtcTimeInMs(val);
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,"
           "\"value\":%" PRIu64 "}\n", target, ms);
    fflush(stdout);
}

/* Emit an array as a JSON array of integers. */
static void jl_read_array_ints(const char* target, MmsValue* arr)
{
    int n = MmsValue_getArraySize(arr);
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,\"value\":[", target);
    for (int i = 0; i < n; i++) {
        MmsValue* elem = MmsValue_getElement(arr, i);
        if (i > 0) putchar(',');
        printf("%" PRId32, elem ? MmsValue_toInt32(elem) : 0);
    }
    printf("]}\n");
    fflush(stdout);
}

/* Emit a two-component structure {bool, int} as a JSON array. */
static void jl_read_structure_bool_int(const char* target, MmsValue* sv)
{
    MmsValue* e0 = MmsValue_getElement(sv, 0);
    MmsValue* e1 = MmsValue_getElement(sv, 1);
    printf("{\"operation\":\"read\",\"target\":\"%s\",\"ok\":true,"
           "\"value\":[%s,%" PRId32 "]}\n",
           target,
           (e0 && MmsValue_getBoolean(e0)) ? "true" : "false",
           e1 ? MmsValue_toInt32(e1) : 0);
    fflush(stdout);
}

/* Emit a GetVariableAccessAttributes result. */
static void jl_var_access_attr(const char* target, const char* type_name)
{
    printf("{\"operation\":\"get-var-access-attr\",\"target\":\"%s\","
           "\"ok\":true,\"type\":\"%s\"}\n", target, type_name);
    fflush(stdout);
}

/* Emit a multi-variable or NVL read result.  op is "read-multiple" or "read-nvl". */
static void jl_values_array(const char* op, const char* target, MmsValue* arr)
{
    int n = MmsValue_getArraySize(arr);
    printf("{\"operation\":\"%s\",\"target\":\"%s\",\"ok\":true,\"values\":[",
           op, target);
    for (int i = 0; i < n; i++) {
        MmsValue* v = MmsValue_getElement(arr, i);
        if (i > 0) putchar(',');
        if (!v) { printf("null"); continue; }
        switch (MmsValue_getType(v)) {
        case MMS_BOOLEAN:
            printf("%s", MmsValue_getBoolean(v) ? "true" : "false"); break;
        case MMS_INTEGER:
            printf("%" PRId32, MmsValue_toInt32(v)); break;
        case MMS_UNSIGNED:
            printf("%" PRIu32, MmsValue_toUint32(v)); break;
        case MMS_FLOAT:
            printf("%g", (double)MmsValue_toFloat(v)); break;
        default:
            printf("null"); break;
        }
    }
    printf("]}\n");
    fflush(stdout);
}

/* -------------------------------------------------------------------------
 * MmsError / MmsDataAccessError → string
 * ---------------------------------------------------------------------- */

static const char* mms_error_str(MmsError e)
{
    switch (e) {
    case MMS_ERROR_NONE:             return "none";
    case MMS_ERROR_CONNECTION_LOST:  return "connection-lost";
    case MMS_ERROR_SERVICE_TIMEOUT:  return "service-timeout";
    default:                         return "mms-error";
    }
}

static const char* dae_str(MmsDataAccessError e)
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

/* -------------------------------------------------------------------------
 * Read / Write helper utilities
 * ---------------------------------------------------------------------- */

/*
 * check_read_val: verify that a value from MmsConnection_readVariable is a
 * real data value (not a per-variable DataAccessError).
 *
 * Returns val on success. On error, emits jl_fail_target and returns NULL
 * (val is freed if it was non-NULL).
 *
 * MmsConnection_readVariable returns a non-NULL MmsValue of type
 * MMS_DATA_ACCESS_ERROR for per-variable errors (mmsError stays NONE).
 */
static MmsValue* check_read_val(MmsValue* val, MmsError mmsErr,
                                const char* target)
{
    if (mmsErr != MMS_ERROR_NONE || !val) {
        jl_fail_target("read", target, mms_error_str(mmsErr));
        if (val) MmsValue_delete(val);
        return NULL;
    }
    if (MmsValue_getType(val) == MMS_DATA_ACCESS_ERROR) {
        jl_fail_target("read", target,
                       dae_str(MmsValue_getDataAccessError(val)));
        MmsValue_delete(val);
        return NULL;
    }
    return val;
}

/*
 * emit_write_result: emit the correct JSON-Lines write result.
 *
 * For single-variable writes, MmsConnection_writeVariable sets BOTH mmsError
 * (to the corresponding MmsError code) AND returns a MmsDataAccessError.
 * We prefer dae_str when dae is a known MMS data-access-error (0–11) because
 * it gives a semantically specific label like "object-access-denied".
 */
static void emit_write_result(const char* target, MmsError mmsErr,
                              MmsDataAccessError dae)
{
    if (dae == DATA_ACCESS_ERROR_SUCCESS && mmsErr == MMS_ERROR_NONE) {
        jl_ok_target("write", target);
    } else if (dae >= 0 && dae <= 11) {
        /* Known MMS data-access-error wire value (0–11): emit its string label.
         * MmsDataAccessError has negative sentinels (SUCCESS=-1, NO_RESPONSE=-2)
         * which must not be treated as valid wire values. */
        jl_fail_target("write", target, dae_str(dae));
    } else {
        jl_fail_target("write", target, mms_error_str(mmsErr));
    }
}

/* MmsVariableSpecification type → short string for JSON. */
static const char* spec_type_str(MmsVariableSpecification* spec)
{
    if (!spec) return "unknown";
    switch (spec->type) {
    case MMS_BOOLEAN:        return "boolean";
    case MMS_INTEGER:        return "integer";
    case MMS_UNSIGNED:       return "unsigned";
    case MMS_FLOAT:          return "float32";
    case MMS_VISIBLE_STRING: return "visible-string";
    case MMS_OCTET_STRING:   return "octet-string";
    case MMS_BIT_STRING:     return "bit-string";
    case MMS_UTC_TIME:       return "utc-time";
    case MMS_ARRAY:          return "array";
    case MMS_STRUCTURE:      return "structure";
    default:                 return "unknown";
    }
}

/* -------------------------------------------------------------------------
 * Single connection run
 * ---------------------------------------------------------------------- */

/*
 * run_sequence: connect, execute the operation sequence, conclude.
 *
 * full_sequence = 1: Phase 1A + Phase 1C (all operations)
 * full_sequence = 0: reconnect verification (read bool + conclude only)
 *
 * Returns 0 on success, 2 on connection failure.
 */
static int run_sequence(const char* host, int port, int full_sequence)
{
    MmsError mmsError = MMS_ERROR_NONE;
    MmsConnection conn = MmsConnection_create();
    if (!conn) {
        jl_log("MmsConnection_create failed");
        return 1;
    }
    if (full_sequence)
        services_install_report_handler(conn);

    MmsConnection_connect(conn, &mmsError, host, port);
    if (mmsError != MMS_ERROR_NONE) {
        jl_log("connect to %s:%d failed: %s", host, port, mms_error_str(mmsError));
        MmsConnection_destroy(conn);
        return 2;
    }
    jl_log("connected to %s:%d", host, port);

    /* ================================================================
     * Phase 1A operations
     * ============================================================== */

    /* --- Identify --- */
    if (full_sequence) {
        MmsServerIdentity* id = MmsConnection_identify(conn, &mmsError);
        if (mmsError != MMS_ERROR_NONE || !id)
            jl_fail("identify", mms_error_str(mmsError));
        else {
            jl_identify(id->vendorName, id->modelName, id->revision);
            MmsServerIdentity_destroy(id);
        }

        /* --- GetNameList: domains --- */
        LinkedList domains = MmsConnection_getDomainNames(conn, &mmsError);
        if (mmsError != MMS_ERROR_NONE || !domains)
            jl_fail("get-name-list", mms_error_str(mmsError));
        else {
            jl_namelist(NULL, domains);
            LinkedList_destroy(domains);
        }

        /* --- GetNameList: variables in "interop" --- */
        LinkedList vars = MmsConnection_getDomainVariableNames(
            conn, &mmsError, "interop");
        if (mmsError != MMS_ERROR_NONE || !vars)
            jl_fail_target("get-name-list", "interop", mms_error_str(mmsError));
        else {
            jl_namelist("interop", vars);
            LinkedList_destroy(vars);
        }
    }

    /* --- Read boolean --- */
    {
        MmsValue* val = MmsConnection_readVariable(
            conn, &mmsError, "interop", "boolean");
        if (mmsError != MMS_ERROR_NONE || !val)
            jl_fail_target("read", "interop/boolean", mms_error_str(mmsError));
        else {
            jl_read_bool("interop/boolean", MmsValue_getBoolean(val));
            MmsValue_delete(val);
        }
    }

    if (full_sequence) {
        /* --- Read integer --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "integer");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/integer", mms_error_str(mmsError));
            else {
                jl_read_int("interop/integer", MmsValue_toInt32(val));
                MmsValue_delete(val);
            }
        }

        /* --- Read float32 --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "float32");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/float32", mms_error_str(mmsError));
            else {
                jl_read_float("interop/float32", MmsValue_toFloat(val));
                MmsValue_delete(val);
            }
        }

        /* --- Write float32 = 99.0 --- */
        {
            MmsValue* wval = MmsValue_newFloat(99.0f);
            MmsDataAccessError dae = MmsConnection_writeVariable(
                conn, &mmsError, "interop", "float32", wval);
            MmsValue_delete(wval);
            emit_write_result("interop/float32", mmsError, dae);
        }

        /* --- Read float32 (verify write) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "float32");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/float32", mms_error_str(mmsError));
            else {
                jl_read_float("interop/float32", MmsValue_toFloat(val));
                MmsValue_delete(val);
            }
        }

        /* --- Write octet-string (expect rejection — read-only) --- */
        {
            static const uint8_t reject_bytes[] = {0xde, 0xad};
            MmsValue* wval = MmsValue_newOctetString(2, 2);
            MmsValue_setOctetString(wval, reject_bytes, 2);
            MmsDataAccessError dae = MmsConnection_writeVariable(
                conn, &mmsError, "interop", "octet-string", wval);
            MmsValue_delete(wval);
            emit_write_result("interop/octet-string", mmsError, dae);
        }

        /* --- Read boolean (verify server still connected after rejection) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "boolean");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/boolean", mms_error_str(mmsError));
            else {
                jl_read_bool("interop/boolean", MmsValue_getBoolean(val));
                MmsValue_delete(val);
            }
        }

        /* ================================================================
         * Phase 1C — Step 5: remaining scalar types
         * ============================================================== */

        /* --- Read unsigned --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "unsigned");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/unsigned", mms_error_str(mmsError));
            else {
                jl_read_uint("interop/unsigned", MmsValue_toUint32(val));
                MmsValue_delete(val);
            }
        }

        /* --- Read visible-string (initial value) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "visible-string");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/visible-string", mms_error_str(mmsError));
            else {
                const char* s = MmsValue_toString(val);
                jl_read_string("interop/visible-string", s ? s : "");
                MmsValue_delete(val);
            }
        }

        /* --- Write visible-string = "hello" --- */
        {
            MmsValue* wval = MmsValue_newVisibleString("hello");
            MmsDataAccessError dae = MmsConnection_writeVariable(
                conn, &mmsError, "interop", "visible-string", wval);
            MmsValue_delete(wval);
            emit_write_result("interop/visible-string", mmsError, dae);
        }

        /* --- Read visible-string (verify write) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "visible-string");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/visible-string", mms_error_str(mmsError));
            else {
                const char* s = MmsValue_toString(val);
                jl_read_string("interop/visible-string", s ? s : "");
                MmsValue_delete(val);
            }
        }

        /* --- Read octet-string (value assertion) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "octet-string");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/octet-string", mms_error_str(mmsError));
            else {
                int len   = MmsValue_getOctetStringSize(val);
                uint8_t* buf = MmsValue_getOctetStringBuffer(val);
                jl_read_bytes("interop/octet-string", buf, len);
                MmsValue_delete(val);
            }
        }

        /* --- Read bit-string --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "bit-string");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/bit-string", mms_error_str(mmsError));
            else {
                jl_read_bits("interop/bit-string", val);
                MmsValue_delete(val);
            }
        }

        /* --- Read utc-time --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "utc-time");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/utc-time", mms_error_str(mmsError));
            else {
                jl_read_utctime("interop/utc-time", val);
                MmsValue_delete(val);
            }
        }

        /* --- Read array --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "array");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/array", mms_error_str(mmsError));
            else {
                jl_read_array_ints("interop/array", val);
                MmsValue_delete(val);
            }
        }

        /* --- Read structure --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "structure");
            if (mmsError != MMS_ERROR_NONE || !val)
                jl_fail_target("read", "interop/structure", mms_error_str(mmsError));
            else {
                jl_read_structure_bool_int("interop/structure", val);
                MmsValue_delete(val);
            }
        }

        /* ================================================================
         * Phase 1C — Step 6: data-model operations
         * ============================================================== */

        /* --- GetVariableAccessAttributes for "boolean" --- */
        {
            MmsVariableSpecification* spec =
                MmsConnection_getVariableAccessAttributes(
                    conn, &mmsError, "interop", "boolean");
            if (mmsError != MMS_ERROR_NONE || !spec)
                jl_fail_target("get-var-access-attr", "interop/boolean",
                               mms_error_str(mmsError));
            else {
                jl_var_access_attr("interop/boolean", spec_type_str(spec));
                MmsVariableSpecification_destroy(spec);
            }
        }

        /* --- Multi-variable read: boolean + integer in one request --- */
        {
            LinkedList varIds = LinkedList_create();
            LinkedList_add(varIds, (void*)"boolean");
            LinkedList_add(varIds, (void*)"integer");
            MmsValue* result = MmsConnection_readMultipleVariables(
                conn, &mmsError, "interop", varIds);
            LinkedList_destroyStatic(varIds);
            if (mmsError != MMS_ERROR_NONE || !result)
                jl_fail_target("read-multiple", "interop", mms_error_str(mmsError));
            else {
                jl_values_array("read-multiple", "interop", result);
                MmsValue_delete(result);
            }
        }

        /* --- Multi-variable write: boolean=false + integer=0 --- */
        {
            LinkedList varNames = LinkedList_create();
            LinkedList_add(varNames, (void*)"boolean");
            LinkedList_add(varNames, (void*)"integer");

            LinkedList writeVals = LinkedList_create();
            MmsValue* bval = MmsValue_newBoolean(false);
            MmsValue* ival = MmsValue_newIntegerFromInt32(0);
            LinkedList_add(writeVals, bval);
            LinkedList_add(writeVals, ival);

            LinkedList accessResults = NULL;
            MmsConnection_writeMultipleVariables(
                conn, &mmsError, "interop", varNames, writeVals, &accessResults);

            MmsValue_delete(bval);
            MmsValue_delete(ival);
            LinkedList_destroyStatic(varNames);
            LinkedList_destroyStatic(writeVals);

            if (mmsError != MMS_ERROR_NONE) {
                jl_fail_target("write-multiple", "interop", mms_error_str(mmsError));
                if (accessResults)
                    LinkedList_destroyDeep(accessResults,
                                          (LinkedListValueDeleteFunction)MmsValue_delete);
            } else {
                /* Check each per-variable result; NULL pointer = success */
                int ok = 1;
                if (accessResults) {
                    LinkedList elem = LinkedList_getNext(accessResults);
                    while (elem) {
                        MmsValue* rv = (MmsValue*) elem->data;
                        if (rv != NULL &&
                            MmsValue_getType(rv) == MMS_DATA_ACCESS_ERROR &&
                            MmsValue_getDataAccessError(rv) != DATA_ACCESS_ERROR_SUCCESS) {
                            ok = 0;
                        }
                        elem = LinkedList_getNext(elem);
                    }
                    LinkedList_destroyDeep(accessResults,
                                          (LinkedListValueDeleteFunction)MmsValue_delete);
                }
                if (ok)
                    jl_ok_target("write-multiple", "interop");
                else
                    jl_fail_target("write-multiple", "interop", "partial-failure");
            }
        }

        /* --- NVL: define "interop/testlist" = {boolean, integer} --- */
        {
            LinkedList varSpecs = LinkedList_create();
            /*
             * MmsVariableAccessSpecification_destroy always calls GLOBAL_FREEMEM on
             * domainId and itemId, so we must pass heap-allocated strings (strdup),
             * NOT string literals.
             */
            MmsVariableAccessSpecification* s1 =
                MmsVariableAccessSpecification_create(strdup("interop"), strdup("boolean"));
            MmsVariableAccessSpecification* s2 =
                MmsVariableAccessSpecification_create(strdup("interop"), strdup("integer"));
            LinkedList_add(varSpecs, s1);
            LinkedList_add(varSpecs, s2);

            MmsConnection_defineNamedVariableList(
                conn, &mmsError, "interop", "testlist", varSpecs);

            LinkedList_destroyDeep(varSpecs,
                (LinkedListValueDeleteFunction)MmsVariableAccessSpecification_destroy);

            if (mmsError != MMS_ERROR_NONE)
                jl_fail_target("define-nvl", "interop/testlist",
                               mms_error_str(mmsError));
            else
                jl_ok_target("define-nvl", "interop/testlist");
        }

        /* --- NVL read: read values of "interop/testlist" --- */
        {
            MmsValue* result = MmsConnection_readNamedVariableListValues(
                conn, &mmsError, "interop", "testlist", false);
            if (mmsError != MMS_ERROR_NONE || !result)
                jl_fail_target("read-nvl", "interop/testlist",
                               mms_error_str(mmsError));
            else {
                jl_values_array("read-nvl", "interop/testlist", result);
                MmsValue_delete(result);
            }
        }

        /* --- NVL delete: delete "interop/testlist" --- */
        {
            bool deleted = MmsConnection_deleteNamedVariableList(
                conn, &mmsError, "interop", "testlist");
            if (mmsError != MMS_ERROR_NONE || !deleted)
                jl_fail_target("delete-nvl", "interop/testlist",
                               mms_error_str(mmsError));
            else
                jl_ok_target("delete-nvl", "interop/testlist");
        }

        /* ================================================================
         * Phase 1C — Step 7: negative tests
         * ============================================================== */

        /* --- Read from unknown domain (expect failure) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "unknown-domain", "boolean");
            if (!check_read_val(val, mmsError, "unknown-domain/boolean")) {
                /* error was already emitted by check_read_val */
            } else {
                /* unexpected success */
                jl_ok_target("read", "unknown-domain/boolean");
                MmsValue_delete(val);
            }
            mmsError = MMS_ERROR_NONE;
        }

        /* --- Read unknown variable in known domain (expect failure) --- */
        {
            MmsValue* val = MmsConnection_readVariable(
                conn, &mmsError, "interop", "nonexistent");
            if (!check_read_val(val, mmsError, "interop/nonexistent")) {
                /* error was already emitted by check_read_val */
            } else {
                /* unexpected success */
                jl_ok_target("read", "interop/nonexistent");
                MmsValue_delete(val);
            }
            mmsError = MMS_ERROR_NONE;
        }

        /* --- Write wrong type: write boolean value to integer variable --- */
        {
            MmsValue* wval = MmsValue_newBoolean(true);
            MmsDataAccessError dae = MmsConnection_writeVariable(
                conn, &mmsError, "interop", "integer", wval);
            MmsValue_delete(wval);
            emit_write_result("interop/integer-wrong-type", mmsError, dae);
        }

        /* ================================================================
         * Fixture mms-v2 services (mms_client_services.c)
         * ============================================================== */
        run_services(conn);
        mmsError = MMS_ERROR_NONE;
    }

    /* --- Conclude --- */
    MmsConnection_conclude(conn, &mmsError);
    if (mmsError != MMS_ERROR_NONE)
        jl_fail("conclude", mms_error_str(mmsError));
    else
        jl_ok("conclude");

    MmsConnection_destroy(conn);
    return 0;
}

/* -------------------------------------------------------------------------
 * Entry point
 * ---------------------------------------------------------------------- */

int main(int argc, char* argv[])
{
    jl_handle_meta_flags(argc, argv, "libiec61850");
    const char* host = "127.0.0.1";
    int         port = 1102;

    for (int i = 1; i < argc - 1; i++) {
        if (strcmp(argv[i], "--host") == 0) host = argv[i + 1];
        if (strcmp(argv[i], "--port") == 0) port = atoi(argv[i + 1]);
    }

    jl_log("connecting to %s:%d", host, port);

    /* Connection 1: full Phase 1A + 1C sequence */
    int rc = run_sequence(host, port, 1);
    if (rc == 2)
        jl_fatal(2, "fatal: connection 1 failed to connect to %s:%d", host, port);
    if (rc != 0)
        return rc;

    /* Connection 2: reconnect verification */
    rc = run_sequence(host, port, 0);
    if (rc == 2)
        jl_fatal(2, "fatal: connection 2 (reconnect) failed to connect to %s:%d",
                 host, port);
    if (rc != 0)
        return rc;

    /* Connection 3: abort instead of conclude */
    run_abort(host, port);

    /* Connection 4: server-initiated abort */
    run_server_abort(host, port);
    return 0;
}
