// SPDX-License-Identifier: MIT

package main

import (
	"context"
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

	iec61850 "github.com/otfabric/go-iec61850"
	"github.com/otfabric/go-iec61850/scl"
	mms "github.com/otfabric/go-mms"
	"github.com/otfabric/go-mms/transport/iso"
)

// controls are the controllable objects of the fixture, with the ctlModel
// values.json gives them.
var controls = []string{"GGIO1.SPCSO1", "GGIO1.SPCSO2", "GGIO1.SPCSO3"}

// runIEDServer is otfabric-ied-server: the fixture IED on go-iec61850's
// server, with its data sets, report control blocks (reports enabled) and
// the three SPCSO controls, whose operate sets stVal to ctlVal unless test.
func runIEDServer(args []string) int {
	fs := flag.NewFlagSet("otfabric-ied-server", flag.ContinueOnError)
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
	defer srv.Close()

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	ln, err := iso.Listen(addr)
	if err != nil {
		return argError("ied-server: listen %s: %v", addr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx, ln) }()
	ready(addr, iedFixture)
	select {
	case <-ctx.Done():
		_ = ln.Close()
		<-served
		return exitOK
	case err := <-served:
		if err != nil && !errors.Is(err, context.Canceled) {
			logf("ied-server: %v", err)
			return exitArgs
		}
		return exitOK
	}
}

// newIEDServer builds the server of the fixture: model from the ICD,
// values.json applied, reports enabled, controls registered.
func newIEDServer(icdPath, valuesPath string) (*iec61850.Server, error) {
	doc, err := scl.ParseFile(icdPath)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", icdPath, err)
	}
	model, err := iec61850.NewServerModelFromSCL(doc, iedName, "")
	if err != nil {
		return nil, fmt.Errorf("model of %s: %w", icdPath, err)
	}
	srv, err := iec61850.NewServer(model, iec61850.ServerOptions{
		// The reporter of the contract triggers its data-change report
		// by writing GGIO1.SPS1.stVal, an ST attribute, as libiec61850's
		// server allows.
		AllowReadOnlyFCWrites: true,
		Identity:              &iec61850.ServerIdentity{Vendor: "OTFabric", Model: "otfabric-ied-server", Revision: "1.0"},
	})
	if err != nil {
		return nil, err
	}
	f, err := os.Open(valuesPath)
	if err != nil {
		srv.Close()
		return nil, err
	}
	defer f.Close()
	var names []string
	for _, ld := range model.LogicalDevices {
		names = append(names, ld.Name)
	}
	domain := domainOf(names)
	ctlModels, err := applyValues(srv, attributesOf(doc), domain, f)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("%s: %w", valuesPath, err)
	}
	srv.EnableReports()
	for _, do := range controls {
		m, ok := ctlModels[ldInst+"/"+do+".ctlModel"]
		if !ok {
			srv.Close()
			return nil, fmt.Errorf("%s has no ctlModel of %s", valuesPath, do)
		}
		key := storeKey(domain, strings.Replace(do, ".", "$ST$", 1)+"$stVal")
		if err := srv.RegisterControl(domain, do, iec61850.CtlModel(m), operateSetsStVal(srv, key)); err != nil {
			srv.Close()
			return nil, fmt.Errorf("control %s: %w", do, err)
		}
	}
	return srv, nil
}

// operateSetsStVal is the operate handler of the fixture controls: stVal :=
// ctlVal, unless the command is a test.
func operateSetsStVal(srv *iec61850.Server, key string) iec61850.ControlHandler {
	return iec61850.ControlHandler{
		OnOperate: func(ctx context.Context, req iec61850.ControlRequest) error {
			b, ok := req.CtlVal.Bool()
			if !ok {
				return fmt.Errorf("ctlVal is not a boolean")
			}
			if !req.Test {
				srv.SetValue(ctx, key, mms.NewBoolean(b))
			}
			return nil
		},
	}
}

// domainOf is the MMS domain go-iec61850 serves the fixture's logical
// device as, among the model's logical device names: "InteropIEDInteropLD",
// its IEC 61850-8-1 name, from go-iec61850 v1.7.0, or "InteropLD", the bare
// instance, before.
func domainOf(names []string) string {
	for _, n := range names {
		if n == iedName+ldInst {
			return n
		}
	}
	return resolveLD(names)
}

// applyValues sets values.json's "values" ("InteropLD/LN.DO.DA" -> scalar)
// in the server, each typed after the attribute's basic type in the model,
// and returns the integer values by key (for the ctlModels). domain is the
// MMS domain of values.json's "InteropLD".
func applyValues(srv *iec61850.Server, attrs attributes, domain string, r io.Reader) (map[string]int64, error) {
	var doc struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Values) == 0 {
		return nil, errors.New(`no "values" object`)
	}
	keys := make([]string, 0, len(doc.Values))
	for k := range doc.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ints := map[string]int64{}
	for _, k := range keys {
		ld, path, ok := strings.Cut(k, "/")
		if !ok {
			return nil, fmt.Errorf("%s is not LD/LN.DO.DA", k)
		}
		a, ok := attrs[k]
		fc, btype := a[0], a[1]
		if !ok {
			return nil, fmt.Errorf("%s is not an attribute of the model", k)
		}
		v, n, err := sclValue(btype, doc.Values[k])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if n != nil {
			ints[k] = *n
		}
		ln, rest, _ := strings.Cut(path, ".")
		if ld != ldInst {
			return nil, fmt.Errorf("%s is not in logical device %s", k, ldInst)
		}
		srv.ValueStore().Set(storeKey(domain, ln+"$"+fc+"$"+strings.ReplaceAll(rest, ".", "$")), v)
	}
	return ints, nil
}

// attributes are the FC and basic type of every leaf attribute of the SCL
// document, by "LD/LN.DO[.SDO].DA".
type attributes map[string][2]string

func attributesOf(doc *scl.SCL) attributes {
	out := attributes{}
	for _, r := range scl.Flatten(doc) {
		if r.IED == iedName && r.Status == "" {
			out[r.LD+"/"+r.LN+"."+r.Path] = [2]string{r.FC, r.BType}
		}
	}
	return out
}

// sclValue converts a values.json scalar to the MMS value of an SCL basic
// type. A Dbpos takes its enumeration (2 = on = "10").
func sclValue(btype string, raw json.RawMessage) (*mms.Value, *int64, error) {
	bad := func() (*mms.Value, *int64, error) {
		return nil, nil, fmt.Errorf("value %s does not fit %s", raw, btype)
	}
	switch {
	case btype == "BOOLEAN":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return bad()
		}
		return mms.NewBoolean(b), nil, nil
	case btype == "Enum" || strings.HasPrefix(btype, "INT") && !strings.HasSuffix(btype, "U"):
		var n int64
		if json.Unmarshal(raw, &n) != nil {
			return bad()
		}
		return mms.NewInteger(n), &n, nil
	case strings.HasPrefix(btype, "INT") && strings.HasSuffix(btype, "U"):
		var n uint64
		if json.Unmarshal(raw, &n) != nil {
			return bad()
		}
		return mms.NewUnsigned(n), nil, nil
	case btype == "FLOAT32" || btype == "FLOAT64":
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return bad()
		}
		return mms.NewFloat(f), nil, nil
	case strings.HasPrefix(btype, "VisString"):
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return bad()
		}
		return mms.NewVisibleString(s), nil, nil
	case strings.HasPrefix(btype, "Unicode"):
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return bad()
		}
		return mms.NewMmsString(s), nil, nil
	case btype == "Dbpos" || btype == "Tcmd":
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 3 {
			return bad()
		}
		return mms.NewBitStringWithLength([]byte{byte(n) << 6}, 2), nil, nil
	}
	return bad()
}

// storeKey is the value store key of an MMS item: "<LD>/<item ID>".
func storeKey(ld, itemID string) string { return ld + "/" + itemID }
