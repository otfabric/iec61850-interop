/* SPDX-License-Identifier: MIT */
/*
 * sv_common.h — shared pieces of the libiec61850 Sampled Values adapters
 * (docs/sv.md). The typed JSON, MAC and APPID helpers are those of the
 * GOOSE adapters (goose_common.h).
 */
#ifndef SV_COMMON_H
#define SV_COMMON_H

#include <stdbool.h>
#include <stdint.h>

#include "goose_common.h"

#define SV_FIXTURE_REVISION "sv-v1"
#define SV_FIXTURE_PATH     "/fixtures/sv/sv.json"

/* Edition 2 simulation bit: MSB of Reserved1, octet 4 of the APDU (as GOOSE). */
#define SV_SIM_OCTET 4
#define SV_SIM_MASK  0x80

/* seqData member types (IEC 61850-9-2 fixed-length encoding). */
typedef enum {
    SV_INT32,    /* "integer"    4 octets */
    SV_INT32U,   /* "unsigned"   4 */
    SV_FLOAT32,  /* "float32"    4 */
    SV_FLOAT64,  /* "float64"    8 */
    SV_BOOLEAN,  /* "boolean"    1 */
    SV_QUALITY,  /* "bit-string" 4, the 13-bit quality */
    SV_TIMESTAMP /* "utc-time"   8 */
} SvType;

/* Contract type name -> SvType; false for a type SV cannot carry. */
bool sv_type_from_name(const char* name, SvType* out);

/* Octets of a member on the wire. */
int sv_type_size(SvType t);

/* smpMod name <-> wire value (samples-per-period 0, samples-per-second 1,
 * seconds-per-sample 2). */
bool sv_smp_mod_from_name(const char* name, uint8_t* out);
const char* sv_smp_mod_name(uint8_t v);

/* 13-bit quality: bit-string position i <-> bit i of the word (from the LSB). */
uint16_t sv_quality_from_value(const MmsValue* bits);
MmsValue* sv_quality_to_value(uint32_t word);

/* ns since the epoch -> RFC 3339 with up to nine fraction digits,
 * trailing zeros dropped (as goose_format_utc_ns). buf >= 40. */
void sv_format_ns(uint64_t ns, char* buf);

/* {"event":"ready",...} of an SV command; sv_id may be NULL. */
void sv_emit_ready(const char* role, const char* iface, const uint8_t src[6], const char* sv_id);

#endif /* SV_COMMON_H */
