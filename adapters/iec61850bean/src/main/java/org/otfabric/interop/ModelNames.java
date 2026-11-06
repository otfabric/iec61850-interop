// SPDX-License-Identifier: MIT
package org.otfabric.interop;

import com.beanit.iec61850bean.LogicalDevice;
import com.beanit.iec61850bean.ModelNode;
import com.beanit.iec61850bean.ServerModel;

/** Name resolution against a retrieved server model (contract 1.3). */
final class ModelNames {

    /** The fixture's logical device name, without an IED-name prefix. */
    static final String LD_NAME = "InteropLD";

    private ModelNames() {}

    /**
     * The logical device named exactly "InteropLD", else the first whose name
     * ends in "InteropLD" (IED-name prefix, e.g. "InteropIEDInteropLD"),
     * else the first logical device of the model; null if there is none.
     */
    static LogicalDevice resolveLd(ServerModel model) {
        LogicalDevice first = null;
        LogicalDevice suffix = null;
        for (ModelNode node : model) {
            if (!(node instanceof LogicalDevice ld)) continue;
            String name = ld.getName();
            if (name.equals(LD_NAME)) return ld;
            if (first == null) first = ld;
            if (suffix == null && name.endsWith(LD_NAME)) suffix = ld;
        }
        return (suffix != null) ? suffix : first;
    }

    /** The name of {@link #resolveLd}, or "InteropLD" if the model has no LD. */
    static String resolveLdName(ServerModel model) {
        LogicalDevice ld = resolveLd(model);
        return (ld != null) ? ld.getName() : LD_NAME;
    }
}
