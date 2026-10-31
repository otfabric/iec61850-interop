// SPDX-License-Identifier: GPL-3.0-only

package jl

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// capture runs f with os.Stdout redirected and returns what it wrote.
func capture(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	f()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-out
}

func TestVersion(t *testing.T) {
	t.Setenv("ADAPTER_VERSION", "")
	if got := Version(); got != "dev" {
		t.Errorf("unset: got %q", got)
	}
	t.Setenv("ADAPTER_VERSION", "1.2.3")
	if got := Version(); got != "1.2.3" {
		t.Errorf("set: got %q", got)
	}
}

func TestEmitWritesOneJSONLine(t *testing.T) {
	got := capture(t, func() { Emit(map[string]int{"a": 1}) })
	if got != "{\"a\":1}\n" {
		t.Errorf("got %q", got)
	}
}

func TestEmitUnmarshalableWritesNothing(t *testing.T) {
	if got := capture(t, func() { Emit(make(chan int)) }); got != "" {
		t.Errorf("stdout carries the contract and must stay valid JSON Lines, got %q", got)
	}
}

// Consumers parse stdout line by line, so concurrent writers must not
// interleave within a line.
func TestEmitConcurrentLinesStayWhole(t *testing.T) {
	const writers, each = 8, 50
	got := capture(t, func() {
		var wg sync.WaitGroup
		for range writers {
			wg.Go(func() {
				for range each {
					Emit(map[string]string{"k": strings.Repeat("x", 512)})
				}
			})
		}
		wg.Wait()
	})
	sc := bufio.NewScanner(strings.NewReader(got))
	sc.Buffer(make([]byte, 4096), 1<<20)
	n := 0
	for sc.Scan() {
		var v map[string]string
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		n++
	}
	if n != writers*each {
		t.Errorf("got %d lines, want %d", n, writers*each)
	}
}

func TestReadyEvent(t *testing.T) {
	t.Setenv("ADAPTER_VERSION", "9.9")
	got := capture(t, func() { Ready("0.0.0.0:102", FixtureRevision, "InteropIED") })
	want := `{"event":"ready","address":"0.0.0.0:102","fixture":"iec61850-v3","adapter":"dscsystems","version":"9.9","ied_name":"InteropIED"}` + "\n"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// capabilities.json is the declared truth of what the images offer, and
// --capabilities must report the same (AGENTS.md rule 2).
func TestCommandsAndRevisionsMatchCapabilities(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "fixtures", "capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	var caps struct {
		FixtureRevision      string          `json:"fixtureRevision"`
		MMSFixtureRevision   string          `json:"mmsFixtureRevision"`
		GooseFixtureRevision string          `json:"gooseFixtureRevision"`
		SVFixtureRevision    string          `json:"svFixtureRevision"`
		Commands             map[string]bool `json:"commands"`
	}
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatal(err)
	}
	var declared []string
	for name, on := range caps.Commands {
		if on && strings.HasPrefix(name, Adapter+"-") {
			declared = append(declared, name)
		}
	}
	slices.Sort(declared)
	have := slices.Sorted(slices.Values(Commands))
	if !slices.Equal(have, declared) {
		t.Errorf("Commands = %v\ncapabilities.json = %v", have, declared)
	}
	for _, c := range []struct{ name, have, want string }{
		{"fixture", FixtureRevision, caps.FixtureRevision},
		{"mms", MMSFixtureRevision, caps.MMSFixtureRevision},
		{"goose", GooseFixtureRevision, caps.GooseFixtureRevision},
		{"sv", SVFixtureRevision, caps.SVFixtureRevision},
	} {
		if c.have != c.want {
			t.Errorf("%s revision: %q, capabilities.json has %q", c.name, c.have, c.want)
		}
	}
}

// TestMetaHelper is the child process of TestHandleMetaFlags: HandleMetaFlags
// exits, so it needs a process of its own.
func TestMetaHelper(t *testing.T) {
	arg := os.Getenv("JL_META_ARG")
	if arg == "" {
		return
	}
	os.Args = []string{"adapter", arg}
	HandleMetaFlags()
	os.Exit(7) // not handled
}

func TestHandleMetaFlags(t *testing.T) {
	cases := []struct {
		arg, event string
		exit       int
	}{
		{"--version", "version", 0},
		{"--capabilities", "capabilities", 0},
		{"--other", "", 7},
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMetaHelper$")
			cmd.Env = append(os.Environ(), "JL_META_ARG="+c.arg, "ADAPTER_VERSION=4.5")
			out, err := cmd.Output()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != c.exit {
				t.Fatalf("exit %d, want %d", code, c.exit)
			}
			if c.event == "" {
				return
			}
			var got struct {
				Event           string   `json:"event"`
				AdapterVersion  string   `json:"adapterVersion"`
				FixtureRevision string   `json:"fixtureRevision"`
				Commands        []string `json:"commands"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("%q: %v", out, err)
			}
			if got.Event != c.event || got.AdapterVersion != "4.5" || got.FixtureRevision != FixtureRevision {
				t.Errorf("got %+v", got)
			}
			if c.event == "capabilities" && !slices.Equal(got.Commands, Commands) {
				t.Errorf("commands %v", got.Commands)
			}
		})
	}
}
