/* SPDX-License-Identifier: MIT */
/* sv_common.c — see sv_common.h. */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "sv_common.h"

static const struct { const char* name; SvType type; int size; } k_types[] = {
    { "integer",    SV_INT32,     4 },
    { "unsigned",   SV_INT32U,    4 },
    { "float32",    SV_FLOAT32,   4 },
    { "float64",    SV_FLOAT64,   8 },
    { "boolean",    SV_BOOLEAN,   1 },
    { "bit-string", SV_QUALITY,   4 },
    { "utc-time",   SV_TIMESTAMP, 8 },
};

bool sv_type_from_name(const char* name, SvType* out)
{
    for (size_t i = 0; i < sizeof(k_types) / sizeof(k_types[0]); i++)
        if (!strcmp(name, k_types[i].name)) {
            *out = k_types[i].type;
            return true;
        }
    return false;
}

int sv_type_size(SvType t)
{
    for (size_t i = 0; i < sizeof(k_types) / sizeof(k_types[0]); i++)
        if (k_types[i].type == t) return k_types[i].size;
    return 0;
}

static const char* k_smp_mods[] = { "samples-per-period", "samples-per-second", "seconds-per-sample" };

bool sv_smp_mod_from_name(const char* name, uint8_t* out)
{
    for (uint8_t i = 0; i < 3; i++)
        if (!strcmp(name, k_smp_mods[i])) {
            *out = i;
            return true;
        }
    return false;
}

const char* sv_smp_mod_name(uint8_t v)
{
    return v < 3 ? k_smp_mods[v] : NULL;
}

uint16_t sv_quality_from_value(const MmsValue* bits)
{
    uint16_t q = 0;
    int n = MmsValue_getBitStringSize((MmsValue*) bits);
    for (int i = 0; i < n && i < 13; i++)
        if (MmsValue_getBitStringBit((MmsValue*) bits, i)) q |= (uint16_t) (1u << i);
    return q;
}

MmsValue* sv_quality_to_value(uint32_t word)
{
    MmsValue* v = MmsValue_newBitString(13);
    for (int i = 0; i < 13; i++)
        MmsValue_setBitStringBit(v, i, (word >> i) & 1);
    return v;
}

void sv_format_ns(uint64_t ns, char* buf)
{
    time_t t = (time_t) (ns / 1000000000ULL);
    uint64_t frac = ns % 1000000000ULL;
    struct tm tm;
    gmtime_r(&t, &tm);
    char base[32];
    strftime(base, sizeof(base), "%Y-%m-%dT%H:%M:%S", &tm);
    if (frac == 0) {
        sprintf(buf, "%sZ", base);
        return;
    }
    char frac_s[16];
    sprintf(frac_s, "%09llu", (unsigned long long) frac);
    for (int i = 8; i > 0 && frac_s[i] == '0'; i--) frac_s[i] = '\0';
    sprintf(buf, "%s.%sZ", base, frac_s);
}

void sv_emit_ready(const char* role, const char* iface, const uint8_t src[6], const char* sv_id)
{
    const char* version = getenv("ADAPTER_VERSION");
    char mac[18];
    goose_format_mac(src, mac);
    struct json_object* o = json_object_new_object();
    json_object_object_add(o, "event", json_object_new_string("ready"));
    json_object_object_add(o, "address", json_object_new_string(iface));
    json_object_object_add(o, "fixture", json_object_new_string(SV_FIXTURE_REVISION));
    json_object_object_add(o, "adapter", json_object_new_string("libiec61850"));
    json_object_object_add(o, "version", json_object_new_string(version ? version : "dev"));
    json_object_object_add(o, "role", json_object_new_string(role));
    json_object_object_add(o, "iface", json_object_new_string(iface));
    json_object_object_add(o, "src", json_object_new_string(mac));
    if (sv_id && sv_id[0])
        json_object_object_add(o, "svID", json_object_new_string(sv_id));
    goose_emit(o);
}
