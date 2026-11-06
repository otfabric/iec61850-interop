/* SPDX-License-Identifier: MIT */
/*
 * ld_resolve.h — resolve the fixture's logical device from the server
 * directory (contract 1.3), shared by ied_reporter.c and ied_controller.c.
 *
 * The SCL ldInst is "InteropLD"; servers name the MMS domain either bare
 * ("InteropLD", go-iec61850) or IED-prefixed ("InteropIEDInteropLD",
 * libiec61850 / iec61850bean). Picks exactly "InteropLD", else the first
 * name ending in "InteropLD", else the first one listed.
 */
#ifndef LD_RESOLVE_H
#define LD_RESOLVE_H

#include <stdbool.h>
#include <stdio.h>
#include <string.h>

#include "iec61850_client.h"

#define LD_NAME "InteropLD"   /* SCL ldInst; the domain may carry an IED prefix */

/*
 * Write the resolved LD name to ld. Leaves LD_NAME when the directory cannot
 * be read (the first operation on it then reports the failure); prog
 * prefixes the stderr note.
 */
static inline void ld_resolve(IedConnection conn, const char* prog,
                              char* ld, size_t ldSize)
{
    IedClientError err = IED_ERROR_OK;
    snprintf(ld, ldSize, "%s", LD_NAME);

    LinkedList list = IedConnection_getLogicalDeviceList(conn, &err);
    if (err != IED_ERROR_OK || list == NULL) {
        fprintf(stderr, "%s: logical device list: error %d\n", prog, (int)err);
        if (list) LinkedList_destroy(list);
        return;
    }

    const char* exact = NULL;
    const char* suffix = NULL;
    const char* first = NULL;
    size_t lx = strlen(LD_NAME);
    for (LinkedList e = LinkedList_getNext(list); e; e = LinkedList_getNext(e)) {
        const char* name = (const char*)e->data;
        if (!name) continue;
        size_t ln = strlen(name);
        if (!first) first = name;
        if (!exact && strcmp(name, LD_NAME) == 0) exact = name;
        if (!suffix && ln >= lx && strcmp(name + ln - lx, LD_NAME) == 0) suffix = name;
    }
    const char* pick = exact ? exact : (suffix ? suffix : first);
    if (pick) snprintf(ld, ldSize, "%s", pick);
    LinkedList_destroy(list);
}

#endif /* LD_RESOLVE_H */
