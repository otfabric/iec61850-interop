// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/cB-Sigma/iec61850-sim/sim"
	"github.com/cB-Sigma/iec61850/mms"
	"github.com/cB-Sigma/iec61850/scl"
)

// runIEDServer is cbsigma-ied-server: the IED of fixtures/iec61850/interop.icd
// on iec61850-sim, its model built by the simulator's own SCL loader
// (sim.LoadSCL: logical nodes, data sets, report control blocks and the
// controls with their ctlModel), then values.json applied. Operating a
// control sets its stVal (the simulated process).
func runIEDServer(args []string) int {
	fs := flag.NewFlagSet("cbsigma-ied-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 1102, "TCP port")
	icd := fs.String("icd", "/fixtures/iec61850/interop.icd", "SCL file of the IED")
	values := fs.String("values", "/fixtures/iec61850/values.json", "initial values")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("ied-server: unexpected argument %q", fs.Arg(0))
	}
	if *port <= 0 || *port > 65535 {
		return argError("ied-server: invalid port %d", *port)
	}
	srv, err := newIEDServer(*icd, *values)
	if err != nil {
		return argError("ied-server: %v", err)
	}
	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	if _, err := srv.Listen(addr); err != nil {
		return argError("ied-server: listen %s: %v", addr, err)
	}
	defer srv.Close()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	ready(addr)
	<-stop
	return exitOK
}

// newIEDServer loads the IED of the ICD into a simulator and applies the
// values.
func newIEDServer(icdPath, valuesPath string) (*sim.Server, error) {
	raw, err := os.ReadFile(icdPath)
	if err != nil {
		return nil, err
	}
	doc, err := scl.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", icdPath, err)
	}
	s := sim.New()
	warnings, err := s.LoadSCL(doc, iedName)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", icdPath, err)
	}
	for _, w := range warnings {
		logf("ied-server: %s", w)
	}
	f, err := os.Open(valuesPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := applyValues(s, f); err != nil {
		return nil, fmt.Errorf("%s: %w", valuesPath, err)
	}
	return s, nil
}

// valueFCs are the functional constraints values.json may address, in the
// order they are tried (values.json names attributes without one).
var valueFCs = []string{"ST", "MX", "CF", "DC", "SP", "SG", "SE", "EX"}

// applyValues sets values.json's "values" ("InteropLD/LN.DO.DA" -> scalar)
// in the simulator, each converted to the type the SCL loader gave the
// attribute.
func applyValues(s *sim.Server, r io.Reader) error {
	var doc struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return err
	}
	if len(doc.Values) == 0 {
		return errors.New(`no "values" object`)
	}
	keys := make([]string, 0, len(doc.Values))
	for k := range doc.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ld, path, ok := strings.Cut(k, "/")
		if !ok {
			return fmt.Errorf("%s is not LD/LN.DO.DA", k)
		}
		domain := iedName + ld
		ln, rest, _ := strings.Cut(path, ".")
		rest = strings.ReplaceAll(rest, ".", "$")
		found := false
		for _, fc := range valueFCs {
			item := ln + "$" + fc + "$" + rest
			cur, ok := s.Get(domain, item)
			if !ok {
				continue
			}
			v, err := jsonValue(cur, doc.Values[k])
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			s.Set(domain, item, v)
			found = true
			break
		}
		if !found {
			return fmt.Errorf("%s is not an attribute of the model", k)
		}
	}
	return nil
}

// jsonValue converts a values.json scalar to a value of the kind of cur. A
// Dbpos (2-bit bit-string) takes its enumeration (2 = on = "10").
func jsonValue(cur mms.Data, raw json.RawMessage) (mms.Data, error) {
	bad := fmt.Errorf("value %s does not fit the attribute's type", raw)
	switch cur.Kind {
	case mms.KBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return mms.Data{}, bad
		}
		return mms.Boolean(b), nil
	case mms.KInt:
		var n int64
		if json.Unmarshal(raw, &n) != nil {
			return mms.Data{}, bad
		}
		return mms.Integer(n), nil
	case mms.KUint:
		var n uint64
		if json.Unmarshal(raw, &n) != nil {
			return mms.Data{}, bad
		}
		return mms.Unsigned(n), nil
	case mms.KFloat:
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return mms.Data{}, bad
		}
		v := mms.Float32(f)
		v.Double = cur.Double
		return v, nil
	case mms.KVisible:
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return mms.Data{}, bad
		}
		return mms.Visible(str), nil
	case mms.KMMSString:
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return mms.Data{}, bad
		}
		return mms.MMSString(str), nil
	case mms.KBitString:
		var n int
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 3 || cur.Bits.NBits != 2 {
			return mms.Data{}, bad
		}
		b := mms.NewBitString(2)
		b.Set(0, n&2 != 0)
		b.Set(1, n&1 != 0)
		return mms.Bits(b), nil
	}
	return mms.Data{}, bad
}
