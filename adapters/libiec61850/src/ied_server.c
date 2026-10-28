/* SPDX-License-Identifier: MIT */
/*
 * ied_server.c — libIEC61850 IED server adapter for mms-interop.
 *
 * Phase 2A Step 8: IED server direction
 *   go-iec61850 client ← libiec61850 IED server
 *
 * The IED model is compiled from static_model.h / static_model.c generated
 * by genmodel.py from fixtures/iec61850/interop.icd during the Docker build.
 * Symbols follow the iedModel_<LD>_<LN>_<DO>[_<DA>...] convention.
 *
 * Initial attribute values are set programmatically to match values.json.
 *
 * Usage:
 *   libiec61850-ied-server [--port PORT]
 *
 * Lifecycle:
 *   1. Parse --port (default 1102).
 *   2. Create IedServer from the static model.
 *   3. Set initial attribute values.
 *   4. Start listening.
 *   5. Emit {"event":"ready","address":"0.0.0.0:<port>"} to stdout.
 *   6. Wait for SIGTERM / SIGINT.
 *   7. Stop and destroy.
 *
 * Exit codes:
 *   0  — clean shutdown
 *   1  — startup / argument error
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>

#include "iec61850_server.h"
#include "hal_thread.h"
#include "static_model.h"
#include "jsonlines.h"

/* -------------------------------------------------------------------------
 * Signal handling
 * ---------------------------------------------------------------------- */

static volatile sig_atomic_t g_running = 1;

static void sig_handler(int sig)
{
    (void)sig;
    g_running = 0;
}

/* -------------------------------------------------------------------------
 * Initial value setup
 *
 * Values match fixtures/iec61850/values.json exactly.
 * ---------------------------------------------------------------------- */

static void set_initial_values(IedServer server)
{
    /* LLN0.Mod.stVal = 1 (on) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_LLN0_Mod_stVal, 1);

    /* LLN0.Mod.ctlModel = 1 (direct-with-normal-security) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_LLN0_Mod_ctlModel, 1);

    /* LLN0.Mod.d = "mode" */
    IedServer_updateVisibleStringAttributeValue(server,
        &iedModel_InteropLD_LLN0_Mod_d, "mode");

    /* LLN0.Beh.stVal = 1 (on) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_LLN0_Beh_stVal, 1);

    /* GGIO1.SPS1.stVal = false */
    IedServer_updateBooleanAttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPS1_stVal, false);

    /* GGIO1.SPS1.d = "status point" */
    IedServer_updateVisibleStringAttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPS1_d, "status point");

    /* GGIO1.DPS1.stVal = 2 (on, bits "10") */
    IedServer_updateDbposValue(server,
        &iedModel_InteropLD_GGIO1_DPS1_stVal, DBPOS_ON);

    /* GGIO1.SPCSO1.stVal = false */
    IedServer_updateBooleanAttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPCSO1_stVal, false);

    /* GGIO1.SPCSO1.ctlModel = 1 (direct-with-normal-security) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPCSO1_ctlModel, 1);

    /* GGIO1.SPCSO2.stVal = false */
    IedServer_updateBooleanAttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPCSO2_stVal, false);

    /* GGIO1.SPCSO2.ctlModel = 2 (sbo-with-normal-security) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPCSO2_ctlModel, 2);

    /* GGIO1.SPCSO3.stVal = false */
    IedServer_updateBooleanAttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPCSO3_stVal, false);

    /* GGIO1.SPCSO3.ctlModel = 4 (sbo-with-enhanced-security) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_GGIO1_SPCSO3_ctlModel, 4);

    /* MMXU1.TotW.mag.f = 1234.5 */
    IedServer_updateFloatAttributeValue(server,
        &iedModel_InteropLD_MMXU1_TotW_mag_f, 1234.5f);

    /* MMTR1.TotVAh.actVal = 42 (INT32U) */
    IedServer_updateUnsignedAttributeValue(server,
        &iedModel_InteropLD_MMTR1_TotVAh_actVal, 42);

    /* GGIO1.SetInt1.setVal = 0 (writable ING setting) */
    IedServer_updateInt32AttributeValue(server,
        &iedModel_InteropLD_GGIO1_SetInt1_setVal, 0);
}

/* -------------------------------------------------------------------------
 * Control handlers
 *
 * Updates SPCSO1.stVal (direct) or SPCSO2.stVal (SBO normal) when an
 * Operate command is received.
 * ---------------------------------------------------------------------- */

static ControlHandlerResult
spcso1_control_handler(ControlAction action, void* parameter, MmsValue* value, bool test)
{
    IedServer server = (IedServer)parameter;
    if (!test && value != NULL) {
        bool ctlVal = MmsValue_getBoolean(value);
        IedServer_updateBooleanAttributeValue(server,
            &iedModel_InteropLD_GGIO1_SPCSO1_stVal, ctlVal);
    }
    return CONTROL_RESULT_OK;
}

static ControlHandlerResult
spcso2_control_handler(ControlAction action, void* parameter, MmsValue* value, bool test)
{
    IedServer server = (IedServer)parameter;
    if (!test && value != NULL) {
        bool ctlVal = MmsValue_getBoolean(value);
        IedServer_updateBooleanAttributeValue(server,
            &iedModel_InteropLD_GGIO1_SPCSO2_stVal, ctlVal);
    }
    return CONTROL_RESULT_OK;
}

static ControlHandlerResult
spcso3_control_handler(ControlAction action, void* parameter, MmsValue* value, bool test)
{
    IedServer server = (IedServer)parameter;
    if (!test && value != NULL) {
        bool ctlVal = MmsValue_getBoolean(value);
        IedServer_updateBooleanAttributeValue(server,
            &iedModel_InteropLD_GGIO1_SPCSO3_stVal, ctlVal);
    }
    return CONTROL_RESULT_OK;
}

/* -------------------------------------------------------------------------
 * main
 * ---------------------------------------------------------------------- */

int main(int argc, char** argv)
{
    jl_handle_meta_flags(argc, argv, "libiec61850");
    int port = 1102;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--port") && i + 1 < argc)
            port = atoi(argv[++i]);
    }
    if (port <= 0 || port > 65535) {
        fprintf(stderr, "ied-server: invalid port %d\n", port);
        return 1;
    }

    signal(SIGTERM, sig_handler);
    signal(SIGINT,  sig_handler);

    IedServer server = IedServer_create(&iedModel);
    if (!server) {
        fprintf(stderr, "ied-server: IedServer_create failed\n");
        return 1;
    }

    /* Allow writes needed by the fixed IED-client sequence and control tests.
     * ST is required so Mod.stVal can be written by *-ied-client (same-stack
     * smoke and reverse Go-server tests use that path). */
    IedServer_setWriteAccessPolicy(server, IEC61850_FC_CF, ACCESS_POLICY_ALLOW);
    IedServer_setWriteAccessPolicy(server, IEC61850_FC_DC, ACCESS_POLICY_ALLOW);
    IedServer_setWriteAccessPolicy(server, IEC61850_FC_SP, ACCESS_POLICY_ALLOW);
    IedServer_setWriteAccessPolicy(server, IEC61850_FC_ST, ACCESS_POLICY_ALLOW);

    set_initial_values(server);

    /* Register direct-control handler for SPCSO1 so that Operate commands
     * update stVal. Without this, libiec61850 accepts the MMS write but
     * does not propagate ctlVal to the status attribute. */
    IedServer_setControlHandler(server,
        &iedModel_InteropLD_GGIO1_SPCSO1,
        spcso1_control_handler,
        (void*)server);

    /* Register SBO-normal control handler for SPCSO2. libiec61850 handles
     * the SBO select/operate protocol automatically when ctlModel=2; the
     * handler is only called after a successful select+operate sequence. */
    IedServer_setControlHandler(server,
        &iedModel_InteropLD_GGIO1_SPCSO2,
        spcso2_control_handler,
        (void*)server);

    /* Register SBO-enhanced (SBOw) control handler for SPCSO3.
     * libiec61850 validates the SelectWithValue + Operate sequence; the
     * handler is only invoked after a successful SBOw select+operate. */
    IedServer_setControlHandler(server,
        &iedModel_InteropLD_GGIO1_SPCSO3,
        spcso3_control_handler,
        (void*)server);

    IedServer_start(server, port);
    if (!IedServer_isRunning(server)) {
        fprintf(stderr, "ied-server: failed to start on port %d\n", port);
        IedServer_destroy(server);
        return 1;
    }

    char addr_buf[64];
    snprintf(addr_buf, sizeof(addr_buf), "0.0.0.0:%d", port);
    jl_ready(addr_buf, "iec61850-v3", "libiec61850", iedModel.name);

    while (g_running)
        Thread_sleep(100);

    IedServer_stop(server);
    IedServer_destroy(server);
    return 0;
}
