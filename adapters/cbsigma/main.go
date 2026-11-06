// SPDX-License-Identifier: MIT

// Command iec61850-interop is the iec61850-interop-cbsigma image:
// cB-Sigma/iec61850 (client, GOOSE) and cB-Sigma/iec61850-sim (the simulated
// IED, the server) behind the adapter contract (docs/CONTAINER_CONTRACT.md,
// docs/goose.md). An independent implementation: Apache-2.0 Go, written
// apart from OT Fabric's stacks.
//
// One binary serves every command. The image links it as cbsigma-<command>
// (cbsigma-ied-server, cbsigma-ied-client, ...), and the command is taken
// from the name it is started as; "iec61850-interop <command> [flags]" works
// as well.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const (
	adapterName = "cbsigma"
	prefix      = adapterName + "-"
	iedFixture  = "iec61850-v3"
	iedName     = "InteropIED"
	ldInst      = "InteropLD"
)

// Set with -ldflags by the Dockerfile.
var adapterVersion = "dev"

// Exit codes of the contract.
const (
	exitOK          = 0
	exitArgs        = 1
	exitConnect     = 2
	exitOpFailed    = 3
	exitUnsupported = 4
)

// commandFunc runs one command with its flags and returns the exit code.
type commandFunc func(args []string) int

// commands are the commands of the contract, by name without the prefix.
// unsupported holds those cB-Sigma cannot provide, with the reason.
var (
	commands = map[string]commandFunc{
		"mms-server":       nil,
		"mms-client":       nil,
		"ied-server":       runIEDServer,
		"ied-client":       runIEDClient,
		"ied-controller":   runIEDController,
		"ied-reporter":     runIEDReporter,
		"goose-publisher":  runGOOSEPublisher,
		"goose-subscriber": runGOOSESubscriber,
		"sv-publisher":     nil,
		"sv-subscriber":    nil,
	}
	unsupported = map[string]string{
		"mms-server": "cB-Sigma has no generic MMS server: iec61850-sim serves an IED (cbsigma-ied-server), " +
			"not the named variables, file store and journals of fixture mms-v2",
		"mms-client": "not adapted: the IEC 61850 services of the client are tested through cbsigma-ied-*",
		"sv-publisher": "not adapted: the focus of this adapter is MMS, reports, controls and GOOSE " +
			"(cB-Sigma/iec61850 has a Sampled Values publisher)",
		"sv-subscriber": "not adapted: the focus of this adapter is MMS, reports, controls and GOOSE",
	}
)

var (
	stdoutMu sync.Mutex
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
)

// emit prints one JSON object on one flushed line of stdout.
func emit(doc any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		logf("cannot encode an event: %v", err)
		return
	}
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	_, _ = stdout.Write(buf.Bytes())
}

func logf(format string, a ...any) {
	_, _ = fmt.Fprintf(stderr, "cbsigma: "+format+"\n", a...)
}

// argError logs an argument or startup error and returns exit code 1.
func argError(format string, a ...any) int {
	logf(format, a...)
	return exitArgs
}

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

// run dispatches on the name the binary is started as, or on the first
// argument when it is started under its own name.
func run(name string, args []string) int {
	cmd, ok := strings.CutPrefix(name, prefix)
	if !ok {
		if len(args) == 0 {
			return argError("usage: %s <command> [flags]; commands: %s", name, strings.Join(commandNames(), ", "))
		}
		cmd, args = strings.TrimPrefix(args[0], prefix), args[1:]
	}
	if _, known := commands[cmd]; !known {
		return argError("unknown command %q; commands: %s", cmd, strings.Join(commandNames(), ", "))
	}
	if len(args) > 0 {
		switch args[0] {
		case "--version":
			emit(versionEvent())
			return exitOK
		case "--capabilities":
			emit(capabilitiesEvent())
			return exitOK
		}
	}
	if reason, no := unsupported[cmd]; no {
		emit(map[string]any{"event": "error", "code": "unsupported", "command": prefix + cmd, "reason": reason})
		return exitUnsupported
	}
	return commands[cmd](args)
}

func commandNames() []string {
	var out []string
	for c := range commands {
		out = append(out, prefix+c)
	}
	slices.Sort(out)
	return out
}

// supportedCommands are the commands that do not answer unsupported.
func supportedCommands() []string {
	var out []string
	for _, c := range commandNames() {
		if _, no := unsupported[strings.TrimPrefix(c, prefix)]; !no {
			out = append(out, c)
		}
	}
	return out
}

// The reference images carry no "upstream" object (contract 1.2): their
// stack is the one of versions.yaml.
func versionEvent() map[string]any {
	return map[string]any{"event": "version", "adapterVersion": adapterVersion, "fixtureRevision": iedFixture}
}

func capabilitiesEvent() map[string]any {
	return map[string]any{
		"event": "capabilities", "adapterVersion": adapterVersion, "fixtureRevision": iedFixture,
		"commands": supportedCommands(),
	}
}

// ready emits the ready event of the IED server.
func ready(addr string) {
	emit(map[string]any{
		"event": "ready", "address": addr, "fixture": iedFixture, "adapter": adapterName,
		"version": adapterVersion, "ied_name": iedName,
	})
}
