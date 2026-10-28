// SPDX-License-Identifier: MIT
package org.otfabric.interop;

import com.beanit.iec61850bean.*;

import java.net.InetAddress;

/**
 * iec61850bean IED controller adapter for iec61850-interop Phase 2E/2I/2J.
 *
 * <p>Mirrors the operation sequence of {@code libiec61850-ied-controller}:
 * <ol>
 *   <li>read-ctlmodel — read GGIO1.SPCSOn.ctlModel[CF]</li>
 *   <li>select        — (SBO normal only) read SPCSO2.SBO[CO]</li>
 *   <li>select-with-value — (SBO enhanced only) write SPCSO3.SBOw[CO]</li>
 *   <li>operate       — write Oper[CO] with ctlVal=&lt;--ctlval&gt;</li>
 *   <li>read-stval    — read the target stVal[ST]</li>
 *   <li>conclude      — disconnect</li>
 * </ol>
 *
 * <p>Command-line arguments:
 * <pre>
 *   --host H                   server host (default: localhost)
 *   --port P                   server TCP port (default: 1102)
 *   --ctlval 0|1               control value to apply (default: 1 = true)
 *   --do SPCSO1|SPCSO2|SPCSO3  target data object (default: SPCSO1)
 * </pre>
 */
public class IedController {

    public static void run(String[] args) throws Exception {
        String  host   = "localhost";
        int     port   = 1102;
        boolean ctlVal = true;
        String  doName = "SPCSO1";

        for (int i = 0; i < args.length - 1; i++) {
            switch (args[i]) {
                case "--host"   -> host   = args[++i];
                case "--port"   -> port   = Integer.parseInt(args[++i]);
                case "--ctlval" -> ctlVal = !args[++i].equals("0");
                case "--do"     -> doName = args[++i];
            }
        }

        String spcsoBase   = "InteropLD/GGIO1." + doName;
        String ctlModelRef = spcsoBase + ".ctlModel[CF]";
        String stvalTarget = spcsoBase + ".stVal[ST]";
        boolean isSBONormal   = "SPCSO2".equals(doName);
        boolean isSBOEnhanced = "SPCSO3".equals(doName);

        ClientSap clientSap = new ClientSap();
        ClientAssociation assoc;
        try {
            assoc = clientSap.associate(InetAddress.getByName(host), port, null, null);
        } catch (Exception e) {
            JsonLines.error("conclude", null,
                    "connect " + host + ":" + port + ": " + e.getMessage());
            System.exit(2);
            return;
        }

        try {
            ServerModel model = assoc.retrieveModel();

            // 1. read-ctlmodel
            readCtlModel(assoc, model, spcsoBase, ctlModelRef);

            // 2a. select (SBO normal only)
            if (isSBONormal) {
                if (!selectSBO(assoc, model, spcsoBase)) {
                    return;
                }
            }

            // 2b. select-with-value (SBO enhanced only)
            if (isSBOEnhanced) {
                if (!selectWithValue(assoc, model, spcsoBase, ctlVal)) {
                    return;
                }
            }

            // 3. operate
            operate(assoc, model, spcsoBase, ctlVal);

            // 4. read-stval
            readStVal(assoc, model, spcsoBase, stvalTarget);

        } finally {
            try { assoc.disconnect(); } catch (Exception ignored) {}
            JsonLines.success("conclude");
        }
    }

    // -----------------------------------------------------------------------
    // read-ctlmodel
    // -----------------------------------------------------------------------

    private static void readCtlModel(ClientAssociation assoc, ServerModel model,
                                     String spcsoBase, String ctlModelTarget) {
        ModelNode node = model.findModelNode(spcsoBase + ".ctlModel", Fc.CF);
        if (!(node instanceof BdaInt8 bda)) {
            JsonLines.error("read-ctlmodel", ctlModelTarget,
                    "ctlModel node not found or wrong type in model");
            return;
        }
        try {
            assoc.getDataValues(bda);
            JsonLines.successReadIntOp("read-ctlmodel", ctlModelTarget, bda.getValue());
        } catch (Exception e) {
            JsonLines.error("read-ctlmodel", ctlModelTarget, e.getMessage());
        }
    }

    // -----------------------------------------------------------------------
    // select — SBO normal: read SBO[CO]
    // -----------------------------------------------------------------------

    private static boolean selectSBO(ClientAssociation assoc, ServerModel model, String spcsoBase) {
        ModelNode node = model.findModelNode(spcsoBase + ".SBO", Fc.CO);
        if (!(node instanceof BdaVisibleString bda)) {
            JsonLines.error("select", spcsoBase, "SBO[CO] node not found or wrong type in model");
            return false;
        }
        try {
            assoc.getDataValues(bda);
            String ref = bda.getStringValue();
            if (ref == null || ref.isEmpty()) {
                JsonLines.error("select", spcsoBase, "server returned empty SBO string (select denied)");
                return false;
            }
            System.out.println("{\"operation\":\"select\",\"target\":\"" + spcsoBase + "\",\"ok\":true}");
            System.out.flush();
            return true;
        } catch (Exception e) {
            JsonLines.error("select", spcsoBase, e.getMessage());
            return false;
        }
    }

    // -----------------------------------------------------------------------
    // select-with-value — SBO enhanced: write SBOw[CO] with Oper structure
    // -----------------------------------------------------------------------

    private static boolean selectWithValue(ClientAssociation assoc, ServerModel model,
                                           String spcsoBase, boolean ctlVal) {
        FcModelNode sbowNode = findFcNode(model, spcsoBase + ".SBOw", Fc.CO);
        if (sbowNode == null) {
            JsonLines.error("select-with-value", spcsoBase, spcsoBase + ".SBOw[CO] not found in model");
            return false;
        }
        ModelNode ctlValNode = model.findModelNode(spcsoBase + ".SBOw.ctlVal", Fc.CO);
        if (!(ctlValNode instanceof BdaBoolean bdaBool)) {
            JsonLines.error("select-with-value", spcsoBase, spcsoBase + ".SBOw.ctlVal[CO] not found or wrong type");
            return false;
        }
        bdaBool.setValue(ctlVal);
        try {
            assoc.setDataValues(sbowNode);
            System.out.println("{\"operation\":\"select-with-value\",\"target\":\"" + spcsoBase + "\",\"ok\":true}");
            System.out.flush();
            return true;
        } catch (Exception e) {
            JsonLines.error("select-with-value", spcsoBase, e.getMessage());
            return false;
        }
    }

    // -----------------------------------------------------------------------
    // operate — write Oper[CO] with ctlVal
    // -----------------------------------------------------------------------

    private static void operate(ClientAssociation assoc, ServerModel model,
                                String spcsoBase, boolean ctlVal) {
        FcModelNode operNode = findFcNode(model, spcsoBase + ".Oper", Fc.CO);
        if (operNode == null) {
            JsonLines.error("operate", spcsoBase, spcsoBase + ".Oper[CO] not found in model");
            return;
        }

        ModelNode ctlValNode = model.findModelNode(spcsoBase + ".Oper.ctlVal", Fc.CO);
        if (!(ctlValNode instanceof BdaBoolean bdaBool)) {
            JsonLines.error("operate", spcsoBase, spcsoBase + ".Oper.ctlVal[CO] not found or wrong type");
            return;
        }
        bdaBool.setValue(ctlVal);

        try {
            assoc.setDataValues(operNode);
            System.out.println("{\"operation\":\"operate\",\"target\":\""
                    + spcsoBase + "\",\"ok\":true,\"ctlval\":"
                    + ctlVal + "}");
            System.out.flush();
        } catch (Exception e) {
            JsonLines.error("operate", spcsoBase, e.getMessage());
        }
    }

    // -----------------------------------------------------------------------
    // read-stval
    // -----------------------------------------------------------------------

    private static void readStVal(ClientAssociation assoc, ServerModel model,
                                  String spcsoBase, String stvalTarget) {
        ModelNode node = model.findModelNode(spcsoBase + ".stVal", Fc.ST);
        if (!(node instanceof BdaBoolean bda)) {
            JsonLines.error("read-stval", stvalTarget, "stVal node not found in model");
            return;
        }
        try {
            assoc.getDataValues(bda);
            JsonLines.successReadBoolOp("read-stval", stvalTarget, bda.getValue());
        } catch (Exception e) {
            JsonLines.error("read-stval", stvalTarget, e.getMessage());
        }
    }

    // -----------------------------------------------------------------------
    // helpers
    // -----------------------------------------------------------------------

    private static FcModelNode findFcNode(ServerModel model, String ref, Fc fc) {
        ModelNode n = model.findModelNode(ref, fc);
        return (n instanceof FcModelNode f) ? f : null;
    }
}
