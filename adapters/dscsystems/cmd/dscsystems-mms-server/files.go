// SPDX-License-Identifier: GPL-3.0-only

// files.go — MMS file services over the file store directory.
//
// The store behaves like libiec61850's: a directory listing is flat, every
// file named by its full '/' path and no entries for directories; a
// fileSpecification naming a directory lists the files below it, one
// naming a file lists that file, anything else is file-non-existent.
// lastModified is always sent (the file system time when the fixture
// gives none). FileRename replaces an existing target, as rename(2) does.
//
// ObtainFile reads the source file from the client (FileOpen, FileRead,
// FileClose sent with ServerConn.Call from a deferred request) and stores
// it under the destination name.
package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dscsystems/go-iec61850/asn1"
	"github.com/dscsystems/go-iec61850/mms"
)

// File class (11) ServiceError codes (ISO 9506-2).
var (
	errFileOther           = &mms.ServiceError{Class: 11, Code: 0}
	errFilePositionInvalid = &mms.ServiceError{Class: 11, Code: 5}
	errFileAccessDenied    = &mms.ServiceError{Class: 11, Code: 6}
	errFileNonExistent     = &mms.ServiceError{Class: 11, Code: 7}
)

// genTimeLayout is the GeneralizedTime form of file times.
const genTimeLayout = "20060102150405.000Z"

// openFile is one file-read state machine (FRSM) of the association.
type openFile struct {
	f    *os.File
	size int64
}

func (h *handler) closeFiles() {
	for id, o := range h.files {
		_ = o.f.Close()
		delete(h.files, id)
	}
}

// storeName turns an MMS file name into a path of the store, refusing
// names that leave it.
func storeName(name string) (string, bool) {
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return ".", true
	}
	if !fs.ValidPath(name) {
		return "", false
	}
	return name, true
}

func fileAttributes(size int64, mod time.Time) *asn1.Element {
	return asn1.Cons(asn1.ContextConstructed(1),
		asn1.UintElem(asn1.ContextPrimitive(0), uint64(size)),
		asn1.Prim(asn1.ContextPrimitive(1), []byte(mod.UTC().Format(genTimeLayout))),
	)
}

func fileNameElem(tag asn1.Tag, name string) *asn1.Element {
	return asn1.Cons(tag, asn1.Prim(asn1.TagGraphicString, []byte(name)))
}

type dirEntry struct {
	name string
	size int64
	mod  time.Time
}

// listFiles lists the files at or below name, flattened, sorted by path.
func (h *handler) listFiles(name string) ([]dirEntry, error) {
	st, err := h.s.store.Stat(name)
	if err != nil {
		return nil, errFileNonExistent
	}
	if !st.IsDir() {
		return []dirEntry{{name, st.Size(), st.ModTime()}}, nil
	}
	var out []dirEntry
	err = fs.WalkDir(h.s.store.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, dirEntry{p, info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, errFileOther
	}
	return out, nil
}

func (h *handler) fileDirectory(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	spec, after := "", ""
	if c, ok, _ := dec.Optional(asn1.ContextConstructed(0)); ok {
		n, err := mms.ParseFileName(c)
		if err != nil {
			return nil, errInvalidArgument
		}
		spec = n
	}
	if c, ok, _ := dec.Optional(asn1.ContextConstructed(1)); ok {
		n, err := mms.ParseFileName(c)
		if err != nil {
			return nil, errInvalidArgument
		}
		after = n
	}
	name, ok := storeName(spec)
	if !ok {
		return nil, errFileNonExistent
	}
	entries, err := h.listFiles(name)
	if err != nil {
		return nil, err
	}
	if after != "" {
		for i, e := range entries {
			if e.name == after {
				entries = entries[i+1:]
				break
			}
		}
	}
	// One page: as many entries as fit the negotiated PDU size.
	budget := h.sc.MaxPDU - 32
	list := asn1.Cons(asn1.TagSequence)
	more, used := false, 0
	for i, e := range entries {
		el := asn1.Cons(asn1.TagSequence,
			fileNameElem(asn1.ContextConstructed(0), e.name),
			fileAttributes(e.size, e.mod))
		if h.sc.MaxPDU > 0 && i > 0 && used+el.Size() > budget {
			more = true
			break
		}
		used += el.Size()
		list.Add(el)
	}
	// listOfDirectoryEntry [0] wraps the SEQUENCE OF explicitly, as
	// libiec61850 sends it.
	resp := asn1.Cons(asn1.ContextConstructed(svcFileDirectory), asn1.Cons(asn1.ContextConstructed(0), list))
	if more {
		resp.Add(asn1.BoolElem(asn1.ContextPrimitive(1), true))
	}
	return resp, nil
}

func (h *handler) fileOpen(content []byte) (*asn1.Element, error) {
	dec := asn1.NewDecoder(content)
	nc, err := dec.Expect(asn1.ContextConstructed(0))
	if err != nil {
		return nil, errInvalidArgument
	}
	fname, err := mms.ParseFileName(nc)
	if err != nil {
		return nil, errInvalidArgument
	}
	var pos uint64
	if b, ok, _ := dec.Optional(asn1.ContextPrimitive(1)); ok {
		pos, _ = asn1.DecodeUint(b)
	}
	name, ok := storeName(fname)
	if !ok {
		return nil, errFileNonExistent
	}
	f, err := h.s.store.Open(name)
	if err != nil {
		return nil, errFileNonExistent
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		return nil, errFileNonExistent
	}
	if int64(pos) > st.Size() {
		_ = f.Close()
		return nil, errFilePositionInvalid
	}
	if _, err := f.Seek(int64(pos), io.SeekStart); err != nil {
		_ = f.Close()
		return nil, errFilePositionInvalid
	}
	id := h.nextFRSM
	h.nextFRSM++
	h.files[id] = &openFile{f: f, size: st.Size()}
	return asn1.Cons(asn1.ContextConstructed(svcFileOpen),
		asn1.IntElem(asn1.ContextPrimitive(0), int64(id)),
		fileAttributes(st.Size(), st.ModTime()),
	), nil
}

func (h *handler) frsm(content []byte) (int32, *openFile, error) {
	n, err := asn1.DecodeInt(content)
	if err != nil {
		return 0, nil, errInvalidArgument
	}
	o := h.files[int32(n)]
	if o == nil {
		return 0, nil, errFileOther
	}
	return int32(n), o, nil
}

func (h *handler) fileRead(content []byte) (*asn1.Element, error) {
	_, o, err := h.frsm(content)
	if err != nil {
		return nil, err
	}
	// leave room in the PDU for the response envelope
	chunk := h.sc.MaxPDU - 32
	if chunk <= 0 || chunk > 65000 {
		chunk = 65000
	}
	buf := make([]byte, chunk)
	n, err := io.ReadFull(o.f, buf)
	more := true
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		more = false
	} else if err != nil {
		return nil, errFileOther
	} else if cur, serr := o.f.Seek(0, io.SeekCurrent); serr == nil && cur >= o.size {
		more = false
	}
	return asn1.Cons(asn1.ContextConstructed(svcFileRead),
		asn1.Prim(asn1.ContextPrimitive(0), buf[:n]),
		asn1.BoolElem(asn1.ContextPrimitive(1), more),
	), nil
}

func (h *handler) fileClose(content []byte) (*asn1.Element, error) {
	id, o, err := h.frsm(content)
	if err != nil {
		return nil, err
	}
	_ = o.f.Close()
	delete(h.files, id)
	return asn1.Prim(asn1.ContextPrimitive(svcFileClose), nil), nil
}

func (h *handler) fileDelete(content []byte) (*asn1.Element, error) {
	fname, err := mms.ParseFileName(content)
	if err != nil {
		return nil, errInvalidArgument
	}
	name, ok := storeName(fname)
	if !ok || name == "." {
		return nil, errFileNonExistent
	}
	if err := h.s.store.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errFileNonExistent
		}
		return nil, errFileAccessDenied
	}
	return asn1.Prim(asn1.ContextPrimitive(svcFileDelete), nil), nil
}

func (h *handler) fileRename(content []byte) (*asn1.Element, error) {
	cur, renamed, err := mms.ParseFileRename(content)
	if err != nil {
		return nil, errInvalidArgument
	}
	from, ok1 := storeName(cur)
	to, ok2 := storeName(renamed)
	if !ok1 || !ok2 || from == "." || to == "." {
		return nil, errFileNonExistent
	}
	if err := h.s.store.Rename(from, to); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errFileNonExistent
		}
		return nil, errFileAccessDenied
	}
	return asn1.Prim(asn1.ContextPrimitive(svcFileRename), nil), nil
}

// obtainFile answers ObtainFile-Request ::= SEQUENCE { sourceFileServer [0]
// OPTIONAL, sourceFile [1] FileName, destinationFile [2] FileName } by
// reading sourceFile from the client.
func (h *handler) obtainFile(req *mms.Request) (*asn1.Element, error) {
	dec := asn1.NewDecoder(req.Content)
	_, _, _ = dec.Optional(asn1.ContextConstructed(0))
	sc, err := dec.Expect(asn1.ContextConstructed(1))
	if err != nil {
		return nil, errInvalidArgument
	}
	dc, err := dec.Expect(asn1.ContextConstructed(2))
	if err != nil {
		return nil, errInvalidArgument
	}
	src, err1 := mms.ParseFileName(sc)
	dst, err2 := mms.ParseFileName(dc)
	if err1 != nil || err2 != nil {
		return nil, errInvalidArgument
	}
	dstName, ok := storeName(dst)
	if !ok || dstName == "." {
		return nil, errFileAccessDenied
	}
	conn := req.Conn
	return req.Defer(func(ctx context.Context) (*asn1.Element, error) {
		data, err := fetchFromClient(ctx, conn, src)
		if err != nil {
			return nil, err
		}
		if dir := path.Dir(dstName); dir != "." {
			if err := h.s.store.MkdirAll(dir, 0o755); err != nil {
				return nil, errFileAccessDenied
			}
		}
		if err := h.s.store.WriteFile(dstName, data, 0o644); err != nil {
			return nil, errFileAccessDenied
		}
		return asn1.Prim(asn1.ContextPrimitive(svcObtainFile), nil), nil
	})
}

// clientRequestTimeout bounds each request sent to the client during
// ObtainFile; a client that does not serve files never answers.
const clientRequestTimeout = 5 * time.Second

// fetchFromClient reads a whole file from the client with FileOpen,
// FileRead and FileClose. A file-class error of the client is passed on;
// anything else (a reject or no answer from a client without a file
// store, a lost connection) becomes file other.
func fetchFromClient(ctx context.Context, conn *mms.ServerConn, name string) ([]byte, error) {
	call := func(service *asn1.Element) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, clientRequestTimeout)
		defer cancel()
		return conn.Call(cctx, service)
	}
	passOn := func(err error) error {
		var se *mms.ServiceError
		if errors.As(err, &se) && !se.Rejected && se.Class == 11 {
			return se
		}
		return errFileOther
	}
	resp, err := call(asn1.Cons(asn1.ContextConstructed(svcFileOpen),
		fileNameElem(asn1.ContextConstructed(0), name),
		asn1.UintElem(asn1.ContextPrimitive(1), 0)))
	if err != nil {
		return nil, passOn(err)
	}
	oc, err := asn1.NewDecoder(resp).Expect(asn1.ContextConstructed(svcFileOpen))
	if err != nil {
		return nil, errFileOther
	}
	idb, err := asn1.NewDecoder(oc).Expect(asn1.ContextPrimitive(0))
	if err != nil {
		return nil, errFileOther
	}
	id, _ := asn1.DecodeInt(idb)
	defer func() { _, _ = call(asn1.IntElem(asn1.ContextPrimitive(svcFileClose), id)) }()

	var data []byte
	for {
		resp, err := call(asn1.IntElem(asn1.ContextPrimitive(svcFileRead), id))
		if err != nil {
			return nil, passOn(err)
		}
		rc, err := asn1.NewDecoder(resp).Expect(asn1.ContextConstructed(svcFileRead))
		if err != nil {
			return nil, errFileOther
		}
		rd := asn1.NewDecoder(rc)
		chunk, err := rd.Expect(asn1.ContextPrimitive(0))
		if err != nil {
			return nil, errFileOther
		}
		data = append(data, chunk...)
		more := true // moreFollows DEFAULT TRUE
		if b, ok, _ := rd.Optional(asn1.ContextPrimitive(1)); ok {
			more, _ = asn1.DecodeBool(b)
		}
		if !more {
			return data, nil
		}
		if len(chunk) == 0 {
			return nil, errFileOther
		}
	}
}
