// SPDX-License-Identifier: GPL-3.0-only

// services.go — the per-association request handler: identify, status,
// name lists, variable access, named variable lists and InformationReport.
//
// Requests are decoded here (ISO 9506-2); values, type specifications and
// alternate access are decoded and encoded with the library's mms codecs.
// Behaviour follows the libiec61850 generic server the C adapter builds
// on, so a client sees the same answers from either image:
//   - GetNameList of domain variables lists structure components as
//     flattened "name$component" names, sorted;
//   - GetNameList of VMD-scope variables is refused (access
//     object-access-unsupported): the fixture has none;
//   - unknown variables read as object-non-existent, writes to read-only
//     variables are object-access-denied, mistyped writes
//     type-inconsistent.
package main

import (
	"errors"
	"net"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/dscsystems/go-iec61850/asn1"
	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

// Confirmed service CHOICE tags (ISO 9506-2).
const (
	svcStatus         = 0
	svcGetNameList    = 1
	svcIdentify       = 2
	svcRead           = 4
	svcWrite          = 5
	svcGetVarAccess   = 6
	svcDefineNVL      = 11
	svcGetNVLAttrs    = 12
	svcDeleteNVL      = 13
	svcObtainFile     = 46
	svcReadJournal    = 65
	svcFileOpen       = 72
	svcFileRead       = 73
	svcFileClose      = 74
	svcFileRename     = 75
	svcFileDelete     = 76
	svcFileDirectory  = 77
	reportTriggerName = "report-trigger"
)

// GetNameList object classes used by the fixture.
const (
	classNamedVariable     = 0
	classNamedVariableList = 2
	classJournal           = 8
	classDomain            = 9
)

var (
	// errUnrecognized rejects a service this server does not implement.
	errUnrecognized = &mms.ServiceError{Rejected: true, Class: 1, Code: 1}
	// errInvalidArgument rejects a request that cannot be decoded.
	errInvalidArgument = &mms.ServiceError{Rejected: true, Class: 1, Code: 4}
	// definition class: object-undefined, object-exists
	errObjectUndefined = &mms.ServiceError{Class: 2, Code: 1}
	errObjectExists    = &mms.ServiceError{Class: 2, Code: 2}
)

// scope of an ObjectName.
type scope int

const (
	scopeVMD scope = iota
	scopeDomain
	scopeAA
)

type objectName struct {
	scope  scope
	domain string
	item   string
}

// nvlMember is one named variable list member, with its alternate access
// kept as the raw content of alternateAccess [5].
type nvlMember struct {
	ref mms.VarRef
	alt []byte
}

type nvl struct {
	members   []nvlMember
	deletable bool
}

type domain struct {
	def      *domainDef
	vars     map[string]*varDef
	names    []string // flattened, sorted GetNameList answer
	journals map[string]*journalDef
	nvls     map[string]*nvl
}

// server is the model shared by all associations. mu guards the
// variable values and the domain and VMD named variable lists.
type server struct {
	mu       sync.Mutex
	fix      *fixtureDef
	domains  map[string]*domain
	domNames []string
	vmdNVLs  map[string]*nvl
	store    *os.Root

	connMu sync.Mutex
	conns  map[*mms.ServerConn]struct{}
}

func newServer(fix *fixtureDef, storeDir string) (*server, error) {
	root, err := os.OpenRoot(storeDir)
	if err != nil {
		return nil, err
	}
	s := &server{
		fix:     fix,
		domains: map[string]*domain{},
		vmdNVLs: map[string]*nvl{},
		store:   root,
		conns:   map[*mms.ServerConn]struct{}{},
	}
	for _, dd := range fix.domains {
		d := &domain{def: dd, vars: map[string]*varDef{}, journals: map[string]*journalDef{}, nvls: map[string]*nvl{}}
		for _, v := range dd.vars {
			d.vars[v.name] = v
			d.names = append(d.names, flatNames(v.name, v.spec)...)
			mode := "ro"
			if v.writable {
				mode = "rw"
			}
			jl.Log("  registered %s/%s (%s)", dd.name, v.name, mode)
		}
		sort.Strings(d.names)
		for _, j := range dd.journals {
			d.journals[j.name] = j
			jl.Log("  journal %s/%s (%d entries)", dd.name, j.name, len(j.entries))
		}
		s.domains[dd.name] = d
		s.domNames = append(s.domNames, dd.name)
	}
	return s, nil
}

// flatNames lists name and, for structures, every component as
// "name$component", the flattened name space libiec61850 answers with.
func flatNames(name string, spec *mms.TypeSpec) []string {
	out := []string{name}
	if spec.Kind == mms.TypeStructure {
		for _, c := range spec.Components {
			out = append(out, flatNames(name+"$"+c.Name, c.Spec)...)
		}
	}
	return out
}

// serve runs one association.
func (s *server) serve(raw net.Conn) {
	sc, err := mms.AcceptConnOpts(raw, acceptOptions())
	if err != nil {
		jl.Log("association from %s refused: %v", raw.RemoteAddr(), err)
		raw.Close()
		return
	}
	jl.Log("association from %s", raw.RemoteAddr())
	s.connMu.Lock()
	s.conns[sc] = struct{}{}
	s.connMu.Unlock()

	h := &handler{s: s, sc: sc, aaNVLs: map[string]*nvl{}, files: map[int32]*openFile{}}
	err = sc.Serve(h)
	h.closeFiles()
	sc.Close()

	s.connMu.Lock()
	delete(s.conns, sc)
	s.connMu.Unlock()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		jl.Log("association from %s ended: %v", raw.RemoteAddr(), err)
	} else {
		jl.Log("association from %s concluded", raw.RemoteAddr())
	}
}

// close aborts the open associations.
func (s *server) close() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for sc := range s.conns {
		sc.Abort()
	}
	s.store.Close()
}

// handler serves one association; requests arrive one at a time.
type handler struct {
	s      *server
	sc     *mms.ServerConn
	aaNVLs map[string]*nvl // association-specific lists

	files    map[int32]*openFile
	nextFRSM int32
}

func (h *handler) Handle(req *mms.Request) (*asn1.Element, error) {
	switch req.Service {
	case svcIdentify:
		return h.identify(), nil
	case svcStatus:
		return mms.EncodeStatusResponse(mms.ServerStatus{Logical: h.s.fix.logical, Physical: h.s.fix.physical}), nil
	case svcGetNameList:
		return h.getNameList(req.Content)
	case svcRead:
		return h.read(req.Content)
	case svcWrite:
		return h.write(req.Content)
	case svcGetVarAccess:
		return h.getVariableAccessAttributes(req.Content)
	case svcDefineNVL:
		return h.defineNVL(req.Content)
	case svcGetNVLAttrs:
		return h.getNVLAttributes(req.Content)
	case svcDeleteNVL:
		return h.deleteNVL(req.Content)
	case svcReadJournal:
		return h.readJournal(req.Content)
	case svcFileDirectory:
		return h.fileDirectory(req.Content)
	case svcFileOpen:
		return h.fileOpen(req.Content)
	case svcFileRead:
		return h.fileRead(req.Content)
	case svcFileClose:
		return h.fileClose(req.Content)
	case svcFileDelete:
		return h.fileDelete(req.Content)
	case svcFileRename:
		return h.fileRename(req.Content)
	case svcObtainFile:
		return h.obtainFile(req)
	}
	return nil, errUnrecognized
}

func (h *handler) identify() *asn1.Element {
	f := h.s.fix
	return asn1.Cons(asn1.ContextConstructed(svcIdentify),
		asn1.Prim(asn1.ContextPrimitive(0), []byte(f.vendor)),
		asn1.Prim(asn1.ContextPrimitive(1), []byte(f.model)),
		asn1.Prim(asn1.ContextPrimitive(2), []byte(f.revisionString)),
	)
}

// ---------------------------------------------------------------------------
// Object names
// ---------------------------------------------------------------------------

// parseObjectName decodes one ObjectName CHOICE element from dec.
func parseObjectName(dec *asn1.Decoder) (objectName, error) {
	tag, c, err := dec.ReadTLV()
	if err != nil {
		return objectName{}, err
	}
	switch tag {
	case asn1.ContextPrimitive(0):
		return objectName{scope: scopeVMD, item: string(c)}, nil
	case asn1.ContextConstructed(1):
		dd := asn1.NewDecoder(c)
		d, err := dd.Expect(asn1.TagVisibleString)
		if err != nil {
			return objectName{}, err
		}
		i, err := dd.Expect(asn1.TagVisibleString)
		if err != nil {
			return objectName{}, err
		}
		return objectName{scope: scopeDomain, domain: string(d), item: string(i)}, nil
	case asn1.ContextPrimitive(2):
		return objectName{scope: scopeAA, item: string(c)}, nil
	}
	return objectName{}, asn1.ErrUnexpected
}

func (n objectName) element() *asn1.Element {
	switch n.scope {
	case scopeDomain:
		return asn1.Cons(asn1.ContextConstructed(1),
			asn1.Prim(asn1.TagVisibleString, []byte(n.domain)),
			asn1.Prim(asn1.TagVisibleString, []byte(n.item)))
	case scopeAA:
		return asn1.Prim(asn1.ContextPrimitive(2), []byte(n.item))
	}
	return asn1.Prim(asn1.ContextPrimitive(0), []byte(n.item))
}

// ---------------------------------------------------------------------------
// GetNameList
// ---------------------------------------------------------------------------

func (h *handler) getNameList(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	// objectClass [0] { basicObjectClass [0] INTEGER }
	oc, err := dec.Expect(asn1.ContextConstructed(0))
	if err != nil {
		return nil, errInvalidArgument
	}
	cb, err := asn1.NewDecoder(oc).Expect(asn1.ContextPrimitive(0))
	if err != nil {
		return nil, errInvalidArgument
	}
	class, _ := asn1.DecodeInt(cb)
	// objectScope [1] CHOICE { vmdSpecific [0], domainSpecific [1], aaSpecific [2] }
	sc, err := dec.Expect(asn1.ContextConstructed(1))
	if err != nil {
		return nil, errInvalidArgument
	}
	stag, sval, err := asn1.NewDecoder(sc).ReadTLV()
	if err != nil {
		return nil, errInvalidArgument
	}
	var after string
	if a, ok, _ := dec.Optional(asn1.ContextPrimitive(2)); ok {
		after = string(a)
	}

	h.s.mu.Lock()
	var names []string
	var lerr error
	switch {
	case class == classDomain && stag.Number == 0:
		names = append(names, h.s.domNames...)
	case stag.Number == 1: // domain-specific
		d := h.s.domains[string(sval)]
		switch {
		case d == nil:
			lerr = mms.AccessObjectNonExistent
		case class == classNamedVariable:
			names = append(names, d.names...)
		case class == classNamedVariableList:
			names = sortedKeys(d.nvls)
		case class == classJournal:
			names = sortedKeys(d.journals)
		default:
			lerr = mms.AccessObjectAccessUnsupported
		}
	case class == classNamedVariableList && stag.Number == 0:
		names = sortedKeys(h.s.vmdNVLs)
	case class == classNamedVariableList && stag.Number == 2:
		names = sortedKeys(h.aaNVLs)
	default:
		lerr = mms.AccessObjectAccessUnsupported
	}
	h.s.mu.Unlock()
	if lerr != nil {
		return nil, lerr
	}

	if after != "" {
		for i, n := range names {
			if n == after {
				names = names[i+1:]
				break
			}
		}
	}
	// One page: as many names as fit the negotiated PDU size.
	budget := h.sc.MaxPDU - 32
	list := asn1.Cons(asn1.ContextConstructed(0))
	more, used := false, 0
	for i, n := range names {
		el := asn1.Prim(asn1.TagVisibleString, []byte(n))
		if h.sc.MaxPDU > 0 && i > 0 && used+el.Size() > budget {
			more = true
			break
		}
		used += el.Size()
		list.Add(el)
	}
	return asn1.Cons(asn1.ContextConstructed(svcGetNameList), list,
		asn1.BoolElem(asn1.ContextPrimitive(1), more)), nil
}

// ---------------------------------------------------------------------------
// Variable access
// ---------------------------------------------------------------------------

// target is a resolved variable access: the fixture variable, the path of
// child indexes to the accessed element and that element's type.
type target struct {
	v    *varDef
	path []int
	spec *mms.TypeSpec
}

// resolve finds domain/item, where item may be a flattened
// "name$component" reference, and applies the alternate access alt (the
// raw content of alternateAccess [5], nil for none). Index ranges and
// other multi-element selections are refused by mms.ParseAlternateAccess
// and answered object-access-unsupported. Call with s.mu held.
func (h *handler) resolve(ref mms.VarRef, alt []byte) (target, mms.DataAccessError, bool) {
	d := h.s.domains[ref.Domain]
	if d == nil || ref.Domain == "" {
		return target{}, mms.AccessObjectNonExistent, false
	}
	parts := strings.Split(ref.Item, "$")
	v := d.vars[parts[0]]
	if v == nil {
		return target{}, mms.AccessObjectNonExistent, false
	}
	var steps []mms.AccessStep
	for _, p := range parts[1:] {
		steps = append(steps, mms.AccessStep{Component: p})
	}
	if alt != nil {
		more, err := mms.ParseAlternateAccess(alt)
		if err != nil {
			if errors.Is(err, mms.ErrAlternateAccessUnsupported) {
				return target{}, mms.AccessObjectAccessUnsupported, false
			}
			return target{}, mms.AccessInvalidAddress, false
		}
		steps = append(steps, more...)
	}
	path, spec, err := mms.AccessPath(v.spec, steps)
	if err != nil {
		if len(parts) > 1 && alt == nil {
			return target{}, mms.AccessObjectNonExistent, false
		}
		return target{}, mms.AccessInvalidAddress, false
	}
	return target{v: v, path: path, spec: spec}, 0, true
}

func (t target) get() *mms.Value {
	cur := t.v.value
	for _, i := range t.path {
		cur = cur.Index(i)
	}
	return cur.Clone()
}

func (t target) set(nv *mms.Value) {
	if len(t.path) == 0 {
		t.v.value = nv
		return
	}
	cur := t.v.value
	for _, i := range t.path[:len(t.path)-1] {
		cur = cur.Index(i)
	}
	cur.SetIndex(t.path[len(t.path)-1], nv)
}

// conform returns v as a value of type spec, or false when it does not
// have that type (type-inconsistent). A float of either width is stored
// as the declared float32.
func conform(spec *mms.TypeSpec, v *mms.Value) (*mms.Value, bool) {
	switch spec.Kind {
	case mms.TypeFloat32:
		if v.Type() == mms.TypeFloat32 || v.Type() == mms.TypeFloat64 {
			return mms.NewFloat32(v.Float32()), true
		}
		return nil, false
	case mms.TypeArray:
		if v.Type() != mms.TypeArray || v.Len() != spec.Elements {
			return nil, false
		}
		out := make([]*mms.Value, v.Len())
		for i := range out {
			c, ok := conform(spec.Element, v.Index(i))
			if !ok {
				return nil, false
			}
			out[i] = c
		}
		return mms.NewArray(out...), true
	case mms.TypeStructure:
		if v.Type() != mms.TypeStructure || v.Len() != len(spec.Components) {
			return nil, false
		}
		out := make([]*mms.Value, v.Len())
		for i := range out {
			c, ok := conform(spec.Components[i].Spec, v.Index(i))
			if !ok {
				return nil, false
			}
			out[i] = c
		}
		return mms.NewStructure(out...), true
	case mms.TypeVisibleString, mms.TypeOctetString:
		// a positive size is the declared maximum length
		if v.Type() != spec.Kind || (spec.Size > 0 && len(v.Bytes()) > spec.Size) {
			return nil, false
		}
		return v.Clone(), true
	}
	if v.Type() != spec.Kind {
		return nil, false
	}
	return v.Clone(), true
}

// accessSpec is one variable of a VariableAccessSpecification.
type accessSpec struct {
	ref mms.VarRef
	alt []byte
}

// parseVariableAccess decodes a VariableAccessSpecification CHOICE:
// listOfVariable [0] or variableListName [1]. A list name is expanded to
// its members; an unknown list is access object-non-existent.
func (h *handler) parseVariableAccess(tag asn1.Tag, content []byte) ([]accessSpec, error) {
	switch tag {
	case asn1.ContextConstructed(0): // listOfVariable
		var out []accessSpec
		ld := asn1.NewDecoder(content)
		for ld.More() {
			entry, err := ld.Expect(asn1.TagSequence)
			if err != nil {
				return nil, errInvalidArgument
			}
			ed := asn1.NewDecoder(entry)
			vs, err := ed.Expect(asn1.ContextConstructed(0)) // variableSpecification: name [0]
			if err != nil {
				return nil, errInvalidArgument
			}
			n, err := parseObjectName(asn1.NewDecoder(vs))
			if err != nil {
				return nil, errInvalidArgument
			}
			a := accessSpec{ref: mms.VarRef{Domain: n.domain, Item: n.item}}
			if alt, ok, _ := ed.Optional(asn1.ContextConstructed(5)); ok {
				a.alt = alt
			}
			out = append(out, a)
		}
		return out, nil
	case asn1.ContextConstructed(1): // variableListName
		n, err := parseObjectName(asn1.NewDecoder(content))
		if err != nil {
			return nil, errInvalidArgument
		}
		l, lerr := h.lookupNVL(n)
		if lerr != nil {
			return nil, lerr
		}
		out := make([]accessSpec, len(l.members))
		for i, m := range l.members {
			out[i] = accessSpec{ref: m.ref, alt: m.alt}
		}
		return out, nil
	}
	return nil, errInvalidArgument
}

func (h *handler) read(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	withSpec := false
	if b, ok, _ := dec.Optional(asn1.ContextPrimitive(0)); ok {
		withSpec, _ = asn1.DecodeBool(b)
	}
	// variableAccessSpecification [1] EXPLICIT CHOICE
	vasContent, err := dec.Expect(asn1.ContextConstructed(1))
	if err != nil {
		return nil, errInvalidArgument
	}
	vd := asn1.NewDecoder(vasContent)
	tag, inner, err := vd.ReadTLV()
	if err != nil {
		return nil, errInvalidArgument
	}

	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	specs, err := h.parseVariableAccess(tag, inner)
	if err != nil {
		return nil, err
	}
	results := asn1.Cons(asn1.ContextConstructed(1))
	for _, a := range specs {
		t, dae, ok := h.resolve(a.ref, a.alt)
		if !ok {
			results.Add(mms.DataElement(mms.NewDataAccessError(dae)))
			continue
		}
		results.Add(mms.DataElement(t.get()))
	}
	resp := asn1.Cons(asn1.ContextConstructed(svcRead))
	if withSpec {
		resp.Add(asn1.Cons(asn1.ContextConstructed(0), asn1.RawTLV(vasContent)))
	}
	resp.Add(results)
	return resp, nil
}

func (h *handler) write(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	tag, inner, err := dec.ReadTLV() // variableAccessSpecification CHOICE
	if err != nil {
		return nil, errInvalidArgument
	}
	dataContent, err := dec.Expect(asn1.ContextConstructed(0)) // listOfData [0]
	if err != nil {
		return nil, errInvalidArgument
	}
	var data []*mms.Value
	dd := asn1.NewDecoder(dataContent)
	for dd.More() {
		v, err := mms.DecodeData(dd)
		if err != nil {
			return nil, errInvalidArgument
		}
		data = append(data, v)
	}

	h.s.mu.Lock()
	specs, err := h.parseVariableAccess(tag, inner)
	if err != nil {
		h.s.mu.Unlock()
		return nil, err
	}
	if len(specs) != len(data) {
		h.s.mu.Unlock()
		return nil, errInvalidArgument
	}
	resp := asn1.Cons(asn1.ContextConstructed(svcWrite))
	trigger := false
	for i, a := range specs {
		dae, ok := h.writeOne(a, data[i])
		if !ok {
			resp.Add(asn1.UintElem(asn1.ContextPrimitive(0), uint64(dae))) // failure [0]
			continue
		}
		resp.Add(asn1.Prim(asn1.ContextPrimitive(1), nil)) // success [1]
		if a.ref.Item == reportTriggerName || strings.HasPrefix(a.ref.Item, reportTriggerName+"$") {
			trigger = true
		}
	}
	if trigger {
		h.sendReports()
	}
	h.s.mu.Unlock()
	return resp, nil
}

// writeOne applies one write. Call with s.mu held.
func (h *handler) writeOne(a accessSpec, v *mms.Value) (mms.DataAccessError, bool) {
	t, dae, ok := h.resolve(a.ref, a.alt)
	if !ok {
		return dae, false
	}
	if !t.v.writable {
		return mms.AccessObjectAccessDenied, false
	}
	nv, ok := conform(t.spec, v)
	if !ok {
		return mms.AccessTypeInconsistent, false
	}
	t.set(nv)
	return 0, true
}

func (h *handler) getVariableAccessAttributes(content []byte) (*asn1.Element, error) {
	// GetVariableAccessAttributes-Request ::= CHOICE { name [0] ObjectName, address [1] }
	nc, err := asn1.NewDecoder(content).Expect(asn1.ContextConstructed(0))
	if err != nil {
		return nil, errInvalidArgument
	}
	n, err := parseObjectName(asn1.NewDecoder(nc))
	if err != nil {
		return nil, errInvalidArgument
	}
	h.s.mu.Lock()
	t, _, ok := h.resolve(mms.VarRef{Domain: n.domain, Item: n.item}, nil)
	h.s.mu.Unlock()
	if !ok {
		return nil, mms.AccessObjectNonExistent
	}
	return asn1.Cons(asn1.ContextConstructed(svcGetVarAccess),
		asn1.BoolElem(asn1.ContextPrimitive(0), false), // mmsDeletable
		asn1.Cons(asn1.ContextConstructed(2), t.spec.BER()),
	), nil
}

// ---------------------------------------------------------------------------
// Named variable lists
// ---------------------------------------------------------------------------

// nvlScope returns the list map an ObjectName refers to. Call with s.mu
// held.
func (h *handler) nvlScope(n objectName) (map[string]*nvl, error) {
	switch n.scope {
	case scopeVMD:
		return h.s.vmdNVLs, nil
	case scopeAA:
		return h.aaNVLs, nil
	}
	d := h.s.domains[n.domain]
	if d == nil {
		return nil, mms.AccessObjectNonExistent
	}
	return d.nvls, nil
}

// lookupNVL finds a list; unknown lists are access object-non-existent.
// Call with s.mu held.
func (h *handler) lookupNVL(n objectName) (*nvl, error) {
	m, err := h.nvlScope(n)
	if err != nil {
		return nil, err
	}
	l := m[n.item]
	if l == nil {
		return nil, mms.AccessObjectNonExistent
	}
	return l, nil
}

func (h *handler) defineNVL(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	n, err := parseObjectName(dec)
	if err != nil {
		return nil, errInvalidArgument
	}
	lc, err := dec.Expect(asn1.ContextConstructed(0)) // listOfVariable [0]
	if err != nil {
		return nil, errInvalidArgument
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	specs, err := h.parseVariableAccess(asn1.ContextConstructed(0), lc)
	if err != nil {
		return nil, err
	}
	m, err := h.nvlScope(n)
	if err != nil {
		return nil, err
	}
	if m[n.item] != nil {
		return nil, errObjectExists
	}
	l := &nvl{deletable: true}
	for _, a := range specs {
		// every member must exist
		if _, _, ok := h.resolve(a.ref, a.alt); !ok {
			return nil, errObjectUndefined
		}
		l.members = append(l.members, nvlMember{ref: a.ref, alt: a.alt})
	}
	m[n.item] = l
	return asn1.Prim(asn1.ContextPrimitive(svcDefineNVL), nil), nil
}

func (h *handler) getNVLAttributes(content []byte) (*asn1.Element, error) {
	n, err := parseObjectName(asn1.NewDecoder(content))
	if err != nil {
		return nil, errInvalidArgument
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	l, err := h.lookupNVL(n)
	if err != nil {
		return nil, err
	}
	list := asn1.Cons(asn1.ContextConstructed(1))
	for _, m := range l.members {
		entry := asn1.Cons(asn1.TagSequence, asn1.Cons(asn1.ContextConstructed(0),
			objectName{scope: scopeDomain, domain: m.ref.Domain, item: m.ref.Item}.element()))
		if m.alt != nil {
			entry.Add(asn1.RawContent(asn1.ContextConstructed(5), m.alt))
		}
		list.Add(entry)
	}
	return asn1.Cons(asn1.ContextConstructed(svcGetNVLAttrs),
		asn1.BoolElem(asn1.ContextPrimitive(0), l.deletable), list), nil
}

// deleteNVL implements scopeOfDelete specific only, as libiec61850 does;
// the bulk scopes are refused.
func (h *handler) deleteNVL(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	if b, ok, _ := dec.Optional(asn1.ContextPrimitive(0)); ok {
		if s, _ := asn1.DecodeInt(b); s != 0 {
			return nil, mms.AccessObjectAccessUnsupported
		}
	}
	var matched, deleted uint64
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if lc, ok, _ := dec.Optional(asn1.ContextConstructed(1)); ok {
		ld := asn1.NewDecoder(lc)
		for ld.More() {
			n, err := parseObjectName(ld)
			if err != nil {
				return nil, errInvalidArgument
			}
			m, err := h.nvlScope(n)
			if err != nil || m[n.item] == nil {
				continue
			}
			matched++
			if m[n.item].deletable {
				delete(m, n.item)
				deleted++
			}
		}
	}
	return asn1.Cons(asn1.ContextConstructed(svcDeleteNVL),
		asn1.UintElem(asn1.ContextPrimitive(0), matched),
		asn1.UintElem(asn1.ContextPrimitive(1), deleted)), nil
}

// ---------------------------------------------------------------------------
// InformationReport
// ---------------------------------------------------------------------------

// sendReports sends the fixture's InformationReports to this association,
// in fixture order. The library holds them until the write response has
// been sent. Unlike the C adapter, which can only send the named-list form
// (libiec61850 mis-encodes listOfVariable reports), both forms are sent.
// Call with s.mu held.
func (h *handler) sendReports() {
	for i, r := range h.s.fix.reports {
		var spec *asn1.Element
		if r.vmdList != "" {
			// variableListName [1] ObjectName (vmd-specific)
			spec = asn1.Cons(asn1.ContextConstructed(1), objectName{scope: scopeVMD, item: r.vmdList}.element())
		} else {
			spec = asn1.Cons(asn1.ContextConstructed(0)) // listOfVariable [0]
		}
		values := asn1.Cons(asn1.ContextConstructed(0)) // listOfAccessResult [0]
		for _, ref := range r.variables {
			dom, item, ok := strings.Cut(ref, "/")
			if !ok {
				continue
			}
			t, dae, found := h.resolve(mms.VarRef{Domain: dom, Item: item}, nil)
			if !found {
				values.Add(mms.DataElement(mms.NewDataAccessError(dae)))
			} else {
				values.Add(mms.DataElement(t.get()))
			}
			if r.vmdList == "" {
				spec.Add(asn1.Cons(asn1.TagSequence, asn1.Cons(asn1.ContextConstructed(0),
					objectName{scope: scopeDomain, domain: dom, item: item}.element())))
			}
		}
		// unconfirmed service informationReport [0]
		if err := h.sc.SendUnconfirmed(asn1.Cons(asn1.ContextConstructed(0), spec, values)); err != nil {
			jl.Log("informationReport %d: %v", i, err)
			continue
		}
		if r.vmdList != "" {
			jl.Log("sent informationReport %d (%s)", i, r.vmdList)
		} else {
			jl.Log("sent informationReport %d (listOfVariable)", i)
		}
	}
}
