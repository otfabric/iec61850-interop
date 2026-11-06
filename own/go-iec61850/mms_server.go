// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	mms "github.com/otfabric/go-mms"
	"github.com/otfabric/go-mms/transport/iso"
)

const mmsFixture = "mms-v2"

// mmsReportTriggerAbort written to interop/report-trigger makes the server
// abort the writing association (ServerConn.Abort) instead of sending the
// reports; the mms-client's server-abort step uses it.
const mmsReportTriggerAbort = -1

// mmsFixtureFile is fixtures/mms/interop.json (revision mms-v2).
type mmsFixtureFile struct {
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
	Domains            map[string]mmsFixtureDomain `json:"domains"`
	InformationReports []mmsFixtureReport          `json:"informationReports"`
	Files              map[string]mmsFixtureStored `json:"files"`
}

type mmsFixtureDomain struct {
	Variables map[string]mmsFixtureVar     `json:"variables"`
	Journals  map[string]mmsFixtureJournal `json:"journals"`
}

type mmsFixtureVar struct {
	Type        string `json:"type"`
	Size        int    `json:"size"`
	Value       any    `json:"value"`
	Writable    bool   `json:"writable"`
	ElementType string `json:"elementType"`
	Count       int    `json:"count"`
}

type mmsFixtureJournal struct {
	Entries []mmsFixtureJournalEntry `json:"entries"`
}

type mmsFixtureJournalEntry struct {
	EntryID        uint64 `json:"entryId"`
	OccurrenceTime string `json:"occurrenceTime"`
	Tag            string `json:"tag"`
	Type           string `json:"type"`
	Value          any    `json:"value"`
	ReasonCode     int    `json:"reasonCode"`
}

type mmsFixtureReport struct {
	VMDList   string   `json:"vmdList"`
	Variables []string `json:"variables"`
}

type mmsFixtureStored struct {
	Content      *string `json:"content"`
	Hex          *string `json:"hex"`
	Size         *int    `json:"size"`
	LastModified string  `json:"lastModified"`
}

// bytes is the content of a fixture file; a generated file (size only)
// holds byte i = i % 251.
func (f mmsFixtureStored) bytes() ([]byte, error) {
	switch {
	case f.Content != nil:
		return []byte(*f.Content), nil
	case f.Hex != nil:
		return hex.DecodeString(*f.Hex)
	case f.Size != nil:
		if *f.Size < 0 {
			return nil, fmt.Errorf("negative size %d", *f.Size)
		}
		b := make([]byte, *f.Size)
		for i := range b {
			b[i] = byte(i % 251)
		}
		return b, nil
	}
	return nil, errors.New("file has no content, hex or size")
}

func loadMMSFixture(path string) (*mmsFixtureFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fix mmsFixtureFile
	if err := json.Unmarshal(data, &fix); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if fix.Revision != mmsFixture {
		return nil, fmt.Errorf("%s is revision %q, want %q", path, fix.Revision, mmsFixture)
	}
	return &fix, nil
}

// runMMSServer is otfabric-mms-server: the generic MMS server of fixture
// mms-v2 on go-mms's server, with the file store, the journals and the
// InformationReports of the fixture.
func runMMSServer(args []string) int {
	fs := flag.NewFlagSet("otfabric-mms-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fixture := fs.String("fixture", "/fixtures/mms/interop.json", "fixture file")
	port := fs.Int("port", 1102, "TCP port")
	// Accepted as the other stacks do; the file store is in memory.
	fs.String("filestore", "", "ignored: the file store is the fixture's, in memory")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if fs.NArg() > 0 {
		return argError("mms-server: unexpected argument %q", fs.Arg(0))
	}
	if *port <= 0 || *port > 65535 {
		return argError("mms-server: invalid port %d", *port)
	}
	fix, err := loadMMSFixture(*fixture)
	if err != nil {
		return argError("mms-server: %v", err)
	}
	srv, err := newMMSServer(fix, time.Now().UTC().Truncate(time.Second))
	if err != nil {
		return argError("mms-server: %v", err)
	}
	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	ln, err := iso.Listen(addr)
	if err != nil {
		return argError("mms-server: listen %s: %v", addr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(context.Background(), ln) }()
	ready(addr, mmsFixture)
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			logf("mms-server: shutdown: %v", err)
		}
		<-served
		return exitOK
	case err := <-served:
		if err != nil && !errors.Is(err, mms.ErrServerClosed) {
			logf("mms-server: %v", err)
			return exitArgs
		}
		return exitOK
	}
}

// newMMSServer builds the server of a fixture. start is the modification
// time of a file the fixture gives none.
func newMMSServer(fix *mmsFixtureFile, start time.Time) (*mms.Server, error) {
	files, err := newMMSFileStore(fix, start)
	if err != nil {
		return nil, err
	}
	journals, err := newMMSJournals(fix)
	if err != nil {
		return nil, err
	}
	srv := mms.NewServer(mms.ServerOptions{FileProvider: files, JournalProvider: journals})
	srv.HandleIdentify(func(context.Context, mms.IdentifyRequest) (*mms.ServerIdentity, error) {
		return &mms.ServerIdentity{Vendor: fix.Identity.Vendor, Model: fix.Identity.Model, Revision: fix.Identity.Revision}, nil
	})
	status := mms.ServerStatus{Logical: mms.VMDLogicalStatus(fix.Status.Logical), Physical: mms.VMDPhysicalStatus(fix.Status.Physical)}
	srv.HandleStatus(func(context.Context, mms.StatusRequest) (*mms.ServerStatus, error) {
		s := status
		return &s, nil
	})

	vars := map[string]mms.Variable{}
	read := func(name string) (*mms.Value, error) {
		v, ok := vars[name]
		if !ok {
			return nil, fmt.Errorf("unknown variable %q", name)
		}
		return v.Read(context.Background())
	}
	for _, domain := range mmsSortedKeys(fix.Domains) {
		if err := srv.RegisterDomain(domain); err != nil {
			return nil, fmt.Errorf("domain %s: %w", domain, err)
		}
		d := fix.Domains[domain]
		for _, item := range mmsSortedKeys(d.Variables) {
			v, err := buildMMSVariable(mms.DomainID(domain), mms.ItemID(item), d.Variables[item])
			if err != nil {
				return nil, fmt.Errorf("variable %s/%s: %w", domain, item, err)
			}
			if item == "report-trigger" {
				store := v.Write
				v.Write = func(ctx context.Context, val *mms.Value) error {
					if err := store(ctx, val); err != nil {
						return err
					}
					return mmsOnReportTrigger(ctx, fix, status, val, read)
				}
			}
			vars[domain+"/"+item] = v
			if err := srv.RegisterVariable(v); err != nil {
				return nil, fmt.Errorf("variable %s/%s: %w", domain, item, err)
			}
		}
	}
	// The VMD list of the named-list report is not registered: the other
	// stacks' servers do not define it either, so a client's VMD list names
	// are those it defined itself.
	return srv, nil
}

// onReportTrigger: a write of -1 aborts the writing association; any other
// value sends every fixture InformationReport to the writing association,
// in fixture order (listOfVariable, or the VMD list name), then an
// UnsolicitedStatus, all before the write response.
func mmsOnReportTrigger(ctx context.Context, fix *mmsFixtureFile, status mms.ServerStatus, val *mms.Value, read func(string) (*mms.Value, error)) error {
	sc := mms.ServerConnFromContext(ctx)
	if sc == nil {
		return errors.New("no server connection in the write context")
	}
	if n, _ := val.Int64(); n == mmsReportTriggerAbort {
		return sc.Abort(ctx)
	}
	for _, r := range fix.InformationReports {
		req := &mms.InformationReportRequest{}
		for _, mmsRef := range r.Variables {
			v, err := read(mmsRef)
			if err != nil {
				return err
			}
			req.Values = append(req.Values, v)
			if r.VMDList == "" {
				dom, item, _ := strings.Cut(mmsRef, "/")
				req.Variables = append(req.Variables, mms.ObjectName{Scope: mms.ObjectScopeDomain, Domain: mms.DomainID(dom), ItemID: mms.ItemID(item)})
			}
		}
		if r.VMDList != "" {
			req.ListName = &mms.ObjectName{Scope: mms.ObjectScopeVMD, ItemID: mms.ItemID(r.VMDList)}
		}
		if err := sc.SendInformationReport(ctx, req); err != nil {
			return err
		}
	}
	return sc.SendUnsolicitedStatus(ctx, status)
}

func mmsSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// buildMMSVariable is a fixture variable with its type and a value that
// writes of the same type replace (when writable).
func buildMMSVariable(domain mms.DomainID, item mms.ItemID, v mmsFixtureVar) (mms.Variable, error) {
	ts, initial, err := mmsTypeAndValue(v)
	if err != nil {
		return mms.Variable{}, err
	}
	var mu sync.Mutex
	current := initial
	out := mms.Variable{
		Name:     mms.ObjectName{Scope: mms.ObjectScopeDomain, Domain: domain, ItemID: item},
		TypeSpec: ts,
		Read: func(context.Context) (*mms.Value, error) {
			mu.Lock()
			defer mu.Unlock()
			return current, nil
		},
	}
	if v.Writable {
		out.Write = func(_ context.Context, val *mms.Value) error {
			// A value of another type is type-inconsistent, as the other
			// stacks' servers answer it (a plain error would be
			// temporarily-unavailable).
			if val == nil || val.Type() != ts.Type || !ts.ShallowCompatible(val) {
				return &mms.DataAccessError{Code: mms.DataAccessErrorTypeInconsistent}
			}
			mu.Lock()
			defer mu.Unlock()
			current = val
			return nil
		}
	}
	return out, nil
}

func mmsTypeAndValue(v mmsFixtureVar) (mms.TypeSpec, *mms.Value, error) {
	switch v.Type {
	case "boolean", "integer", "unsigned", "float32":
		ts, err := mmsScalarType(v.Type, v.Size)
		if err != nil {
			return mms.TypeSpec{}, nil, err
		}
		val, err := mmsScalarValue(v.Type, v.Value)
		return ts, val, err
	case "visible-string":
		s, ok := v.Value.(string)
		if !ok {
			return mms.TypeSpec{}, nil, fmt.Errorf("visible-string: value is %T", v.Value)
		}
		return mms.TypeSpec{Type: mms.ValueTypeVisibleString, Size: v.Size}, mms.NewVisibleString(s), nil
	case "octet-string":
		s, ok := v.Value.(string)
		if !ok {
			return mms.TypeSpec{}, nil, fmt.Errorf("octet-string: value is %T", v.Value)
		}
		b, err := hex.DecodeString(s)
		if err != nil {
			return mms.TypeSpec{}, nil, fmt.Errorf("octet-string: %w", err)
		}
		return mms.TypeSpec{Type: mms.ValueTypeOctetString, Size: len(b)}, mms.NewOctetString(b), nil
	case "bit-string":
		s, ok := v.Value.(string)
		if !ok {
			return mms.TypeSpec{}, nil, fmt.Errorf("bit-string: value is %T", v.Value)
		}
		bits := make([]byte, (len(s)+7)/8)
		for i, c := range s {
			switch c {
			case '1':
				bits[i/8] |= 0x80 >> (i % 8)
			case '0':
			default:
				return mms.TypeSpec{}, nil, fmt.Errorf("bit-string: %q is not binary digits", s)
			}
		}
		return mms.TypeSpec{Type: mms.ValueTypeBitString, Size: len(s)}, mms.NewBitStringWithLength(bits, len(s)), nil
	case "utc-time":
		s, ok := v.Value.(string)
		if !ok {
			return mms.TypeSpec{}, nil, fmt.Errorf("utc-time: value is %T", v.Value)
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return mms.TypeSpec{}, nil, fmt.Errorf("utc-time: %w", err)
		}
		return mms.TypeSpec{Type: mms.ValueTypeUTCTime}, mms.NewUTCTime(t.UTC()), nil
	case "array":
		elem, err := mmsScalarType(v.ElementType, 0)
		if err != nil {
			return mms.TypeSpec{}, nil, fmt.Errorf("array: %w", err)
		}
		raw, ok := v.Value.([]any)
		if !ok {
			return mms.TypeSpec{}, nil, fmt.Errorf("array: value is %T", v.Value)
		}
		count := len(raw)
		if v.Count > 0 {
			count = v.Count
		}
		if count != len(raw) {
			return mms.TypeSpec{}, nil, fmt.Errorf("array: count %d, %d values", count, len(raw))
		}
		elems := make([]*mms.Value, 0, len(raw))
		for i, x := range raw {
			e, err := mmsScalarValue(v.ElementType, x)
			if err != nil {
				return mms.TypeSpec{}, nil, fmt.Errorf("array[%d]: %w", i, err)
			}
			elems = append(elems, e)
		}
		return mms.TypeSpec{Type: mms.ValueTypeArray, Count: count, Element: &elem}, mms.NewArray(elems), nil
	case "structure":
		raw, ok := v.Value.([]any)
		if !ok {
			return mms.TypeSpec{}, nil, fmt.Errorf("structure: value is %T", v.Value)
		}
		var specs []mms.TypeSpecElement
		var vals []*mms.Value
		for i, x := range raw {
			comp, ok := x.(map[string]any)
			if !ok {
				return mms.TypeSpec{}, nil, fmt.Errorf("structure[%d]: %T", i, x)
			}
			ct, _ := comp["type"].(string)
			name, _ := comp["name"].(string)
			if name == "" {
				name = fmt.Sprintf("comp%d", i)
			}
			cts, err := mmsScalarType(ct, 0)
			if err != nil {
				return mms.TypeSpec{}, nil, fmt.Errorf("structure[%d]: %w", i, err)
			}
			cv, err := mmsScalarValue(ct, comp["value"])
			if err != nil {
				return mms.TypeSpec{}, nil, fmt.Errorf("structure[%d]: %w", i, err)
			}
			specs = append(specs, mms.TypeSpecElement{Name: name, Type: cts})
			vals = append(vals, cv)
		}
		return mms.TypeSpec{Type: mms.ValueTypeStructure, Elements: specs}, mms.NewStructure(vals), nil
	}
	return mms.TypeSpec{}, nil, fmt.Errorf("unsupported variable type %q", v.Type)
}

func mmsScalarType(name string, size int) (mms.TypeSpec, error) {
	if size == 0 {
		size = 32
	}
	switch name {
	case "boolean":
		return mms.TypeSpec{Type: mms.ValueTypeBoolean}, nil
	case "integer":
		return mms.TypeSpec{Type: mms.ValueTypeInteger, Size: size}, nil
	case "unsigned":
		return mms.TypeSpec{Type: mms.ValueTypeUnsigned, Size: size}, nil
	case "float32":
		return mms.TypeSpec{Type: mms.ValueTypeFloat, FormatWidth: 32, ExponentWidth: 8}, nil
	}
	return mms.TypeSpec{}, fmt.Errorf("unsupported scalar type %q", name)
}

func mmsScalarValue(name string, raw any) (*mms.Value, error) {
	if name == "boolean" {
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("boolean: value is %T", raw)
		}
		return mms.NewBoolean(b), nil
	}
	n, ok := raw.(float64)
	if !ok {
		return nil, fmt.Errorf("%s: value is %T", name, raw)
	}
	switch name {
	case "integer":
		return mms.NewInteger(int64(n)), nil
	case "unsigned":
		if n < 0 {
			return nil, fmt.Errorf("unsigned: %v", n)
		}
		return mms.NewUnsigned(uint64(n)), nil
	case "float32":
		return mms.NewFloat(n), nil
	}
	return nil, fmt.Errorf("unsupported scalar type %q", name)
}

// ---------------------------------------------------------------------------
// File store
// ---------------------------------------------------------------------------

type mmsMemFile struct {
	data    []byte
	modTime time.Time
}

type mmsMemHandle struct {
	data []byte
	pos  int
}

// mmsFileStore is an in-memory mms.FileProvider over the fixture files. A
// listing is flat, as the other stacks' servers list their stores: every
// file by its full '/' path and no directory entries; a FileSpec names a
// file or a directory (the files below it).
type mmsFileStore struct {
	mu    sync.Mutex
	files map[string]*mmsMemFile
}

func newMMSFileStore(fix *mmsFixtureFile, start time.Time) (*mmsFileStore, error) {
	st := &mmsFileStore{files: map[string]*mmsMemFile{}}
	for name, f := range fix.Files {
		data, err := f.bytes()
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", name, err)
		}
		mod := start
		if f.LastModified != "" {
			if mod, err = time.Parse(time.RFC3339, f.LastModified); err != nil {
				return nil, fmt.Errorf("file %s: %w", name, err)
			}
		}
		st.files[strings.TrimPrefix(name, "/")] = &mmsMemFile{data: data, modTime: mod.UTC()}
	}
	return st, nil
}

func (s *mmsFileStore) List(_ context.Context, req mms.FileListRequest) (*mms.FileListResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec := strings.TrimPrefix(req.FileSpec, "/")
	var entries []mms.FileEntry
	add := func(name string, f *mmsMemFile) {
		entries = append(entries, mms.FileEntry{Name: name, Size: int64(len(f.data)), LastModified: f.modTime})
	}
	switch f, isFile := s.files[spec]; {
	case spec == "":
		for name, f := range s.files {
			add(name, f)
		}
	case isFile:
		add(spec, f)
	default:
		dir := strings.TrimRight(spec, `/\`) + "/"
		for name, f := range s.files {
			if strings.HasPrefix(name, dir) {
				add(name, f)
			}
		}
	}
	if spec != "" && len(entries) == 0 {
		return nil, fs.ErrNotExist
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name < entries[b].Name })
	res := &mms.FileListResult{}
	for _, e := range entries {
		if req.ContinueAfter == "" || e.Name > req.ContinueAfter {
			res.Entries = append(res.Entries, e)
		}
	}
	return res, nil
}

func (s *mmsFileStore) Open(_ context.Context, path string) (mms.FileHandle, mms.FileAttributes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[strings.TrimPrefix(path, "/")]
	if !ok {
		return nil, mms.FileAttributes{}, fs.ErrNotExist
	}
	return &mmsMemHandle{data: f.data}, mms.FileAttributes{Size: int64(len(f.data)), LastModified: f.modTime}, nil
}

func (s *mmsFileStore) Read(_ context.Context, handle mms.FileHandle, maxBytes int) ([]byte, bool, error) {
	h, ok := handle.(*mmsMemHandle)
	if !ok {
		return nil, false, errors.New("not a handle of this store")
	}
	if maxBytes <= 0 {
		return nil, false, fmt.Errorf("read of %d bytes", maxBytes)
	}
	end := min(h.pos+maxBytes, len(h.data))
	chunk := h.data[h.pos:end]
	h.pos = end
	return chunk, h.pos < len(h.data), nil
}

func (s *mmsFileStore) Close(context.Context, mms.FileHandle) error { return nil }

func (s *mmsFileStore) Delete(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path = strings.TrimPrefix(path, "/")
	if _, ok := s.files[path]; !ok {
		return fs.ErrNotExist
	}
	delete(s.files, path)
	return nil
}

func (s *mmsFileStore) Rename(_ context.Context, current, renamed string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, renamed = strings.TrimPrefix(current, "/"), strings.TrimPrefix(renamed, "/")
	f, ok := s.files[current]
	if !ok {
		return fs.ErrNotExist
	}
	if _, exists := s.files[renamed]; exists {
		return mms.ErrFileAccessDenied
	}
	delete(s.files, current)
	s.files[renamed] = f
	return nil
}

// ObtainFile copies a file of the store: go-mms's FileProvider defines
// ObtainFile as a copy on the server, it has no call that fetches the
// source from the client.
func (s *mmsFileStore) ObtainFile(_ context.Context, source, destination string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[strings.TrimPrefix(source, "/")]
	if !ok {
		return fs.ErrNotExist
	}
	s.files[strings.TrimPrefix(destination, "/")] = &mmsMemFile{data: append([]byte(nil), f.data...), modTime: time.Now().UTC().Truncate(time.Second)}
	return nil
}

func (s *mmsFileStore) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return mmsSortedKeys(s.files)
}

// ---------------------------------------------------------------------------
// Journals
// ---------------------------------------------------------------------------

// mmsJournals is an mms.JournalProvider over the fixture journals. Each
// entry carries its data variable and the 7-bit ReasonCode bit string, as
// libiec61850 servers send it; entry IDs are 8 little-endian bytes.
type mmsJournals struct {
	journals map[string]map[string][]mms.JournalEntry // domain -> journal -> entries
}

func newMMSJournals(fix *mmsFixtureFile) (*mmsJournals, error) {
	j := &mmsJournals{journals: map[string]map[string][]mms.JournalEntry{}}
	for domain, d := range fix.Domains {
		for name, journal := range d.Journals {
			var entries []mms.JournalEntry
			for _, e := range journal.Entries {
				tm, err := time.Parse(time.RFC3339Nano, e.OccurrenceTime)
				if err != nil {
					return nil, fmt.Errorf("journal %s/%s entry %d: %w", domain, name, e.EntryID, err)
				}
				val, err := mmsScalarValue(e.Type, e.Value)
				if err != nil {
					return nil, fmt.Errorf("journal %s/%s entry %d: %w", domain, name, e.EntryID, err)
				}
				id := make([]byte, 8)
				binary.LittleEndian.PutUint64(id, e.EntryID)
				entries = append(entries, mms.JournalEntry{
					EntryID:        id,
					OccurrenceTime: tm,
					Variables: []mms.JournalVariable{
						{Tag: e.Tag, Value: val},
						{Tag: "ReasonCode", Value: mmsReasonCode(e.ReasonCode)},
					},
				})
			}
			sort.SliceStable(entries, func(a, b int) bool { return entries[a].OccurrenceTime.Before(entries[b].OccurrenceTime) })
			if j.journals[domain] == nil {
				j.journals[domain] = map[string][]mms.JournalEntry{}
			}
			j.journals[domain][name] = entries
		}
	}
	return j, nil
}

// mmsReasonCode is a reason code as libiec61850 encodes it: a 7-bit bit
// string whose bit i (MSB first) carries 1<<i.
func mmsReasonCode(code int) *mms.Value {
	var b byte
	for i := range 7 {
		if code&(1<<i) != 0 {
			b |= 0x80 >> i
		}
	}
	return mms.NewBitStringWithLength([]byte{b}, 7)
}

func (j *mmsJournals) entries(domain, journal string) ([]mms.JournalEntry, error) {
	e, ok := j.journals[domain][journal]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return e, nil
}

func (j *mmsJournals) ListJournals(_ context.Context, domain string) ([]string, error) {
	return mmsSortedKeys(j.journals[domain]), nil
}

func (j *mmsJournals) ReadTimeRange(_ context.Context, domain, journal string, start, stop time.Time, maxEntries int) (*mms.JournalResult, error) {
	all, err := j.entries(domain, journal)
	if err != nil {
		return nil, err
	}
	res := &mms.JournalResult{}
	for _, e := range all {
		if e.OccurrenceTime.Before(start) || e.OccurrenceTime.After(stop) {
			continue
		}
		if maxEntries > 0 && len(res.Entries) == maxEntries {
			res.MoreFollows = true
			break
		}
		res.Entries = append(res.Entries, e)
	}
	return res, nil
}

func (j *mmsJournals) ReadStartAfter(_ context.Context, domain, journal string, afterID []byte, afterTime time.Time, maxEntries int) (*mms.JournalResult, error) {
	all, err := j.entries(domain, journal)
	if err != nil {
		return nil, err
	}
	res := &mms.JournalResult{}
	found := false
	for _, e := range all {
		if !found {
			found = e.OccurrenceTime.Equal(afterTime) && string(e.EntryID) == string(afterID)
			continue
		}
		if maxEntries > 0 && len(res.Entries) == maxEntries {
			res.MoreFollows = true
			break
		}
		res.Entries = append(res.Entries, e)
	}
	if !found {
		return nil, fs.ErrNotExist
	}
	return res, nil
}
