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
 *   --trigger dchg|gi     report trigger (default: dchg; contract 1.3)
 * </pre>
 *
 * <p>Names (contract 1.3): the logical device is resolved from the server
 * model (exactly {@code InteropLD}, else one ending in it such as
 * {@code InteropIEDInteropLD}, else the first), and the URCB in its LLN0
 * ({@code urcb01}, else its first indexed instance such as
 * {@code urcb0101}). Targets use the resolved LD and the SCL name
 * {@code urcb01}.
 *
 * <p>JSON Lines output format (stdout):
 * <pre>
 *   {"operation":"get-rcb","target":"...","ok":true,"rptID":"...","rptEna":false}
 *   {"operation":"enable-rcb","target":"...","ok":true}
 *   {"operation":"write","target":"...","ok":true}          (dchg)
 *   {"operation":"trigger-gi","target":"...","ok":true}     (gi)
 *   {"operation":"receive-report","ok":true,"rptID":"...","seqNum":N,
 *    "inclusion":[true,false],"values":[<v>],"reasons":["data-change"]}
 *   {"operation":"disable-rcb","target":"...","ok":true}
 *   {"operation":"conclude","ok":true}
 *   {"operation":"<op>","ok":false,"error":"<msg>"}
 * </pre>
 */
public class IedReporter {

    private static final String URCB_NAME  = "urcb01";  /* SCL name, without FC/instance suffix */

    public static void run(String[] args) throws Exception {
        String  host    = "localhost";
        int     port    = 1102;
        boolean initial = false;
        String  trigger = "dchg";

        for (int i = 0; i < args.length; i++) {
            String a = args[i];
            if (!a.equals("--host") && !a.equals("--port")
                    && !a.equals("--sps1-initial") && !a.equals("--trigger")) {
                continue;
            }
            if (i + 1 >= args.length) {
                argError("missing value for " + a);
            }
            String v = args[++i];
            switch (a) {
                case "--host" -> host = v;
                case "--port" -> {
                    try {
                        port = Integer.parseInt(v);
                    } catch (NumberFormatException e) {
                        argError("invalid --port: " + v);
                    }
                }
                case "--sps1-initial" -> {
                    if (!v.equals("0") && !v.equals("1")) {
                        argError("invalid --sps1-initial: " + v + " (want 0 or 1)");
                    }
                    initial = v.equals("1");
                }
                case "--trigger" -> {
                    if (!v.equals("dchg") && !v.equals("gi")) {
                        argError("invalid --trigger: " + v + " (want dchg or gi)");
                    }
                    trigger = v;
                }
                default -> { }
            }
        }

        CountDownLatch reportLatch  = new CountDownLatch(1);
        Report[]       reportHolder = new Report[1];

        ClientEventListener listener = new ClientEventListener() {
            @Override
            public void newReport(Report report) {
                if (reportHolder[0] == null) {
                    reportHolder[0] = report;
                }
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

            LogicalDevice ld = ModelNames.resolveLd(model);
            String ldName = (ld != null) ? ld.getName() : ModelNames.LD_NAME;
            String rcbRef = ldName + "/LLN0.RP." + URCB_NAME;
            String sps1Target = ldName + "/GGIO1.SPS1.stVal[ST]";

            Urcb urcb = (ld != null) ? resolveUrcb(ld) : null;
            if (urcb == null) {
                JsonLines.error("get-rcb", rcbRef,
                        "URCB '" + URCB_NAME + "' (or an indexed instance) not found in "
                        + ldName + "/LLN0");
                return;
            }
            System.err.printf("[debug] resolved LD=%s URCB=%s%n", ldName, urcb.getName());

            // 1. get-rcb — read current attributes from server.
            assoc.getRcbValues(urcb);
            BdaVisibleString rptIdBda = urcb.getRptId();
            BdaBoolean       rptEnaBda = urcb.getRptEna();
            String rptID = (rptIdBda != null) ? rptIdBda.getStringValue() : "";
            boolean rptEna = (rptEnaBda != null) && rptEnaBda.getValue();
            System.out.printf(
                    "{\"operation\":\"get-rcb\",\"target\":%s,\"ok\":true,\"rptID\":%s,\"rptEna\":%s}%n",
                    JsonLines.quote(rcbRef),
                    JsonLines.quote(rptID),
                    rptEna ? "true" : "false");
            System.out.flush();

            // 2. enable-rcb — set RptEna=true (no explicit reservation, mirroring libiec61850).
            try {
                assoc.enableReporting(urcb);
                JsonLines.successTarget("enable-rcb", rcbRef);
            } catch (Exception e) {
                JsonLines.error("enable-rcb", rcbRef, e.getMessage());
                return;
            }

            if (trigger.equals("gi")) {
                // 3. trigger-gi — write GI=true on the URCB.
                try {
                    assoc.startGi(urcb);
                    JsonLines.successTarget("trigger-gi", rcbRef);
                } catch (Exception e) {
                    JsonLines.error("trigger-gi", rcbRef, e.getMessage());
                }
            } else {
                // 3. write — toggle GGIO1.SPS1.stVal to trigger dchg.
                boolean newVal = !initial;
                ModelNode n = model.findModelNode(ldName + "/GGIO1.SPS1.stVal", Fc.ST);
                if (!(n instanceof BdaBoolean sps1)) {
                    JsonLines.error("write", sps1Target,
                            "GGIO1.SPS1.stVal[ST] not found in server model");
                } else {
                    try {
                        sps1.setValue(newVal);
                        assoc.setDataValues(sps1);
                        JsonLines.successTarget("write", sps1Target);
                    } catch (Exception e) {
                        JsonLines.error("write", sps1Target, e.getMessage());
                    }
                }
            }

            // 4. receive-report — wait up to 10 s for the report.
            if (reportLatch.await(10, TimeUnit.SECONDS) && reportHolder[0] != null) {
                emitReport(reportHolder[0]);
            } else {
                JsonLines.error("receive-report", null,
                        "timeout-waiting-for-" + trigger + "-report");
            }

            // 5. disable-rcb — set RptEna=false.
            try {
                assoc.disableReporting(urcb);
                JsonLines.successTarget("disable-rcb", rcbRef);
            } catch (Exception e) {
                JsonLines.error("disable-rcb", rcbRef, e.getMessage());
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

    private static void argError(String msg) {
        System.err.println("iec61850bean-ied-reporter: " + msg);
        System.exit(1);
    }

    // -----------------------------------------------------------------------
    // Name resolution (contract 1.3)
    // -----------------------------------------------------------------------

    /**
     * The URCB "urcb01" in the LLN0 of the given LD, else its first indexed
     * instance ("urcb0101", "urcb0102", ... per IEC 61850-6).
     */
    private static Urcb resolveUrcb(LogicalDevice ld) {
        ModelNode lnNode = ld.getChild("LLN0");
        if (!(lnNode instanceof LogicalNode lln0)) return null;
        Urcb exact = lln0.getUrcb(URCB_NAME);
        if (exact != null) return exact;
        Urcb best = null;
        for (Urcb u : lln0.getUrcbs()) {
            String name = u.getName();
            if (!name.startsWith(URCB_NAME) || name.length() == URCB_NAME.length()) continue;
            String suffix = name.substring(URCB_NAME.length());
            if (!suffix.chars().allMatch(Character::isDigit)) continue;
            if (best == null || name.compareTo(best.getName()) < 0) best = u;
        }
        return best;
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
