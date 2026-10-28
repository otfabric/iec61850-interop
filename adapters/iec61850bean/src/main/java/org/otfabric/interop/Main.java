// SPDX-License-Identifier: MIT
package org.otfabric.interop;

import java.util.Arrays;

/**
 * Entry point dispatched by the iec61850bean-ied-server, iec61850bean-ied-client,
 * iec61850bean-ied-reporter, and iec61850bean-ied-controller wrapper scripts.
 *
 * Usage:
 *   java -jar iec61850bean-adapter.jar server     [--port P] [--icd PATH] [--values PATH]
 *   java -jar iec61850bean-adapter.jar client     [--host H] [--port P]
 *   java -jar iec61850bean-adapter.jar reporter   [--host H] [--port P] [--sps1-initial 0|1]
 *   java -jar iec61850bean-adapter.jar controller [--host H] [--port P] [--ctlval 0|1]
 *
 * Meta flags (any subcommand or as the sole argument):
 *   --capabilities   Emit capabilities JSON and exit 0.
 *   --version        Emit version JSON and exit 0.
 */
public class Main {
    private static final String FIXTURE_REVISION = "iec61850-v3";

    public static void main(String[] args) throws Exception {
        // Handle meta flags regardless of subcommand position.
        for (String arg : args) {
            if ("--capabilities".equals(arg)) {
                String version = System.getenv("ADAPTER_VERSION");
                if (version == null) version = "dev";
                System.out.printf(
                    "{\"event\":\"capabilities\",\"adapterVersion\":\"%s\","
                    + "\"fixtureRevision\":\"%s\","
                    + "\"commands\":[\"iec61850bean-ied-server\",\"iec61850bean-ied-client\","
                    + "\"iec61850bean-ied-controller\",\"iec61850bean-ied-reporter\"]}%n",
                    version, FIXTURE_REVISION);
                System.exit(0);
            }
            if ("--version".equals(arg)) {
                String version = System.getenv("ADAPTER_VERSION");
                if (version == null) version = "dev";
                System.out.printf(
                    "{\"event\":\"version\",\"adapterVersion\":\"%s\","
                    + "\"fixtureRevision\":\"%s\"}%n",
                    version, FIXTURE_REVISION);
                System.exit(0);
            }
        }

        if (args.length == 0) {
            System.err.println("Usage: iec61850bean-adapter <server|client|reporter|controller> [options]");
            System.exit(1);
        }
        String[] rest = Arrays.copyOfRange(args, 1, args.length);
        switch (args[0]) {
            case "server"     -> IedServer.run(rest);
            case "client"     -> IedClient.run(rest);
            case "reporter"   -> IedReporter.run(rest);
            case "controller" -> IedController.run(rest);
            default -> {
                System.err.println("Unknown command: " + args[0]);
                System.exit(1);
            }
        }
    }
}
