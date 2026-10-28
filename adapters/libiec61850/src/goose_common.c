/* SPDX-License-Identifier: MIT */
/*
 * goose_common.c — see goose_common.h.
 *
 * Value JSON (same as the MMS value contract):
 *   boolean, integer, unsigned, float32, float64  JSON boolean / number
 *   visible-string, mms-string                    JSON string
 *   octet-string                                  lower-case hex
 *   bit-string                                    MSB-first "0110..."
 *   utc-time, binary-time                         "YYYY-MM-DDThh:mm:ss.mmmZ"
 *   array, structure                              JSON array of typed values
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "goose_common.h"
#include "mms_value_internal.h"   /* float format width (private) */

static int hexval(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

/* "2024-01-01T00:00:01.500Z" (fraction optional, up to ns) -> ms since epoch. */
static bool parse_time_ms(const char* s, uint64_t* out)
{
    struct tm tm;
    memset(&tm, 0, sizeof(tm));
    const char* p = strptime(s, "%Y-%m-%dT%H:%M:%S", &tm);
    if (!p) return false;
    uint64_t ms = 0;
    if (*p == '.') {
        p++;
        int digits = 0;
        uint64_t ns = 0;
        while (*p >= '0' && *p <= '9') {
            if (digits < 9) { ns = ns * 10 + (uint64_t)(*p - '0'); digits++; }
            p++;
        }
        while (digits++ < 9) ns *= 10;
        ms = (ns + 500000) / 1000000;
    }
    if (*p != 'Z' || p[1] != '\0') return false;
    *out = (uint64_t) timegm(&tm) * 1000 + ms;
    return true;
}

static void format_time_ms(uint64_t ms, char* buf, size_t n)
{
    time_t secs = (time_t)(ms / 1000);
    struct tm tm;
    gmtime_r(&secs, &tm);
    char base[32];
    strftime(base, sizeof(base), "%Y-%m-%dT%H:%M:%S", &tm);
    snprintf(buf, n, "%s.%03uZ", base, (unsigned)(ms % 1000));
}

void goose_format_utc_ns(const uint8_t raw[8], char* buf)
{
    uint32_t secs = ((uint32_t)raw[0] << 24) | ((uint32_t)raw[1] << 16) |
                    ((uint32_t)raw[2] << 8) | raw[3];
    uint64_t frac = ((uint64_t)raw[4] << 16) | ((uint64_t)raw[5] << 8) | raw[6];
    uint64_t ns = (frac * 1000000000ULL) >> 24;
    time_t t = (time_t) secs;
    struct tm tm;
    gmtime_r(&t, &tm);
    char base[32];
    strftime(base, sizeof(base), "%Y-%m-%dT%H:%M:%S", &tm);
    if (ns == 0) {
        sprintf(buf, "%sZ", base);
        return;
    }
    /* RFC 3339 with the trailing zeros of the fraction removed, as Go's
     * time.RFC3339Nano writes it. */
    char frac_s[16];
    sprintf(frac_s, "%09llu", (unsigned long long) ns);
    for (int i = 8; i > 0 && frac_s[i] == '0'; i--) frac_s[i] = '\0';
    sprintf(buf, "%s.%sZ", base, frac_s);
}

/* UtcTime octets -> ms, rounded to the nearest ms. */
static uint64_t utc_ms_rounded(const uint8_t raw[8])
{
    uint32_t secs = ((uint32_t)raw[0] << 24) | ((uint32_t)raw[1] << 16) |
                    ((uint32_t)raw[2] << 8) | raw[3];
    uint64_t frac = ((uint64_t)raw[4] << 16) | ((uint64_t)raw[5] << 8) | raw[6];
    uint64_t ns = (frac * 1000000000ULL) >> 24;
    return (uint64_t) secs * 1000 + (ns + 500000) / 1000000;
}

MmsValue* goose_value_from_json(struct json_object* typed, const char** err)
{
    struct json_object *jt = NULL, *jv = NULL;
    if (!json_object_is_type(typed, json_type_object) ||
        !json_object_object_get_ex(typed, "type", &jt) ||
        !json_object_object_get_ex(typed, "value", &jv)) {
        *err = "value is not {\"type\":..,\"value\":..}";
        return NULL;
    }
    const char* type = json_object_get_string(jt);

    if (!strcmp(type, "boolean")) {
        if (!json_object_is_type(jv, json_type_boolean)) { *err = "boolean: not a JSON boolean"; return NULL; }
        return MmsValue_newBoolean(json_object_get_boolean(jv));
    }
    if (!strcmp(type, "integer")) {
        if (!json_object_is_type(jv, json_type_int)) { *err = "integer: not a JSON integer"; return NULL; }
        return MmsValue_newIntegerFromInt64(json_object_get_int64(jv));
    }
    if (!strcmp(type, "unsigned")) {
        if (!json_object_is_type(jv, json_type_int) || json_object_get_int64(jv) < 0 ||
            json_object_get_int64(jv) > 0xffffffffLL) {
            *err = "unsigned: not a 32-bit unsigned integer";
            return NULL;
        }
        return MmsValue_newUnsignedFromUint32((uint32_t) json_object_get_int64(jv));
    }
    if (!strcmp(type, "float32") || !strcmp(type, "float64")) {
        if (!json_object_is_type(jv, json_type_double) && !json_object_is_type(jv, json_type_int)) {
            *err = "float: not a JSON number";
            return NULL;
        }
        double d = json_object_get_double(jv);
        return type[5] == '3' ? MmsValue_newFloat((float) d) : MmsValue_newDouble(d);
    }
    if (!strcmp(type, "visible-string") || !strcmp(type, "mms-string")) {
        if (!json_object_is_type(jv, json_type_string)) { *err = "string: not a JSON string"; return NULL; }
        return type[0] == 'v' ? MmsValue_newVisibleString(json_object_get_string(jv))
                              : MmsValue_newMmsString(json_object_get_string(jv));
    }
    if (!strcmp(type, "octet-string")) {
        const char* s = json_object_get_string(jv);
        size_t n = s ? strlen(s) : 0;
        if (!json_object_is_type(jv, json_type_string) || n % 2) { *err = "octet-string: not hex"; return NULL; }
        uint8_t* buf = malloc(n / 2 + 1);
        for (size_t i = 0; i < n / 2; i++) {
            int hi = hexval(s[2 * i]), lo = hexval(s[2 * i + 1]);
            if (hi < 0 || lo < 0) { free(buf); *err = "octet-string: not hex"; return NULL; }
            buf[i] = (uint8_t)(hi << 4 | lo);
        }
        MmsValue* v = MmsValue_newOctetString((int)(n / 2), (int)(n / 2));
        MmsValue_setOctetString(v, buf, (int)(n / 2));
        free(buf);
        return v;
    }
    if (!strcmp(type, "bit-string")) {
        const char* s = json_object_get_string(jv);
        if (!json_object_is_type(jv, json_type_string)) { *err = "bit-string: not a string"; return NULL; }
        int n = (int) strlen(s);
        MmsValue* v = MmsValue_newBitString(n);
        for (int i = 0; i < n; i++) {
            if (s[i] != '0' && s[i] != '1') { MmsValue_delete(v); *err = "bit-string: not binary digits"; return NULL; }
            MmsValue_setBitStringBit(v, i, s[i] == '1');
        }
        return v;
    }
    if (!strcmp(type, "utc-time") || !strcmp(type, "binary-time")) {
        uint64_t ms;
        if (!json_object_is_type(jv, json_type_string) || !parse_time_ms(json_object_get_string(jv), &ms)) {
            *err = "time: not YYYY-MM-DDThh:mm:ss[.fff]Z";
            return NULL;
        }
        if (type[0] == 'u')
            return MmsValue_newUtcTimeByMsTime(ms);
        MmsValue* v = MmsValue_newBinaryTime(false);
        MmsValue_setBinaryTime(v, ms);
        return v;
    }
    if (!strcmp(type, "array") || !strcmp(type, "structure")) {
        if (!json_object_is_type(jv, json_type_array)) { *err = "array/structure: not a JSON array"; return NULL; }
        int n = (int) json_object_array_length(jv);
        MmsValue* v = type[0] == 'a' ? MmsValue_createEmptyArray(n) : MmsValue_createEmptyStructure(n);
        for (int i = 0; i < n; i++) {
            MmsValue* c = goose_value_from_json(json_object_array_get_idx(jv, i), err);
            if (!c) { MmsValue_delete(v); return NULL; }
            MmsValue_setElement(v, i, c);
        }
        return v;
    }
    *err = "unsupported type";
    return NULL;
}

LinkedList goose_values_from_json(struct json_object* array, const char** err)
{
    if (!array || !json_object_is_type(array, json_type_array)) {
        *err = "values is not a JSON array";
        return NULL;
    }
    LinkedList list = LinkedList_create();
    int n = (int) json_object_array_length(array);
    for (int i = 0; i < n; i++) {
        MmsValue* v = goose_value_from_json(json_object_array_get_idx(array, i), err);
        if (!v) {
            LinkedList_destroyDeep(list, (LinkedListValueDeleteFunction) MmsValue_delete);
            return NULL;
        }
        LinkedList_add(list, v);
    }
    return list;
}

static struct json_object* typed(const char* type, struct json_object* value)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "type", json_object_new_string(type));
    json_object_object_add(o, "value", value);
    return o;
}

struct json_object* goose_value_to_json(const MmsValue* v)
{
    char buf[64];
    if (!v) return typed("none", NULL);
    switch (MmsValue_getType(v)) {
    case MMS_BOOLEAN:
        return typed("boolean", json_object_new_boolean(MmsValue_getBoolean(v)));
    case MMS_INTEGER:
        return typed("integer", json_object_new_int64(MmsValue_toInt64(v)));
    case MMS_UNSIGNED:
        return typed("unsigned", json_object_new_int64((int64_t) MmsValue_toUint32(v)));
    case MMS_FLOAT:
        if (v->value.floatingPoint.formatWidth == 64)
            return typed("float64", json_object_new_double(MmsValue_toDouble(v)));
        /* float32 printed with the shortest form that round-trips, as Go does. */
        for (int prec = 1; prec <= 9; prec++) {
            snprintf(buf, sizeof(buf), "%.*g", prec, (double) MmsValue_toFloat(v));
            if ((float) strtod(buf, NULL) == MmsValue_toFloat(v)) break;
        }
        return typed("float32", json_object_new_double_s((double) MmsValue_toFloat(v), buf));
    case MMS_VISIBLE_STRING:
        return typed("visible-string", json_object_new_string(MmsValue_toString((MmsValue*) v)));
    case MMS_STRING:
        return typed("mms-string", json_object_new_string(MmsValue_toString((MmsValue*) v)));
    case MMS_OCTET_STRING: {
        int n = MmsValue_getOctetStringSize(v);
        char* hex = malloc((size_t) n * 2 + 1);
        uint8_t* b = MmsValue_getOctetStringBuffer((MmsValue*) v);
        for (int i = 0; i < n; i++) sprintf(hex + 2 * i, "%02x", b[i]);
        hex[2 * n] = '\0';
        struct json_object* o = typed("octet-string", json_object_new_string(hex));
        free(hex);
        return o;
    }
    case MMS_BIT_STRING: {
        int n = MmsValue_getBitStringSize(v);
        char* bits = malloc((size_t) n + 1);
        for (int i = 0; i < n; i++) bits[i] = MmsValue_getBitStringBit(v, i) ? '1' : '0';
        bits[n] = '\0';
        struct json_object* o = typed("bit-string", json_object_new_string(bits));
        free(bits);
        return o;
    }
    case MMS_UTC_TIME:
        format_time_ms(utc_ms_rounded(MmsValue_getUtcTimeBuffer((MmsValue*) v)), buf, sizeof(buf));
        return typed("utc-time", json_object_new_string(buf));
    case MMS_BINARY_TIME:
        format_time_ms(MmsValue_getBinaryTimeAsUtcMs(v), buf, sizeof(buf));
        return typed("binary-time", json_object_new_string(buf));
    case MMS_ARRAY:
    case MMS_STRUCTURE: {
        struct json_object* arr = json_object_new_array();
        int n = MmsValue_getArraySize(v);
        for (int i = 0; i < n; i++)
            json_object_array_add(arr, goose_value_to_json(MmsValue_getElement(v, i)));
        return typed(MmsValue_getType(v) == MMS_ARRAY ? "array" : "structure", arr);
    }
    case MMS_DATA_ACCESS_ERROR:
        return typed("data-access-error", json_object_new_int(MmsValue_getDataAccessError(v)));
    default:
        return typed("unknown", NULL);
    }
}

bool goose_parse_appid(const char* s, uint16_t* out)
{
    char* end = NULL;
    unsigned long n = strtoul(s, &end, 0);
    if (!s[0] || *end || n > 0xffff) return false;
    *out = (uint16_t) n;
    return true;
}

bool goose_parse_mac(const char* s, uint8_t out[6])
{
    unsigned int b[6];
    char sep1, sep2, sep3, sep4, sep5, tail;
    if (sscanf(s, "%2x%c%2x%c%2x%c%2x%c%2x%c%2x%c", &b[0], &sep1, &b[1], &sep2, &b[2], &sep3,
               &b[3], &sep4, &b[4], &sep5, &b[5], &tail) != 11)
        return false;
    for (int i = 0; i < 6; i++) out[i] = (uint8_t) b[i];
    return true;
}

void goose_format_mac(const uint8_t mac[6], char* buf)
{
    sprintf(buf, "%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
}

void goose_emit(struct json_object* obj)
{
    fputs(json_object_to_json_string_ext(obj, JSON_C_TO_STRING_PLAIN | JSON_C_TO_STRING_NOSLASHESCAPE), stdout);
    fputc('\n', stdout);
    fflush(stdout);
    json_object_put(obj);
}

void goose_emit_ready(const char* role, const char* iface, const uint8_t src[6], const char* gocb_ref)
{
    const char* version = getenv("ADAPTER_VERSION");
    char mac[18];
    goose_format_mac(src, mac);
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("ready"));
    json_object_object_add(o, "address", json_object_new_string(iface));
    json_object_object_add(o, "fixture", json_object_new_string(GOOSE_FIXTURE_REVISION));
    json_object_object_add(o, "adapter", json_object_new_string("libiec61850"));
    json_object_object_add(o, "version", json_object_new_string(version ? version : "dev"));
    json_object_object_add(o, "role", json_object_new_string(role));
    json_object_object_add(o, "iface", json_object_new_string(iface));
    json_object_object_add(o, "src", json_object_new_string(mac));
    if (gocb_ref && gocb_ref[0])
        json_object_object_add(o, "gocbRef", json_object_new_string(gocb_ref));
    goose_emit(o);
}

void goose_emit_error(const char* msg)
{
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("error"));
    json_object_object_add(o, "error", json_object_new_string(msg));
    goose_emit(o);
}
