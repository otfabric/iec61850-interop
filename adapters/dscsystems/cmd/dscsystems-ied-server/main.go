// SPDX-License-Identifier: GPL-3.0-only

// Command dscsystems-ied-server is the dscsystems/go-iec61850 IED server
// adapter for mms-interop, the counterpart of libiec61850-ied-server.
//
// The model is loaded at start-up from the ICD (scl.LoadModel), not compiled
// in; the logical device is named IED name + inst, InteropIEDInteropLD, as
// libiec61850 and iec61850bean name it. Initial values come from values.json.
//
// Usage:
//
//	dscsystems-ied-server [--port PORT] [--icd FILE] [--values FILE]
//
// Lifecycle: load model, apply values, register control handlers, listen,
// emit {"event":"ready",...}, serve until SIGTERM / SIGINT.
//
// Exit codes: 0 clean shutdown, 1 startup / argument error.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
	"github.com/dscsystems/go-iec61850/scl"
	"github.com/dscsystems/go-iec61850/server"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/jl"
)

// controlled are the SPC objects of the fixture, by DO name: SPCSO1 direct,
// SPCSO2 SBO normal, SPCSO3 SBO enhanced (ctlModel from values.json).
var controlled = []string{"SPCSO1", "SPCSO2", "SPCSO3"}

func main() {
	jl.HandleMetaFlags()
	port := flag.Int("port", 1102, "listen port")
	icd := flag.String("icd", "/fixtures/iec61850/interop.icd", "SCL (ICD) file")
	valuesPath := flag.String("values", "/fixtures/iec61850/values.json", "initial values")
	flag.Parse()
	if *port <= 0 || *port > 65535 {
		jl.Fatal(1, "ied-server: invalid port %d", *port)
	}

	m, err := scl.LoadModel(*icd)
	if err != nil {
		jl.Fatal(1, "ied-server: load %s: %v", *icd, err)
	}
	values, err := loadValues(*valuesPath)
	if err != nil {
		jl.Fatal(1, "ied-server: %v", err)
	}

	// Writes the fixed IED-client sequence and the control tests need.
	// libiec61850 also allows ST (so *-ied-client can write Mod.stVal and
	// *-ied-reporter SPS1.stVal); go-iec61850/dscsystems drops ST from this
	// list by design (IEC 61850-7-2: status is read-only), so such writes
	// are refused with object-access-denied.
	srv := server.New(m,
		server.WithIdentity(server.Identity{Vendor: "dscsystems", Model: m.Name, Revision: jl.Version()}),
		server.WithWritableFCs(model.CF, model.DC, model.SP, model.ST),
	)

	if err := applyValues(srv, m, values); err != nil {
		jl.Fatal(1, "ied-server: %v", err)
	}

	// Accept every select and operate on the SPC objects. The library
	// enforces the control model (SBO read / SBOw select, ctlNum, enhanced
	// CommandTermination) and reflects an accepted ctlVal into stVal itself,
	// which is what the C handlers do by hand.
	ld := m.Name + "InteropLD"
	for _, do := range controlled {
		ref := model.ObjectReference(ld + "/GGIO1." + do)
		if m.Lookup(ref, model.ALL) == nil {
			jl.Fatal(1, "ied-server: %s not in model", ref)
		}
		srv.OnControl(ref, func(cc *server.ControlCtx) model.AddCause {
			phase := "operate"
			if cc.Select {
				phase = "select"
			}
			jl.Log("ied-server: %s %s ctlVal=%s test=%v", phase, cc.Ref, cc.Value, cc.Test)
			return model.AddCauseNone
		})
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		jl.Fatal(1, "ied-server: failed to start on port %d: %v", *port, err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	jl.Ready(fmt.Sprintf("0.0.0.0:%d", *port), jl.FixtureRevision, m.Name)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sig:
		srv.Close()
		<-served
	case err := <-served:
		srv.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			jl.Fatal(1, "ied-server: serve: %v", err)
		}
	}
}

// loadValues reads the "values" map of values.json: "InteropLD/LN.DO.DA"
// (no IED prefix) to a JSON scalar.
func loadValues(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Values map[string]any `json:"values"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Values, nil
}

// applyValues sets every values.json entry on the model, typed after the
// attribute. All of values.json is applied (the C sets the subset its
// fixture code names; DPS1.stVal and MMTR1.TotVAh.actVal are the extras).
func applyValues(srv *server.Server, m *model.Model, values map[string]any) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type set struct {
		ref model.ObjectReference
		fc  model.FC
		v   *mms.Value
	}
	var sets []set
	for _, k := range keys {
		ref := model.ObjectReference(m.Name + k)
		da := m.Attribute(ref, model.ALL)
		if da == nil {
			return fmt.Errorf("values.json: %s not in model", ref)
		}
		v, err := typed(da, values[k])
		if err != nil {
			return fmt.Errorf("values.json: %s: %w", ref, err)
		}
		sets = append(sets, set{ref, da.FC, v})
	}
	srv.Update(func(tx *server.Tx) {
		for _, s := range sets {
			tx.Set(s.ref, s.fc, s.v)
		}
	})
	return nil
}

// typed converts a values.json scalar to the attribute's MMS type.
func typed(da *model.DataAttribute, raw any) (*mms.Value, error) {
	switch da.Kind {
	case mms.TypeBoolean:
		if b, ok := raw.(bool); ok {
			return mms.NewBool(b), nil
		}
	case mms.TypeInteger:
		if n, ok := raw.(float64); ok {
			return mms.NewInt64(int64(n)), nil
		}
	case mms.TypeUnsigned:
		if n, ok := raw.(float64); ok && n >= 0 {
			return mms.NewUint32(uint32(n)), nil
		}
	case mms.TypeFloat32:
		if n, ok := raw.(float64); ok {
			return mms.NewFloat32(float32(n)), nil
		}
	case mms.TypeFloat64:
		if n, ok := raw.(float64); ok {
			return mms.NewFloat64(n), nil
		}
	case mms.TypeVisibleString:
		if s, ok := raw.(string); ok {
			return mms.NewVisibleString(s), nil
		}
	case mms.TypeMMSString:
		if s, ok := raw.(string); ok {
			return mms.NewMMSString(s), nil
		}
	case mms.TypeBitString:
		// Dbpos: values.json carries the enumeration (2 = on).
		if n, ok := raw.(float64); ok && da.BType == "Dbpos" {
			return model.Dbpos(int(n)).Value(), nil
		}
	}
	return nil, fmt.Errorf("value %v does not fit %s (%s)", raw, da.Kind, da.BType)
}
