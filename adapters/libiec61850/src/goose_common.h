/* SPDX-License-Identifier: MIT */
/*
 * goose_common.h — shared helpers of the libiec61850 GOOSE adapters:
 * typed JSON <-> MmsValue (the contract's value form) and the small
 * pieces of Ethernet framing the adapters handle themselves.
 */
#ifndef GOOSE_COMMON_H
#define GOOSE_COMMON_H

#include <stdbool.h>
#include <stdint.h>
#include <json-c/json.h>

#include "mms_value.h"
#include "linked_list.h"

#define GOOSE_FIXTURE_REVISION "goose-v1"

/* Edition 2 simulation bit: MSB of Reserved1, octet 4 of the APDU. */
#define GOOSE_SIM_OCTET 4
#define GOOSE_SIM_MASK  0x80

/* Typed JSON {"type":..,"value":..} -> new MmsValue; NULL and *err set on
 * failure (err is a static string). */
MmsValue* goose_value_from_json(struct json_object* typed, const char** err);

/* JSON array of typed values -> LinkedList of MmsValue (caller destroys
 * with LinkedList_destroyDeep(list, (LinkedListValueDeleteFunction) MmsValue_delete)). */
LinkedList goose_values_from_json(struct json_object* array, const char** err);

/* MmsValue -> new typed JSON object. */
struct json_object* goose_value_to_json(const MmsValue* v);

/* "0x1001" / "4097" -> APPID; false when not a 16-bit number. */
bool goose_parse_appid(const char* s, uint16_t* out);

/* "01:0c:cd:01:00:01" or with '-' -> 6 octets. */
bool goose_parse_mac(const char* s, uint8_t out[6]);

/* 6 octets -> "01:0c:cd:01:00:01" (buf >= 18). */
void goose_format_mac(const uint8_t mac[6], char* buf);

/* UtcTime octets -> RFC 3339 with nanoseconds ("...T..:..:..123456789Z");
 * the fraction converts as (frac * 1e9) >> 24. buf >= 40. */
void goose_format_utc_ns(const uint8_t raw[8], char* buf);

/* Emit a JSON object as one line on stdout and free it. */
void goose_emit(struct json_object* obj);

/* {"event":"ready",...} of a GOOSE command. */
void goose_emit_ready(const char* role, const char* iface, const uint8_t src[6], const char* gocb_ref);

/* {"event":"error","error":...}: a rejected stdin command or send failure. */
void goose_emit_error(const char* msg);

#endif /* GOOSE_COMMON_H */
