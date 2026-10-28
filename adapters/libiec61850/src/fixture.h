/* SPDX-License-Identifier: MIT */
/*
 * fixture.h — load an mms-interop fixture JSON into a ready-to-use model.
 *
 * Each VarDef holds an allocated MmsVariableSpecification (type shape) and
 * an initial MmsValue.  Both are owned by the FixtureDef; call fixture_free()
 * when done.
 */
#ifndef FIXTURE_H
#define FIXTURE_H

#include <stdbool.h>
#include <stdint.h>
#include "mms_value.h"
#include "mms_types.h"

typedef struct {
    char*                   name;
    MmsVariableSpecification* spec;   /* owned */
    MmsValue*               value;   /* owned, initial value */
    bool                    writable;
} VarDef;

/* One journal entry: a single data variable plus libiec61850's ReasonCode. */
typedef struct {
    uint64_t  entry_id;
    uint64_t  time_ms;
    char*     tag;          /* owned */
    MmsValue* value;        /* owned */
    uint8_t   reason_code;
} JournalEntryDef;

typedef struct {
    char*            name;
    int              entry_count;
    JournalEntryDef* entries;  /* array of entry_count, ascending by time */
} JournalDef;

typedef struct {
    char*       name;
    int         var_count;
    VarDef*     vars;      /* array of var_count VarDef */
    int         journal_count;
    JournalDef* journals;  /* array of journal_count JournalDef */
} DomainDef;

/* A file of the virtual file store. name uses '/' as separator. */
typedef struct {
    char*    name;
    uint8_t* data;
    int      size;
    uint64_t last_modified_ms;
} FileDef;

/* An InformationReport sent when report-trigger is written. vmd_list is
 * NULL for the listOfVariable form. variables are "domain/item" strings. */
typedef struct {
    char*  vmd_list;
    int    var_count;
    char** variables;
} ReportDef;

typedef struct {
    char*       fixture_revision;
    char*       vendor;
    char*       model;
    char*       revision;
    int         status_logical;
    int         status_physical;
    int         domain_count;
    DomainDef*  domains;   /* array of domain_count DomainDef */
    int         file_count;
    FileDef*    files;
    int         report_count;
    ReportDef*  reports;
} FixtureDef;

/*
 * Parse the JSON fixture at `path`.  Returns NULL and prints an error to
 * stderr on failure.  The caller owns the returned FixtureDef and must call
 * fixture_free() when done.
 */
FixtureDef* fixture_load(const char* path);

void fixture_free(FixtureDef* f);

#endif /* FIXTURE_H */
