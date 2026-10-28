// SPDX-License-Identifier: MIT
package org.otfabric.interop;

import com.beanit.iec61850bean.*;

import java.net.InetAddress;
import java.util.*;

/**
 * iec61850bean IED client adapter for mms-interop.
 *
 * <p>Executes the same fixed operation sequence as the libiec61850 IED client
 * (see {@code adapters/libiec61850/src/ied_client.c}) and emits one JSON Line
 * per operation to stdout, using identical field names and value encoding.
 *
 * <p>The logical device name is resolved from the server directory: bare
 * {@code InteropLD} against go-iec61850, or IED-prefixed
 * {@code InteropIEDInteropLD} against libiec61850 / iec61850bean servers.
 *
 * <p>Exit codes:
 * <ul>
 *   <li>0 — all operations ok:true</li>
 *   <li>1 — argument / startup error</li>
 *   <li>2 — connection / associate failure</li>
 *   <li>3 — one or more operations emitted ok:false (conclude still attempted)</li>
 * </ul>
 */
public class IedClient {

    public static void run(String[] args) throws Exception {
        String host = "localhost";
        int    port = 1102;

        for (int i = 0; i < args.length - 1; i++) {
            switch (args[i]) {
                case "--host" -> host = args[++i];
                case "--port" -> port = Integer.parseInt(args[++i]);
            }
        }
        if (port <= 0 || port > 65535) {
            System.err.println("ied-client: invalid port " + port);
            System.exit(1);
        }

        ClientSap clientSap = new ClientSap();
        ClientAssociation assoc;
        try {
            assoc = clientSap.associate(InetAddress.getByName(host), port, null, null);
        } catch (Exception e) {
            JsonLines.error("associate", null, "connect " + host + ":" + port + ": " + e.getMessage());
            System.exit(2);
            return;
        }
        JsonLines.success("associate");

        boolean failed = false;
        try {
            ServerModel model = assoc.retrieveModel();
            assoc.updateDataSets();

            List<String> ldNames = new ArrayList<>();
            for (ModelNode ldNode : model) {
                ldNames.add(ldNode.getName());
            }
            JsonLines.successNames("get-server-directory", null, ldNames);

            String ld = resolveInteropLd(ldNames);
            if (ld == null) {
                JsonLines.error("get-server-directory", null, "no logical device found");
                failed = true;
            } else {
                ModelNode interopLD = model.findModelNode(ld, null);
                if (interopLD == null) {
                    JsonLines.error("get-ld-directory", ld, "not found in model");
                    failed = true;
                } else {
                    List<String> lnNames = new ArrayList<>();
                    for (ModelNode ln : interopLD) {
                        lnNames.add(ln.getName());
                    }
                    JsonLines.successNames("get-ld-directory", ld, lnNames);

                    String ggio1Ref = ld + "/GGIO1";
                    ModelNode ggio1 = model.findModelNode(ggio1Ref, null);
                    if (ggio1 == null) {
                        JsonLines.error("get-ln-directory", ggio1Ref, "not found in model");
                        failed = true;
                    } else {
                        Set<String> seen = new LinkedHashSet<>();
                        for (ModelNode fcdo : ggio1) {
                            seen.add(fcdo.getName());
                        }
                        JsonLines.successNames("get-ln-directory", ggio1Ref,
                                new ArrayList<>(seen));
                    }
                }

                failed |= !readBool(assoc, model, ld + "/GGIO1.SPS1.stVal", Fc.ST,
                        ld + "/GGIO1.SPS1.stVal[ST]");
                failed |= !readFloatMagF(assoc, model, ld);
                failed |= !readDbpos(assoc, model, ld + "/GGIO1.DPS1.stVal", Fc.ST,
                        ld + "/GGIO1.DPS1.stVal[ST]");
                failed |= !readUInt32(assoc, model, ld + "/MMTR1.TotVAh.actVal", Fc.ST,
                        ld + "/MMTR1.TotVAh.actVal[ST]");
                failed |= !readInt8(assoc, model, ld + "/LLN0.Mod.ctlModel", Fc.CF,
                        ld + "/LLN0.Mod.ctlModel[CF]");
                failed |= !readString(assoc, model, ld + "/LLN0.Mod.d", Fc.DC,
                        ld + "/LLN0.Mod.d[DC]");
                failed |= !writeInt32(assoc, model, ld + "/LLN0.Mod.stVal", Fc.ST,
                        ld + "/LLN0.Mod.stVal[ST]", 5);
                failed |= !readDataSet(assoc, model, ld + "/LLN0$dsInterop");
            }

        } finally {
            try {
                assoc.disconnect();
            } catch (Exception ignored) {}
            JsonLines.success("conclude");
        }

        if (failed) {
            System.exit(3);
        }
    }

    /** Prefer bare InteropLD, else first name ending with InteropLD, else first LD. */
    static String resolveInteropLd(List<String> ldNames) {
        if (ldNames == null || ldNames.isEmpty()) {
            return null;
        }
        if (ldNames.contains("InteropLD")) {
            return "InteropLD";
        }
        for (String name : ldNames) {
            if (name != null && name.endsWith("InteropLD")) {
                return name;
            }
        }
        return ldNames.get(0);
    }

    private static boolean readBool(ClientAssociation assoc, ServerModel model,
                                    String ref, Fc fc, String target) {
        ModelNode node = model.findModelNode(ref, fc);
        if (!(node instanceof BdaBoolean bda)) {
            JsonLines.error("read", target, "model node not found or wrong type");
            return false;
        }
        try {
            assoc.getDataValues((FcModelNode) bda);
            JsonLines.successReadBool(target, bda.getValue());
            return true;
        } catch (Exception e) {
            JsonLines.error("read", target, e.getMessage());
            return false;
        }
    }

    private static boolean readFloatMagF(ClientAssociation assoc, ServerModel model, String ld) {
        String target = ld + "/MMXU1.TotW.mag.f[MX]";
        ModelNode totW = model.findModelNode(ld + "/MMXU1.TotW", Fc.MX);
        if (!(totW instanceof FcModelNode totWFc)) {
            JsonLines.error("read", target, "TotW[MX] not found in model");
            return false;
        }
        try {
            assoc.getDataValues(totWFc);
            ModelNode f = model.findModelNode(ld + "/MMXU1.TotW.mag.f", Fc.MX);
            if (!(f instanceof BdaFloat32 bda)) {
                JsonLines.error("read", target, "TotW.mag.f[MX] not found after read");
                return false;
            }
            JsonLines.successReadFloat(target, bda.getFloat());
            return true;
        } catch (Exception e) {
            JsonLines.error("read", target, e.getMessage());
            return false;
        }
    }

    private static boolean readInt8(ClientAssociation assoc, ServerModel model,
                                    String ref, Fc fc, String target) {
        ModelNode node = model.findModelNode(ref, fc);
        if (!(node instanceof BdaInt8 bda)) {
            JsonLines.error("read", target, "model node not found or wrong type");
            return false;
        }
        try {
            assoc.getDataValues((FcModelNode) bda);
            JsonLines.successReadInt(target, bda.getValue());
            return true;
        } catch (Exception e) {
            JsonLines.error("read", target, e.getMessage());
            return false;
        }
    }

    /** Dbpos is reported as its enumeration value 0..3 (2 = on, bits "10"). */
    private static boolean readDbpos(ClientAssociation assoc, ServerModel model,
                                     String ref, Fc fc, String target) {
        ModelNode node = model.findModelNode(ref, fc);
        if (!(node instanceof BdaBitString bda)) {
            JsonLines.error("read", target, "model node not found or wrong type");
            return false;
        }
        try {
            assoc.getDataValues((FcModelNode) bda);
            byte b = bda.getValue()[0];
            JsonLines.successReadInt(target, ((b >> 7) & 1) << 1 | ((b >> 6) & 1));
            return true;
        } catch (Exception e) {
            JsonLines.error("read", target, e.getMessage());
            return false;
        }
    }

    private static boolean readUInt32(ClientAssociation assoc, ServerModel model,
                                      String ref, Fc fc, String target) {
        ModelNode node = model.findModelNode(ref, fc);
        if (!(node instanceof BdaInt32U bda)) {
            JsonLines.error("read", target, "model node not found or wrong type");
            return false;
        }
        try {
            assoc.getDataValues((FcModelNode) bda);
            JsonLines.successReadInt(target, bda.getValue());
            return true;
        } catch (Exception e) {
            JsonLines.error("read", target, e.getMessage());
            return false;
        }
    }

    private static boolean readString(ClientAssociation assoc, ServerModel model,
                                      String ref, Fc fc, String target) {
        ModelNode node = model.findModelNode(ref, fc);
        if (!(node instanceof BdaVisibleString bda)) {
            JsonLines.error("read", target, "model node not found or wrong type");
            return false;
        }
        try {
            assoc.getDataValues((FcModelNode) bda);
            JsonLines.successReadString(target, bda.getStringValue());
            return true;
        } catch (Exception e) {
            JsonLines.error("read", target, e.getMessage());
            return false;
        }
    }

    private static boolean writeInt32(ClientAssociation assoc, ServerModel model,
                                      String ref, Fc fc, String target, int value) {
        ModelNode node = model.findModelNode(ref, fc);
        if (!(node instanceof BdaInt32 bda)) {
            JsonLines.error("write", target, "model node not found or wrong type");
            return false;
        }
        try {
            bda.setValue(value);
            assoc.setDataValues((FcModelNode) bda);
            JsonLines.successTarget("write", target);
            return true;
        } catch (Exception e) {
            JsonLines.error("write", target, e.getMessage());
            return false;
        }
    }

    private static boolean readDataSet(ClientAssociation assoc, ServerModel model,
                                       String dsRef) {
        DataSet ds = model.getDataSet(dsRef.replace('$', '.'));
        if (ds == null) {
            JsonLines.error("read-dataset", dsRef, "dataset not found in model");
            return false;
        }
        try {
            assoc.getDataSetValues(ds);
            List<FcModelNode> members = ds.getMembers();
            StringBuilder sb = new StringBuilder("[");
            for (int i = 0; i < members.size(); i++) {
                if (i > 0) sb.append(",");
                sb.append(memberValueJson(members.get(i)));
            }
            sb.append("]");
            JsonLines.successDataSet(dsRef, sb.toString());
            return true;
        } catch (Exception e) {
            JsonLines.error("read-dataset", dsRef, e.getMessage());
            return false;
        }
    }

    private static String memberValueJson(FcModelNode member) {
        if (member instanceof BdaBoolean bda)      return String.valueOf(bda.getValue());
        if (member instanceof BdaInt32 bda)        return String.valueOf(bda.getValue());
        if (member instanceof BdaInt8 bda)         return String.valueOf(bda.getValue());
        if (member instanceof BdaFloat32 bda)      return String.valueOf(bda.getFloat());
        if (member instanceof BdaVisibleString bda) return JsonLines.quote(bda.getStringValue());
        return "null";
    }
}
