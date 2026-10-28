/* SPDX-License-Identifier: MIT */
#ifndef MMS_CLIENT_SERVICES_H
#define MMS_CLIENT_SERVICES_H

#include "mms_client_connection.h"

/* Installs the InformationReport handler. Call before the first request
 * so that no report is lost. */
void services_install_report_handler(MmsConnection conn);

/* Runs the fixture mms-v2 service sequence (status, name lists, alternate
 * access, named variable lists in every scope, file services, journals,
 * InformationReport reception) and emits one JSON line per operation. */
void run_services(MmsConnection conn);

/* Opens a separate association, reads one variable and aborts it. */
void run_abort(const char* host, int port);

/* Opens a separate association and writes -1 to interop/report-trigger,
 * which asks the server to abort it. */
void run_server_abort(const char* host, int port);

#endif
