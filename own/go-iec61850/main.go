// SPDX-License-Identifier: MIT

// Command iec61850-interop is the iec61850-interop-otfabric image:
// go-iec61850 (and the go-mms it is built on) behind the adapter contract
// (docs/CONTAINER_CONTRACT.md, docs/goose.md, docs/sv.md).
//
// One binary serves every command. The image links it as otfabric-<command>
// (otfabric-ied-server, otfabric-ied-client, ...), and the command is taken
// from the name it is started as; "iec61850-interop <command> [flags]" works
// as well.
//
// It is OT Fabric's own stack, not an independent implementation: results
// against it are never independent evidence for go-iec61850 or go-mms
// (README.md).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
)

const (
	adapterName = "otfabric"
	prefix      = adapterName + "-"
	iedFixture  = "iec61850-v3"
	iedName     = "InteropIED"
	ldInst      = "InteropLD"
)

// Set with -ldflags by the Dockerfile.
var (
	adapterVersion   = "dev"
	upstreamVersion  = "dev"
	upstreamRevision = "0000000000000000000000000000000000000000"
	mmsRevision      = "0000000000000000000000000000000000000000"
)

// Exit codes of the contract.
const (
	exitOK          = 0
	exitArgs        = 1
	exitConnect     = 2
	exitOpFailed    = 3
	exitUnsupported = 4
)

const mmsModule = "github.com/otfabric/go-mms"

// commandFunc runs one command with its flags and returns the exit code.
type commandFunc func(args []string) int

// commands are the commands of this image, by name without the prefix.
// unsupported holds the contract's commands go-iec61850 cannot provide.
var (
	commands = map[string]commandFunc{
		"mms-server":       runMMSServer,
		"mms-client":       runMMSClient,
		"ied-server":       runIEDServer,
		"ied-client":       runIEDClient,
		"ied-controller":   runIEDController,
		"ied-reporter":     runIEDReporter,
		"goose-publisher":  runGOOSEPublisher,
		"goose-subscriber": runGOOSESubscriber,
		"sv-publisher":     runSVPublisher,
		"sv-subscriber":    runSVSubscriber,
	}
	unsupported = map[string]string{}
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
	_, _ = fmt.Fprintf(stderr, "otfabric: "+format+"\n", a...)
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

// upstream is what the image says of the stack behind it (contract 1.2).
func upstream() map[string]any {
	return map[string]any{
		"name":              "go-iec61850",
		"version":           upstreamVersion,
		"revision":          upstreamRevision,
		"license":           "MIT",
		"mmsVersion":        moduleVersion(mmsModule),
		"mmsRevision":       mmsRevision,
		"independentEngine": false,
	}
}

// moduleVersion is the version of a dependency compiled into the binary.
func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range info.Deps {
		if d.Path == path {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return "unknown"
}

func versionEvent() map[string]any {
	return map[string]any{"event": "version", "adapterVersion": adapterVersion, "fixtureRevision": iedFixture, "upstream": upstream()}
}

func capabilitiesEvent() map[string]any {
	return map[string]any{
		"event": "capabilities", "adapterVersion": adapterVersion, "fixtureRevision": iedFixture,
		"commands": supportedCommands(), "upstream": upstream(),
	}
}

// ready emits the ready event of an MMS or IED server.
func ready(addr, fixture string) {
	emit(map[string]any{
		"event": "ready", "address": addr, "fixture": fixture, "adapter": adapterName,
		"version": adapterVersion, "ied_name": iedName,
	})
}
