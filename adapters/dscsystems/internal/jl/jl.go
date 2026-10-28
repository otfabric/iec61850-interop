// SPDX-License-Identifier: GPL-3.0-only

// Package jl writes the mms-interop adapter contract: JSON Lines on
// stdout, human-readable diagnostics on stderr.
package jl

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Adapter is the adapter name in ready, version and capabilities events.
const Adapter = "dscsystems"

// FixtureRevision is the IEC 61850 fixture revision; MMSFixtureRevision
// the MMS one and GooseFixtureRevision the GOOSE one.
const (
	FixtureRevision      = "iec61850-v2"
	MMSFixtureRevision   = "mms-v2"
	GooseFixtureRevision = "goose-v1"
)

// Commands lists every command of the image, as --capabilities reports it.
var Commands = []string{
	"dscsystems-mms-server",
	"dscsystems-mms-client",
	"dscsystems-ied-server",
	"dscsystems-ied-client",
	"dscsystems-ied-controller",
	"dscsystems-ied-reporter",
	"dscsystems-goose-publisher",
	"dscsystems-goose-subscriber",
}

var mu sync.Mutex

// Version is the image version from $ADAPTER_VERSION, "dev" when unset.
func Version() string {
	if v := os.Getenv("ADAPTER_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// Emit writes v as one JSON line on stdout. Safe for concurrent use.
func Emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		Log("jl: marshal: %v", err)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	os.Stdout.Write(append(b, '\n'))
}

// Log writes a diagnostic line on stderr.
func Log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(os.Stderr, strings.TrimRight(msg, "\n"))
}

// Fatal logs and exits with code.
func Fatal(code int, format string, args ...any) {
	Log(format, args...)
	os.Exit(code)
}

// Ready emits the readiness event. iedName may be empty.
func Ready(address, fixture, iedName string) {
	Emit(struct {
		Event   string `json:"event"`
		Address string `json:"address"`
		Fixture string `json:"fixture"`
		Adapter string `json:"adapter"`
		Version string `json:"version"`
		IEDName string `json:"ied_name"`
	}{"ready", address, fixture, Adapter, Version(), iedName})
}

// HandleMetaFlags answers --version and --capabilities when they are the
// first argument, and exits 0. Call it first in main.
func HandleMetaFlags() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "--version":
		Emit(struct {
			Event           string `json:"event"`
			AdapterVersion  string `json:"adapterVersion"`
			FixtureRevision string `json:"fixtureRevision"`
		}{"version", Version(), FixtureRevision})
		os.Exit(0)
	case "--capabilities":
		Emit(struct {
			Event           string   `json:"event"`
			AdapterVersion  string   `json:"adapterVersion"`
			FixtureRevision string   `json:"fixtureRevision"`
			Commands        []string `json:"commands"`
		}{"capabilities", Version(), FixtureRevision, Commands})
		os.Exit(0)
	}
}
