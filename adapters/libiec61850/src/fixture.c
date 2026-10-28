/* SPDX-License-Identifier: MIT */
/*
 * fixture.c — parse an iec61850-interop fixture JSON file.
 *
 * Supported variable types: boolean, integer, unsigned, float32,
 * visible-string, octet-string, bit-string, utc-time, array, structure.
 *
 * Encoding norms (from PLAN.md §Fixture encoding norms):
 *   octet-string: lowercase hexadecimal, no prefix, even digit count.
 *   bit-string:   MSB-first bit sequence; length == strlen(value).
 *   utc-time:     ISO 8601 UTC with Z suffix.
 *   array:        homogeneous element type given by "elementType"; initial
 *                 values in "value" JSON array; element count in "count".
 *   structure:    ordered anonymous components in "value" JSON array; each
 *                 component is {"type":"...","value":...}.
 */

#include "fixture.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <errno.h>

#include <json-c/json.h>
#include "mms_value.h"
#include "mms_types.h"

/* -------------------------------------------------------------------------
 * Hex helpers
 * ---------------------------------------------------------------------- */

static int hex_char(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static uint8_t* hex_decode(const char* s, int* out_len)
{
    int slen = (int)strlen(s);
    if (slen % 2 != 0) return NULL;
    int n = slen / 2;
    uint8_t* buf = (uint8_t*)malloc(n ? n : 1);
    if (!buf) return NULL;
    for (int i = 0; i < n; i++) {
        int hi = hex_char(s[2 * i]);
        int lo = hex_char(s[2 * i + 1]);
        if (hi < 0 || lo < 0) { free(buf); return NULL; }
        buf[i] = (uint8_t)((hi << 4) | lo);
    }
    *out_len = n;
    return buf;
}

/* -------------------------------------------------------------------------
 * UTC-time parsing  (RFC 3339 — "2024-01-01T00:00:00.000Z")
 * ---------------------------------------------------------------------- */

static uint64_t parse_utc_ms(const char* s)
{
    struct tm tm;
    memset(&tm, 0, sizeof(tm));
    unsigned int ms = 0;

    /* try with milliseconds first */
    int n = sscanf(s, "%4d-%2d-%2dT%2d:%2d:%2d.%3uZ",
                   &tm.tm_year, &tm.tm_mon, &tm.tm_mday,
                   &tm.tm_hour, &tm.tm_min, &tm.tm_sec, &ms);
    if (n < 6) {
        /* try without milliseconds */
        n = sscanf(s, "%4d-%2d-%2dT%2d:%2d:%2dZ",
                   &tm.tm_year, &tm.tm_mon, &tm.tm_mday,
                   &tm.tm_hour, &tm.tm_min, &tm.tm_sec);
        if (n < 6) return 0;
    }
    tm.tm_year -= 1900;
    tm.tm_mon  -= 1;
    tm.tm_isdst = 0;

#if defined(_WIN32)
    time_t t = _mkgmtime(&tm);
#else
    time_t t = timegm(&tm);
#endif
    return (uint64_t)t * 1000ULL + ms;
}

/* -------------------------------------------------------------------------
 * MmsVariableSpecification factory
 * ---------------------------------------------------------------------- */

static MmsVariableSpecification* make_spec(const char* type, int size)
{
    MmsVariableSpecification* s =
        (MmsVariableSpecification*)calloc(1, sizeof(MmsVariableSpecification));
    if (!s) return NULL;

    if (strcmp(type, "boolean") == 0) {
        s->type = MMS_BOOLEAN;
    } else if (strcmp(type, "integer") == 0) {
        s->type = MMS_INTEGER;
        s->typeSpec.integer = (size > 0) ? size : 32;
    } else if (strcmp(type, "unsigned") == 0) {
        s->type = MMS_UNSIGNED;
        s->typeSpec.unsignedInteger = (size > 0) ? size : 32;
    } else if (strcmp(type, "float32") == 0) {
        s->type = MMS_FLOAT;
        s->typeSpec.floatingpoint.exponentWidth = 8;
        s->typeSpec.floatingpoint.formatWidth   = 32;
    } else if (strcmp(type, "visible-string") == 0) {
        s->type = MMS_VISIBLE_STRING;
        s->typeSpec.visibleString = (size > 0) ? size : -1;
    } else if (strcmp(type, "octet-string") == 0) {
        s->type = MMS_OCTET_STRING;
        s->typeSpec.octetString = (size > 0) ? size : -1;
    } else if (strcmp(type, "bit-string") == 0) {
        s->type = MMS_BIT_STRING;
        s->typeSpec.bitString = (size > 0) ? size : -1;
    } else if (strcmp(type, "utc-time") == 0) {
        s->type = MMS_UTC_TIME;
    } else if (strcmp(type, "array") == 0) {
        /* Array: element type and count provided by caller after spec creation. */
        s->type = MMS_ARRAY;
        /* typeSpec.array is populated after this call once element type is known. */
    } else if (strcmp(type, "structure") == 0) {
        s->type = MMS_STRUCTURE;
        /* typeSpec.structure is populated after this call once components are known. */
    } else {
        fprintf(stderr, "fixture: unsupported type '%s', skipping\n", type);
        free(s);
        return NULL;
    }
    return s;
}

/* -------------------------------------------------------------------------
 * MmsValue factory
 * ---------------------------------------------------------------------- */

static MmsValue* make_value(const char* type, struct json_object* jval)
{
    if (strcmp(type, "boolean") == 0) {
        bool b = json_object_get_boolean(jval);
        return MmsValue_newBoolean(b);
    }
    if (strcmp(type, "integer") == 0) {
        int32_t i = (int32_t)json_object_get_int64(jval);
        return MmsValue_newIntegerFromInt32(i);
    }
    if (strcmp(type, "unsigned") == 0) {
        uint32_t u = (uint32_t)json_object_get_int64(jval);
        return MmsValue_newUnsignedFromUint32(u);
    }
    if (strcmp(type, "float32") == 0) {
        double d = json_object_get_double(jval);
        return MmsValue_newFloat((float)d);
    }
    if (strcmp(type, "visible-string") == 0) {
        const char* str = json_object_get_string(jval);
        return MmsValue_newVisibleString(str ? str : "");
    }
    if (strcmp(type, "octet-string") == 0) {
        const char* hex = json_object_get_string(jval);
        int len = 0;
        uint8_t* buf = hex_decode(hex ? hex : "", &len);
        MmsValue* mv = MmsValue_newOctetString(len, len);
        if (buf && len > 0) MmsValue_setOctetString(mv, buf, len);
        free(buf);
        return mv;
    }
    if (strcmp(type, "bit-string") == 0) {
        const char* bits = json_object_get_string(jval);
        if (!bits) bits = "";
        int blen = (int)strlen(bits);
        MmsValue* mv = MmsValue_newBitString(blen);
        for (int i = 0; i < blen; i++) {
            MmsValue_setBitStringBit(mv, i, bits[i] == '1');
        }
        return mv;
    }
    if (strcmp(type, "utc-time") == 0) {
        const char* ts = json_object_get_string(jval);
        uint64_t ms = parse_utc_ms(ts ? ts : "");
        return MmsValue_newUtcTimeByMsTime(ms);
    }
    /* array and structure are handled by the caller (fixture_load) */
    return NULL;
}

/* -------------------------------------------------------------------------
 * Array and structure helpers
 * ---------------------------------------------------------------------- */

/*
 * build_array_spec: build an MmsVariableSpecification for an array variable.
 * elem_type_str: element type (e.g. "integer"); count: number of elements.
 */
static MmsVariableSpecification* build_array_spec(const char* name,
                                                   const char* elem_type_str,
                                                   int count, int elem_size)
{
    MmsVariableSpecification* elem_spec = make_spec(elem_type_str, elem_size);
    if (!elem_spec) return NULL;

    MmsVariableSpecification* s =
        (MmsVariableSpecification*)calloc(1, sizeof(MmsVariableSpecification));
    if (!s) { free(elem_spec); return NULL; }

    s->name                              = name ? strdup(name) : NULL;
    s->type                              = MMS_ARRAY;
    s->typeSpec.array.elementTypeSpec    = elem_spec;
    s->typeSpec.array.elementCount       = count;
    return s;
}

/* build_array_value: build an MmsValue of type MMS_ARRAY from a JSON array. */
static MmsValue* build_array_value(const char* elem_type_str,
                                   struct json_object* jarr)
{
    if (!json_object_is_type(jarr, json_type_array)) return NULL;
    int n = (int)json_object_array_length(jarr);
    MmsValue* arr = MmsValue_createEmptyArray(n);
    if (!arr) return NULL;

    for (int i = 0; i < n; i++) {
        struct json_object* jelem = json_object_array_get_idx(jarr, i);
        MmsValue* elem = make_value(elem_type_str, jelem);
        if (!elem) { MmsValue_delete(arr); return NULL; }
        MmsValue_setElement(arr, i, elem);
    }
    return arr;
}

/*
 * build_structure_spec: build an MmsVariableSpecification for a structure.
 * jcomps: JSON array of {"type":"...","value":...} component objects.
 *
 * Components are given synthetic names "comp0", "comp1", etc. because
 * CONFIG_MMS_SUPPORT_FLATTED_NAME_SPACE=1 requires component names for
 * GetNameList (it recursively enumerates "structure/comp0", etc.).
 * Our fixture uses anonymous components, so synthetic names are the only option.
 */
static MmsVariableSpecification* build_structure_spec(const char* name,
                                                       struct json_object* jcomps)
{
    if (!json_object_is_type(jcomps, json_type_array)) return NULL;
    int n = (int)json_object_array_length(jcomps);

    MmsVariableSpecification** comps =
        (MmsVariableSpecification**)calloc(n, sizeof(MmsVariableSpecification*));
    if (!comps) return NULL;

    for (int i = 0; i < n; i++) {
        struct json_object* jc = json_object_array_get_idx(jcomps, i);
        struct json_object* jt = NULL;
        if (!json_object_object_get_ex(jc, "type", &jt)) { free(comps); return NULL; }
        const char* ct = json_object_get_string(jt);
        struct json_object* jsz = NULL;
        int csz = 0;
        if (json_object_object_get_ex(jc, "size", &jsz)) csz = json_object_get_int(jsz);
        comps[i] = make_spec(ct, csz);
        if (!comps[i]) {
            for (int j = 0; j < i; j++) { free(comps[j]->name); free(comps[j]); }
            free(comps);
            return NULL;
        }
        /* libiec61850's flattened name-space support
         * (CONFIG_MMS_SUPPORT_FLATTED_NAME_SPACE=1) requires component
         * names; components without a "name" get comp0, comp1, ... */
        struct json_object* jn = NULL;
        char name_buf[16];
        snprintf(name_buf, sizeof(name_buf), "comp%d", i);
        if (json_object_object_get_ex(jc, "name", &jn))
            comps[i]->name = strdup(json_object_get_string(jn));
        else
            comps[i]->name = strdup(name_buf);
        if (!comps[i]->name) {
            for (int j = 0; j <= i; j++) { free(comps[j]->name); free(comps[j]); }
            free(comps);
            return NULL;
        }
    }

    MmsVariableSpecification* s =
        (MmsVariableSpecification*)calloc(1, sizeof(MmsVariableSpecification));
    if (!s) {
        for (int i = 0; i < n; i++) { free(comps[i]->name); free(comps[i]); }
        free(comps);
        return NULL;
    }
    s->name                              = name ? strdup(name) : NULL;
    s->type                              = MMS_STRUCTURE;
    s->typeSpec.structure.elements       = comps;
    s->typeSpec.structure.elementCount   = n;
    return s;
}

/* build_structure_value: build an MmsValue of type MMS_STRUCTURE. */
static MmsValue* build_structure_value(struct json_object* jcomps)
{
    if (!json_object_is_type(jcomps, json_type_array)) return NULL;
    int n = (int)json_object_array_length(jcomps);

    MmsValue* sv = MmsValue_createEmptyStructure(n);
    if (!sv) return NULL;

    for (int i = 0; i < n; i++) {
        struct json_object* jc  = json_object_array_get_idx(jcomps, i);
        struct json_object* jt  = NULL, *jv = NULL;
        if (!json_object_object_get_ex(jc, "type",  &jt) ||
            !json_object_object_get_ex(jc, "value", &jv)) {
            MmsValue_delete(sv);
            return NULL;
        }
        const char* ct = json_object_get_string(jt);
        MmsValue* elem = make_value(ct, jv);
        if (!elem) { MmsValue_delete(sv); return NULL; }
        MmsValue_setElement(sv, i, elem);
    }
    return sv;
}

static int json_count_keys(struct json_object* obj)
{
    int n = 0;
    json_object_object_foreach(obj, k, v) { (void)k; (void)v; n++; }
    return n;
}

static bool load_journal(JournalDef* j, const char* name, struct json_object* jobj)
{
    j->name = strdup(name);
    struct json_object* jentries = NULL;
    if (!json_object_object_get_ex(jobj, "entries", &jentries) ||
        !json_object_is_type(jentries, json_type_array)) {
        fprintf(stderr, "fixture: journal '%s' missing 'entries' array\n", name);
        return false;
    }
    int n = (int)json_object_array_length(jentries);
    j->entries = (JournalEntryDef*)calloc(n ? n : 1, sizeof(JournalEntryDef));
    for (int i = 0; i < n; i++) {
        struct json_object* je = json_object_array_get_idx(jentries, i);
        struct json_object *jid = NULL, *jt = NULL, *jtag = NULL, *jtype = NULL, *jv = NULL, *jrc = NULL;
        if (!json_object_object_get_ex(je, "entryId", &jid) ||
            !json_object_object_get_ex(je, "occurrenceTime", &jt) ||
            !json_object_object_get_ex(je, "tag", &jtag) ||
            !json_object_object_get_ex(je, "type", &jtype) ||
            !json_object_object_get_ex(je, "value", &jv)) {
            fprintf(stderr, "fixture: journal '%s' entry %d incomplete\n", name, i);
            return false;
        }
        JournalEntryDef* e = &j->entries[j->entry_count];
        e->entry_id = (uint64_t)json_object_get_int64(jid);
        e->time_ms  = parse_utc_ms(json_object_get_string(jt));
        e->tag      = strdup(json_object_get_string(jtag));
        e->value    = make_value(json_object_get_string(jtype), jv);
        if (json_object_object_get_ex(je, "reasonCode", &jrc))
            e->reason_code = (uint8_t)json_object_get_int(jrc);
        if (!e->value) {
            fprintf(stderr, "fixture: journal '%s' entry %d: bad value\n", name, i);
            free(e->tag);
            return false;
        }
        j->entry_count++;
    }
    return true;
}

static bool load_file(FileDef* fd, const char* name, struct json_object* jobj)
{
    struct json_object *jc = NULL, *jhex = NULL, *jsize = NULL, *jlm = NULL;
    fd->name = strdup(name);
    if (json_object_object_get_ex(jobj, "content", &jc)) {
        const char* s = json_object_get_string(jc);
        fd->size = (int)strlen(s);
        fd->data = (uint8_t*)malloc(fd->size ? fd->size : 1);
        memcpy(fd->data, s, fd->size);
    } else if (json_object_object_get_ex(jobj, "hex", &jhex)) {
        fd->data = hex_decode(json_object_get_string(jhex), &fd->size);
        if (!fd->data) {
            fprintf(stderr, "fixture: file '%s': bad hex\n", name);
            return false;
        }
    } else if (json_object_object_get_ex(jobj, "size", &jsize)) {
        fd->size = json_object_get_int(jsize);
        fd->data = (uint8_t*)malloc(fd->size ? fd->size : 1);
        for (int i = 0; i < fd->size; i++)
            fd->data[i] = (uint8_t)(i % 251);
    } else {
        fprintf(stderr, "fixture: file '%s' needs content, hex or size\n", name);
        return false;
    }
    if (json_object_object_get_ex(jobj, "lastModified", &jlm))
        fd->last_modified_ms = parse_utc_ms(json_object_get_string(jlm));
    return true;
}

static bool load_report(ReportDef* r, struct json_object* jobj)
{
    struct json_object *jl = NULL, *jvars = NULL;
    if (json_object_object_get_ex(jobj, "vmdList", &jl))
        r->vmd_list = strdup(json_object_get_string(jl));
    if (!json_object_object_get_ex(jobj, "variables", &jvars) ||
        !json_object_is_type(jvars, json_type_array)) {
        fprintf(stderr, "fixture: informationReport missing 'variables'\n");
        return false;
    }
    int n = (int)json_object_array_length(jvars);
    r->variables = (char**)calloc(n ? n : 1, sizeof(char*));
    for (int i = 0; i < n; i++)
        r->variables[i] = strdup(json_object_get_string(json_object_array_get_idx(jvars, i)));
    r->var_count = n;
    return true;
}

FixtureDef* fixture_load(const char* path)
{
    struct json_object* root = json_object_from_file(path);
    if (!root) {
        fprintf(stderr, "fixture: cannot parse '%s': %s\n", path,
                json_util_get_last_err());
        return NULL;
    }

    FixtureDef* f = (FixtureDef*)calloc(1, sizeof(FixtureDef));
    if (!f) { json_object_put(root); return NULL; }

    /* identity */
    struct json_object* jid = NULL;
    if (json_object_object_get_ex(root, "identity", &jid)) {
        struct json_object* jv = NULL;
        if (json_object_object_get_ex(jid, "vendor",   &jv)) f->vendor   = strdup(json_object_get_string(jv));
        if (json_object_object_get_ex(jid, "model",    &jv)) f->model    = strdup(json_object_get_string(jv));
        if (json_object_object_get_ex(jid, "revision", &jv)) f->revision = strdup(json_object_get_string(jv));
    }
    if (!f->vendor)   f->vendor   = strdup("OTFabric");
    if (!f->model)    f->model    = strdup("iec61850-interop");
    if (!f->revision) f->revision = strdup("1.0");

    struct json_object* jrev = NULL;
    if (json_object_object_get_ex(root, "revision", &jrev))
        f->fixture_revision = strdup(json_object_get_string(jrev));
    else
        f->fixture_revision = strdup("mms-v1");

    struct json_object* jstatus = NULL;
    if (json_object_object_get_ex(root, "status", &jstatus)) {
        struct json_object* jv = NULL;
        if (json_object_object_get_ex(jstatus, "logical", &jv))  f->status_logical  = json_object_get_int(jv);
        if (json_object_object_get_ex(jstatus, "physical", &jv)) f->status_physical = json_object_get_int(jv);
    }

    struct json_object* jfiles = NULL;
    if (json_object_object_get_ex(root, "files", &jfiles)) {
        f->files = (FileDef*)calloc(json_count_keys(jfiles) + 1, sizeof(FileDef));
        json_object_object_foreach(jfiles, fname, fobj) {
            if (!load_file(&f->files[f->file_count], fname, fobj)) {
                json_object_put(root);
                fixture_free(f);
                return NULL;
            }
            f->file_count++;
        }
    }

    struct json_object* jreports = NULL;
    if (json_object_object_get_ex(root, "informationReports", &jreports) &&
        json_object_is_type(jreports, json_type_array)) {
        int n = (int)json_object_array_length(jreports);
        f->reports = (ReportDef*)calloc(n ? n : 1, sizeof(ReportDef));
        for (int i = 0; i < n; i++) {
            if (!load_report(&f->reports[i], json_object_array_get_idx(jreports, i))) {
                json_object_put(root);
                fixture_free(f);
                return NULL;
            }
            f->report_count++;
        }
    }

    /* domains */
    struct json_object* jdoms = NULL;
    if (!json_object_object_get_ex(root, "domains", &jdoms)) {
        fprintf(stderr, "fixture: missing 'domains' key\n");
        json_object_put(root);
        fixture_free(f);
        return NULL;
    }

    /* Count domains */
    int dom_count = 0;
    json_object_object_foreach(jdoms, dname, dval) {
        (void)dname; (void)dval;
        dom_count++;
    }
    f->domain_count = dom_count;
    f->domains = (DomainDef*)calloc(dom_count, sizeof(DomainDef));

    int di = 0;
    json_object_object_foreach(jdoms, dom_name, dom_obj) {
        DomainDef* d = &f->domains[di++];
        d->name = strdup(dom_name);

        struct json_object* jjournals = NULL;
        if (json_object_object_get_ex(dom_obj, "journals", &jjournals)) {
            d->journals = (JournalDef*)calloc(json_count_keys(jjournals) + 1, sizeof(JournalDef));
            json_object_object_foreach(jjournals, jname, jobj) {
                if (!load_journal(&d->journals[d->journal_count], jname, jobj)) {
                    json_object_put(root);
                    fixture_free(f);
                    return NULL;
                }
                d->journal_count++;
            }
        }

        struct json_object* jvars = NULL;
        if (!json_object_object_get_ex(dom_obj, "variables", &jvars)) continue;

        /* Count variables */
        int var_count = 0;
        json_object_object_foreach(jvars, vname, vval) {
            (void)vname; (void)vval;
            var_count++;
        }

        d->vars = (VarDef*)calloc(var_count, sizeof(VarDef));
        int vi = 0;

        json_object_object_foreach(jvars, var_name, var_obj) {
            struct json_object* jtype = NULL;
            if (!json_object_object_get_ex(var_obj, "type", &jtype)) {
                fprintf(stderr, "fixture: var '%s' missing 'type'\n", var_name);
                continue;
            }
            const char* type_str = json_object_get_string(jtype);

            struct json_object* jsize = NULL;
            int size = 0;
            if (json_object_object_get_ex(var_obj, "size", &jsize))
                size = json_object_get_int(jsize);

            struct json_object* jval = NULL;
            if (!json_object_object_get_ex(var_obj, "value", &jval)) {
                fprintf(stderr, "fixture: var '%s' missing 'value'\n", var_name);
                continue;
            }

            MmsVariableSpecification* spec = NULL;
            MmsValue* mv = NULL;

            if (strcmp(type_str, "array") == 0) {
                /* array: requires "elementType" and "count" in addition to "value" */
                struct json_object* jet = NULL, *jcount = NULL;
                if (!json_object_object_get_ex(var_obj, "elementType", &jet)) {
                    fprintf(stderr, "fixture: array '%s' missing 'elementType'\n", var_name);
                    continue;
                }
                const char* elem_type = json_object_get_string(jet);
                int count = 0;
                if (json_object_object_get_ex(var_obj, "count", &jcount))
                    count = json_object_get_int(jcount);
                if (count <= 0 && json_object_is_type(jval, json_type_array))
                    count = (int)json_object_array_length(jval);
                spec = build_array_spec(var_name, elem_type, count, 0);
                if (!spec) {
                    fprintf(stderr, "fixture: cannot build array spec for '%s'\n", var_name);
                    continue;
                }
                mv = build_array_value(elem_type, jval);
                if (!mv) {
                    fprintf(stderr, "fixture: cannot build array value for '%s'\n", var_name);
                    free(spec->name); free(spec);
                    continue;
                }
            } else if (strcmp(type_str, "structure") == 0) {
                /* structure: "value" is a JSON array of component {"type","value"} objects */
                spec = build_structure_spec(var_name, jval);
                if (!spec) {
                    fprintf(stderr, "fixture: cannot build structure spec for '%s'\n", var_name);
                    continue;
                }
                mv = build_structure_value(jval);
                if (!mv) {
                    fprintf(stderr, "fixture: cannot build structure value for '%s'\n", var_name);
                    free(spec->name); free(spec);
                    continue;
                }
            } else {
                spec = make_spec(type_str, size);
                if (!spec) continue; /* unsupported type — already warned */

                mv = make_value(type_str, jval);
                if (!mv) {
                    fprintf(stderr, "fixture: cannot create value for '%s'\n", var_name);
                    free(spec);
                    continue;
                }
                spec->name = strdup(var_name);
            }

            struct json_object* jw = NULL;
            bool writable = false;
            if (json_object_object_get_ex(var_obj, "writable", &jw))
                writable = json_object_get_boolean(jw);

            VarDef* v   = &d->vars[vi++];
            v->name     = strdup(var_name);
            v->spec     = spec;
            v->value    = mv;
            v->writable = writable;
        }
        d->var_count = vi;
    }

    json_object_put(root);
    return f;
}

void fixture_free(FixtureDef* f)
{
    if (!f) return;
    free(f->fixture_revision);
    free(f->vendor);
    free(f->model);
    free(f->revision);
    for (int i = 0; i < f->file_count; i++) {
        free(f->files[i].name);
        free(f->files[i].data);
    }
    free(f->files);
    for (int i = 0; i < f->report_count; i++) {
        free(f->reports[i].vmd_list);
        for (int k = 0; k < f->reports[i].var_count; k++)
            free(f->reports[i].variables[k]);
        free(f->reports[i].variables);
    }
    free(f->reports);
    for (int di = 0; di < f->domain_count; di++) {
        DomainDef* d = &f->domains[di];
        free(d->name);
        for (int ji = 0; ji < d->journal_count; ji++) {
            JournalDef* j = &d->journals[ji];
            free(j->name);
            for (int ei = 0; ei < j->entry_count; ei++) {
                free(j->entries[ei].tag);
                MmsValue_delete(j->entries[ei].value);
            }
            free(j->entries);
        }
        free(d->journals);
        for (int vi = 0; vi < d->var_count; vi++) {
            VarDef* v = &d->vars[vi];
            free(v->name);
            if (v->spec) {
                free(v->spec->name);
                /* Free nested specs for array and structure. */
                if (v->spec->type == MMS_ARRAY) {
                    MmsVariableSpecification* es = v->spec->typeSpec.array.elementTypeSpec;
                    if (es) { free(es->name); free(es); }
                } else if (v->spec->type == MMS_STRUCTURE) {
                    int nc = v->spec->typeSpec.structure.elementCount;
                    MmsVariableSpecification** comps = v->spec->typeSpec.structure.elements;
                    if (comps) {
                        for (int ci = 0; ci < nc; ci++) {
                            if (comps[ci]) { free(comps[ci]->name); free(comps[ci]); }
                        }
                        free(comps);
                    }
                }
                free(v->spec);
            }
            if (v->value) MmsValue_delete(v->value);
        }
        free(d->vars);
    }
    free(f->domains);
    free(f);
}
