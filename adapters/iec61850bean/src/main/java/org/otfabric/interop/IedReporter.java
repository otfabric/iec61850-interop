// SPDX-License-Identifier: MIT
package org.otfabric.interop;

import com.beanit.iec61850bean.*;

import java.io.IOException;
import java.net.InetAddress;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * iec61850bean IED reporter adapter for iec61850-interop Phase 2D.
 *
 * <p>Mirrors the operation sequence of {@code libiec61850-ied-reporter}:
 * <ol>
 *   <li>get-rcb    — read current URCB attributes.</li>
 *   <li>enable-rcb — set RptEna=true.</li>
 *   <li>write      — toggle GGIO1.SPS1.stVal[ST] to trigger a dchg report.</li>
 *   <li>receive-report — wait up to 10 s for the InformationReport.</li>
 *   <li>disable-rcb — set RptEna=false.</li>
 *   <li>conclude   — disconnect.</li>
 * </ol>
 *
 * <p>Reports arrive via the {@link ClientEventListener#newReport} callback that
 * is supplied to {@link ClientSap#associate}.
 *
 * <p>Command-line arguments:
 * <pre>
 *   --host H              server host (default: localhost)
 *   --port P              server TCP port (default: 1102)
 *   --sps1-initial 0|1    initial value of SPS1.stVal (default: 0)
 * </pre>
 *
 * <p>JSON Lines output format (stdout):
 * <pre>
 *   {"operation":"get-rcb","target":"...","ok":true,"rptID":"...","rptEna":false}
 *   {"operation":"enable-rcb","target":"...","ok":true}
 *   {"operation":"write","target":"...","ok":true}
 *   {"operation":"receive-report","ok":true,"rptID":"...","seqNum":N,
 *    "inclusion":[true,false],"values":[<v>],"reasons":["data-change"]}
 *   {"operation":"disable-rcb","target":"...","ok":true}
 *   {"operation":"conclude","ok":true}
 *   {"operation":"<op>","ok":false,"error":"<msg>"}
 * </pre>
 */
public class IedReporter {

    private static final String RCB_REF    = "InteropLD/LLN0.RP.urcb01";
    private static final String URCB_MODEL = "InteropLD/LLN0.urcb01";
    private static final String URCB_NAME  = "urcb01";  /* SCL name, without FC/instance suffix */
    private static final String SPS1_REF   = "InteropLD/GGIO1.SPS1.stVal";
    private static final String SPS1_TARGET = SPS1_REF + "[ST]";
    private static final int    DS_SIZE    = 2;

    public static void run(String[] args) throws Exception {
        String  host    = "localhost";
        int     port    = 1102;
        boolean initial = false;

        for (int i = 0; i < args.length - 1; i++) {
            switch (args[i]) {
                case "--host"         -> host    = args[++i];
                case "--port"         -> port    = Integer.parseInt(args[++i]);
                case "--sps1-initial" -> initial = Integer.parseInt(args[++i]) != 0;
            }
        }

        CountDownLatch reportLatch  = new CountDownLatch(1);
        Report[]       reportHolder = new Report[1];

        ClientEventListener listener = new ClientEventListener() {
            @Override
            public void newReport(Report report) {
                reportHolder[0] = report;
                reportLatch.countDown();
            }

            @Override
            public void associationClosed(IOException e) {}
        };

        ClientSap          clientSap = new ClientSap();
        ClientAssociation  assoc;
        try {
            assoc = clientSap.associate(InetAddress.getByName(host), port, null, listener);
        } catch (Exception e) {
            JsonLines.error("conclude", null,
                    "connect " + host + ":" + port + ": " + e.getMessage());
            System.exit(2);
            return;
        }

        try {
            ServerModel model = assoc.retrieveModel();
            assoc.updateDataSets();

            // Debug: log LD/LN structure and URCBs
            for (ModelNode ldNode : model) {
                if (!(ldNode instanceof LogicalDevice ld)) continue;
                System.err.printf("[debug] LD: %s%n", ld.getName());
                for (ModelNode lnNode : ld) {
                    if (!(lnNode instanceof LogicalNode ln)) continue;
                    System.err.printf("[debug]   LN: %s urcbs=%s%n",
                            ln.getName(), ln.getUrcbs());
                }
            }

            // Locate the URCB by traversing all LDs/LNs to support any IED name prefix.
            Urcb urcb = findUrcbByName(model, URCB_NAME);
            if (urcb == null) {
                JsonLines.error("get-rcb", RCB_REF,
                        "URCB '" + URCB_NAME + "' not found in server model");
                return;
            }

            // 1. get-rcb — read current attributes from server.
            assoc.getRcbValues(urcb);
            BdaVisibleString rptIdBda = urcb.getRptId();
            BdaBoolean       rptEnaBda = urcb.getRptEna();
            String rptID = (rptIdBda != null) ? rptIdBda.getStringValue() : "";
            boolean rptEna = (rptEnaBda != null) && rptEnaBda.getValue();
            System.out.printf(
                    "{\"operation\":\"get-rcb\",\"target\":%s,\"ok\":true,\"rptID\":%s,\"rptEna\":%s}%n",
                    JsonLines.quote(RCB_REF),
                    JsonLines.quote(rptID),
                    rptEna ? "true" : "false");
            System.out.flush();

            // 2. enable-rcb — set RptEna=true (no explicit reservation, mirroring libiec61850).
            try {
                assoc.enableReporting(urcb);
                System.out.printf(
                        "{\"operation\":\"enable-rcb\",\"target\":%s,\"ok\":true}%n",
                        JsonLines.quote(RCB_REF));
                System.out.flush();
            } catch (Exception e) {
                JsonLines.error("enable-rcb", RCB_REF, e.getMessage());
                return;
            }

            // 3. write — toggle GGIO1.SPS1.stVal to trigger dchg.
            boolean newVal = !initial;
            // Search for SPS1.stVal across all LDs to handle any IED name prefix.
            BdaBoolean sps1 = findSPS1StVal(model);
            if (sps1 == null) {
                JsonLines.error("write", SPS1_TARGET,
                        "GGIO1.SPS1.stVal[ST] not found in server model");
            } else {
                try {
                    sps1.setValue(newVal);
                    assoc.setDataValues((FcModelNode) sps1);
                    System.out.printf(
                            "{\"operation\":\"write\",\"target\":%s,\"ok\":true}%n",
                            JsonLines.quote(SPS1_TARGET));
                    System.out.flush();
                } catch (Exception e) {
                    JsonLines.error("write", SPS1_TARGET, e.getMessage());
                }
            }

            // 4. receive-report — wait up to 10 s for the dchg report.
            if (reportLatch.await(10, TimeUnit.SECONDS) && reportHolder[0] != null) {
                emitReport(reportHolder[0]);
            } else {
                JsonLines.error("receive-report", null,
                        "timeout-waiting-for-dchg-report");
            }

            // 5. disable-rcb — set RptEna=false.
            try {
                assoc.disableReporting(urcb);
                System.out.printf(
                        "{\"operation\":\"disable-rcb\",\"target\":%s,\"ok\":true}%n",
                        JsonLines.quote(RCB_REF));
                System.out.flush();
            } catch (Exception e) {
                JsonLines.error("disable-rcb", RCB_REF, e.getMessage());
            }

        } catch (Exception e) {
            JsonLines.error("conclude", null, e.getMessage());
        } finally {
            try {
                assoc.disconnect();
            } catch (Exception ignored) {}
            System.out.printf("{\"operation\":\"conclude\",\"ok\":true}%n");
            System.out.flush();
        }
    }

    // -----------------------------------------------------------------------
    // Model search helpers (IED-name-agnostic)
    // -----------------------------------------------------------------------

    /**
     * Finds the first URCB with the given SCL name by traversing all logical
     * devices and nodes in the model. This approach is independent of the
     * IED name prefix used in the MMS domain name.
     */
    private static Urcb findUrcbByName(ServerModel model, String urcbName) {
        for (ModelNode ldNode : model) {
            if (!(ldNode instanceof LogicalDevice ld)) continue;
            for (ModelNode lnNode : ld) {
                if (!(lnNode instanceof LogicalNode ln)) continue;
                Urcb u = ln.getUrcb(urcbName);
                if (u != null) return u;
            }
        }
        return null;
    }

    /**
     * Finds GGIO1.SPS1.stVal[ST] by traversing all logical devices in the
     * model. This is IED-name-agnostic: the logical device may be named
     * "InteropLD" (iec61850bean server) or "InteropIEDInteropLD" (go-iec61850
     * server with IED prefix).
     */
    private static BdaBoolean findSPS1StVal(ServerModel model) {
        for (ModelNode ldNode : model) {
            if (!(ldNode instanceof LogicalDevice ld)) continue;
            String ldName = ld.getName();
            // Try the standard path within this LD.
            ModelNode n = model.findModelNode(ldName + "/GGIO1.SPS1.stVal", Fc.ST);
            if (n instanceof BdaBoolean bda) return bda;
        }
        return null;
    }

    // -----------------------------------------------------------------------
    // Report serialisation
    // -----------------------------------------------------------------------

    private static void emitReport(Report report) {
        boolean[]              inclusion = report.getInclusionBitString();
        List<FcModelNode>      values    = report.getValues();
        List<BdaReasonForInclusion> reasons = report.getReasonCodes();

        StringBuilder sb = new StringBuilder();
        sb.append("{\"operation\":\"receive-report\",\"ok\":true");

        String rptId = report.getRptId();
        sb.append(",\"rptID\":").append(JsonLines.quote(rptId != null ? rptId : ""));

        Integer sqNum = report.getSqNum();
        sb.append(",\"seqNum\":").append(sqNum != null ? sqNum : 0);

        // inclusion — one entry per dataset member.
        sb.append(",\"inclusion\":[");
        if (inclusion != null) {
            for (int i = 0; i < inclusion.length; i++) {
                if (i > 0) sb.append(",");
                sb.append(inclusion[i] ? "true" : "false");
            }
        }
        sb.append("]");

        // values — one entry per *included* dataset member (parallel to reasons).
        sb.append(",\"values\":[");
        if (values != null) {
            boolean first = true;
            for (FcModelNode v : values) {
                if (!first) sb.append(",");
                first = false;
                sb.append(nodeValueJson(v));
            }
        }
        sb.append("]");

        // reasons — one entry per *included* dataset member.
        sb.append(",\"reasons\":[");
        if (reasons != null) {
            boolean first = true;
            for (BdaReasonForInclusion r : reasons) {
                if (!first) sb.append(",");
                first = false;
                sb.append(JsonLines.quote(reasonStr(r)));
            }
        }
        sb.append("]}");

        System.out.println(sb);
        System.out.flush();
    }

    private static String nodeValueJson(FcModelNode node) {
        if (node instanceof BdaBoolean bda)        return String.valueOf(bda.getValue());
        if (node instanceof BdaInt32 bda)          return String.valueOf(bda.getValue());
        if (node instanceof BdaInt8U bda)          return String.valueOf(bda.getValue());
        if (node instanceof BdaInt8 bda)           return String.valueOf(bda.getValue());
        if (node instanceof BdaFloat32 bda)        return String.valueOf(bda.getFloat());
        if (node instanceof BdaVisibleString bda)  return JsonLines.quote(bda.getStringValue());
        return "null";
    }

    private static String reasonStr(BdaReasonForInclusion r) {
        if (r.isDataChange())           return "data-change";
        if (r.isQualityChange())        return "quality-change";
        if (r.isDataUpdate())           return "data-update";
        if (r.isIntegrity())            return "integrity";
        if (r.isGeneralInterrogation()) return "gi";
        return "unknown";
    }
}
