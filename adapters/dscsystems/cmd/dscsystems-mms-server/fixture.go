// SPDX-License-Identifier: GPL-3.0-only

// fixture.go — parse an mms-interop fixture JSON file (revision mms-v2)
// into type specifications and initial values for the generic MMS server.
//
// Port of adapters/libiec61850/src/fixture.c. Scalar values use the typed
// JSON form of internal/mmsjson ({"type":..,"value":..}); arrays take an
// "elementType" and a JSON array of plain values; structure components
// without a "name" are called comp0, comp1, ... as in the C adapter.
//
// Type sizes follow fixture.c: integer and unsigned default to 32 bits,
// strings and bit strings without a "size" are declared as -1.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/mms-interop/adapters/dscsystems/internal/mmsjson"
)

// varDef is one domain variable: its type, initial value and writability.
type varDef struct {
	name     string
	spec     *mms.TypeSpec
	value    *mms.Value
	writable bool
}

// journalEntry is one fixture journal entry: a single data variable plus
// the ReasonCode libiec61850 adds to every entry.
type journalEntry struct {
	entryID    uint64
	time       time.Time
	tag        string
	value      *mms.Value
	reasonCode uint8
}

type journalDef struct {
	name    string
	entries []journalEntry // ascending by time, as in the fixture
}

type domainDef struct {
	name     string
	vars     []*varDef // sorted by name
	journals []*journalDef
}

// fileDef is a file of the virtual file store; name uses '/' as separator.
type fileDef struct {
	name         string
	data         []byte
	lastModified time.Time // zero when the fixture gives none
}

// reportDef is an InformationReport sent when report-trigger is written.
// vmdList is empty for the listOfVariable form; variables are
// "domain/item" references.
type reportDef struct {
	vmdList   string
	variables []string
}

type fixtureDef struct {
	revision       string
	vendor         string
	model          string
	revisionString string
	logical        int
	physical       int
	domains        []*domainDef // sorted by name
	files          []*fileDef
	reports        []reportDef
}

// JSON shape of the fixture file.
type jsonFixture struct {
	Revision string `json:"revision"`
	Identity struct {
		Vendor   string `json:"vendor"`
		Model    string `json:"model"`
		Revision string `json:"revision"`
	} `json:"identity"`
	Status struct {
		Logical  int `json:"logical"`
		Physical int `json:"physical"`
	} `json:"status"`
	Domains map[string]struct {
		Variables map[string]jsonVar `json:"variables"`
		Journals  map[string]struct {
			Entries []struct {
				EntryID        uint64 `json:"entryId"`
				OccurrenceTime string `json:"occurrenceTime"`
				Tag            string `json:"tag"`
				Type           string `json:"type"`
				Value          any    `json:"value"`
				ReasonCode     uint8  `json:"reasonCode"`
			} `json:"entries"`
		} `json:"journals"`
	} `json:"domains"`
	InformationReports []struct {
		VMDList   string   `json:"vmdList"`
		Variables []string `json:"variables"`
	} `json:"informationReports"`
	Files map[string]struct {
		Content      *string `json:"content"`
		Hex          *string `json:"hex"`
		Size         *int    `json:"size"`
		LastModified string  `json:"lastModified"`
	} `json:"files"`
}

type jsonVar struct {
	Type        string `json:"type"`
	Size        int    `json:"size"`
	Value       any    `json:"value"`
	Writable    bool   `json:"writable"`
	ElementType string `json:"elementType"`
	Count       int    `json:"count"`
}

type jsonComponent struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int    `json:"size"`
	Value any    `json:"value"`
}

// loadFixture parses the fixture at path.
func loadFixture(path string) (*fixtureDef, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var jf jsonFixture
	if err := json.Unmarshal(raw, &jf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	f := &fixtureDef{
		revision:       orDefault(jf.Revision, "mms-v1"),
		vendor:         orDefault(jf.Identity.Vendor, "OTFabric"),
		model:          orDefault(jf.Identity.Model, "mms-interop"),
		revisionString: orDefault(jf.Identity.Revision, "1.0"),
		logical:        jf.Status.Logical,
		physical:       jf.Status.Physical,
	}
	if jf.Domains == nil {
		return nil, fmt.Errorf("missing 'domains' key")
	}

	for _, dname := range sortedKeys(jf.Domains) {
		jd := jf.Domains[dname]
		d := &domainDef{name: dname}
		for _, vname := range sortedKeys(jd.Variables) {
			v, err := buildVar(vname, jd.Variables[vname])
			if err != nil {
				return nil, fmt.Errorf("variable %s/%s: %w", dname, vname, err)
			}
			d.vars = append(d.vars, v)
		}
		for _, jname := range sortedKeys(jd.Journals) {
			j := &journalDef{name: jname}
			for i, e := range jd.Journals[jname].Entries {
				t, err := time.Parse(time.RFC3339Nano, e.OccurrenceTime)
				if err != nil {
					return nil, fmt.Errorf("journal %s entry %d: %w", jname, i, err)
				}
				v, err := mmsjson.ToValue(mmsjson.Typed{Type: e.Type, Value: e.Value})
				if err != nil {
					return nil, fmt.Errorf("journal %s entry %d: %w", jname, i, err)
				}
				j.entries = append(j.entries, journalEntry{e.EntryID, t, e.Tag, v, e.ReasonCode})
			}
			d.journals = append(d.journals, j)
		}
		f.domains = append(f.domains, d)
	}

	for _, name := range sortedKeys(jf.Files) {
		jfile := jf.Files[name]
		fd := &fileDef{name: name}
		switch {
		case jfile.Content != nil:
			fd.data = []byte(*jfile.Content)
		case jfile.Hex != nil:
			b, err := hex.DecodeString(*jfile.Hex)
			if err != nil {
				return nil, fmt.Errorf("file %s: bad hex: %w", name, err)
			}
			fd.data = b
		case jfile.Size != nil:
			// generated content: byte i is i % 251
			fd.data = make([]byte, *jfile.Size)
			for i := range fd.data {
				fd.data[i] = byte(i % 251)
			}
		default:
			return nil, fmt.Errorf("file %s needs content, hex or size", name)
		}
		if jfile.LastModified != "" {
			t, err := time.Parse(time.RFC3339Nano, jfile.LastModified)
			if err != nil {
				return nil, fmt.Errorf("file %s: lastModified: %w", name, err)
			}
			fd.lastModified = t
		}
		f.files = append(f.files, fd)
	}

	for _, r := range jf.InformationReports {
		if r.Variables == nil {
			return nil, fmt.Errorf("informationReport missing 'variables'")
		}
		f.reports = append(f.reports, reportDef{vmdList: r.VMDList, variables: r.Variables})
	}
	return f, nil
}

// buildVar creates the type specification and initial value of a fixture
// variable.
func buildVar(name string, jv jsonVar) (*varDef, error) {
	v := &varDef{name: name, writable: jv.Writable}
	switch jv.Type {
	case "array":
		if jv.ElementType == "" {
			return nil, fmt.Errorf("array missing 'elementType'")
		}
		elems, ok := jv.Value.([]any)
		if !ok {
			return nil, fmt.Errorf("array value is not a JSON array")
		}
		count := jv.Count
		if count <= 0 {
			count = len(elems)
		}
		elemSpec, err := scalarSpec(jv.ElementType, 0)
		if err != nil {
			return nil, err
		}
		vals := make([]*mms.Value, len(elems))
		for i, e := range elems {
			if vals[i], err = mmsjson.ToValue(mmsjson.Typed{Type: jv.ElementType, Value: e}); err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
		}
		v.spec = &mms.TypeSpec{Kind: mms.TypeArray, Elements: count, Element: elemSpec}
		v.value = mms.NewArray(vals...)
	case "structure":
		// "value" is a JSON array of {"name","type","value"} components
		raw, _ := json.Marshal(jv.Value)
		var comps []jsonComponent
		if err := json.Unmarshal(raw, &comps); err != nil {
			return nil, fmt.Errorf("structure value: %w", err)
		}
		v.spec = &mms.TypeSpec{Kind: mms.TypeStructure}
		vals := make([]*mms.Value, len(comps))
		for i, c := range comps {
			cs, err := scalarSpec(c.Type, c.Size)
			if err != nil {
				return nil, fmt.Errorf("component %d: %w", i, err)
			}
			cname := c.Name
			if cname == "" {
				cname = fmt.Sprintf("comp%d", i)
			}
			v.spec.Components = append(v.spec.Components, mms.Component{Name: cname, Spec: cs})
			if vals[i], err = mmsjson.ToValue(mmsjson.Typed{Type: c.Type, Value: c.Value}); err != nil {
				return nil, fmt.Errorf("component %d: %w", i, err)
			}
		}
		v.value = mms.NewStructure(vals...)
	default:
		spec, err := scalarSpec(jv.Type, jv.Size)
		if err != nil {
			return nil, err
		}
		v.spec = spec
		if v.value, err = mmsjson.ToValue(mmsjson.Typed{Type: jv.Type, Value: jv.Value}); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// scalarSpec is make_spec of fixture.c: the type specification of a
// scalar fixture type.
func scalarSpec(typ string, size int) (*mms.TypeSpec, error) {
	sized := func(def int) int {
		if size > 0 {
			return size
		}
		return def
	}
	switch typ {
	case "boolean":
		return &mms.TypeSpec{Kind: mms.TypeBoolean}, nil
	case "integer":
		return &mms.TypeSpec{Kind: mms.TypeInteger, Size: sized(32)}, nil
	case "unsigned":
		return &mms.TypeSpec{Kind: mms.TypeUnsigned, Size: sized(32)}, nil
	case "float32":
		return &mms.TypeSpec{Kind: mms.TypeFloat32}, nil
	case "visible-string":
		return &mms.TypeSpec{Kind: mms.TypeVisibleString, Size: sized(-1)}, nil
	case "octet-string":
		return &mms.TypeSpec{Kind: mms.TypeOctetString, Size: sized(-1)}, nil
	case "bit-string":
		return &mms.TypeSpec{Kind: mms.TypeBitString, Size: sized(-1)}, nil
	case "utc-time":
		return &mms.TypeSpec{Kind: mms.TypeUTCTime}, nil
	}
	return nil, fmt.Errorf("unsupported type %q", typ)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
