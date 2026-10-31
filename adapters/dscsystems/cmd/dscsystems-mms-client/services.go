// SPDX-License-Identifier: GPL-3.0-only

// services.go — fixture mms-v2 service sequence of the MMS client adapter.
//
// Port of adapters/libiec61850/src/mms_client_services.c. Every operation
// emits one JSON line:
//
//	{"operation":"<op>","target":"<target>","ok":true,"value":<json>}
//	{"operation":"<op>","target":"<target>","ok":false,"error":"<reason>"}
//
// The operation/target pairs are part of the adapter output contract and
// are asserted by consumer test suites.
//
// Where the dscsystems mms.Conn has no call for a step, the step emits
// "error":"unsupported":
//   - alternate access reads and writes (array element, index range,
//     structure component): Read and Write take whole variables only;
//   - association-specific named variable lists: object names are
//     domain- or VMD-specific only;
//   - writing a named variable list: Write takes a list of variables only.
//
// Library behaviour that changes a line without failing it:
//   - FileOpen does not return the file's lastModified, so file-read
//     reports "lastModified":0;
//   - ReadJournal* and FileDirectory follow continuation themselves, so
//     read-journal always reports "moreFollows":false.
package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sync/atomic"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
)

// ---------------------------------------------------------------------------
// Emit helpers
// ---------------------------------------------------------------------------

// emitRead is emit_read: service errors and per-variable data access
// errors fail the line, otherwise the value is emitted.
func emitRead(op, target string, v *mms.Value, err error) {
	switch {
	case err != nil:
		emitFail(op, target, errName(err))
	case v == nil:
		emitFail(op, target, "none")
	default:
		if dae, ok := v.AccessError(); ok {
			emitFail(op, target, daeName(dae))
			return
		}
		emitValue(op, target, v)
	}
}

// emitList renders a list of values as one JSON array value.
func emitList(op, target string, vals []*mms.Value, err error) {
	if err != nil {
		emitFail(op, target, errName(err))
		return
	}
	emitValue(op, target, mms.NewArray(vals...))
}

func emitResult(op, target string, err error) {
	if err != nil {
		emitFail(op, target, errName(err))
	} else {
		emitOK(op, target)
	}
}

// ---------------------------------------------------------------------------
// InformationReport reception
// ---------------------------------------------------------------------------

var reportCount atomic.Int64

// installReportHandler emits every InformationReport. Named-list reports
// are one line with all values; listOfVariable reports one line per
// variable (target "var:<domain>/<item>"), as libiec61850 delivers them.
// Install before the first request so that no report is lost.
func installReportHandler(conn *mms.Conn) {
	conn.OnInformationReport(func(r *mms.InformationReport) {
		if r.IsVMDNamed {
			emitValue("information-report", "list:"+refString(r.ListRef), mms.NewArray(r.Values...))
		} else {
			for i, v := range r.Values {
				var ref mms.VarRef
				if i < len(r.VarRefs) {
					ref = r.VarRefs[i]
				}
				emitValue("information-report", "var:"+refString(ref), v)
			}
		}
		reportCount.Add(1)
	})
}

// refString is "<domain>/<item>", or "<item>" without a domain.
func refString(r mms.VarRef) string {
	if r.Domain == "" {
		return r.Item
	}
	return r.Domain + "/" + r.Item
}

// ---------------------------------------------------------------------------
// Service groups
// ---------------------------------------------------------------------------

var memberRefs = []mms.VarRef{{Domain: "interop", Item: "boolean"}, {Domain: "interop", Item: "integer"}}

func svcStatus(c *mms.Conn) {
	for i, target := range []string{"", "extended"} {
		ctx, cancel := reqCtx()
		st, err := c.Status(ctx, i == 1)
		cancel()
		if err != nil {
			emitFail("status", target, errName(err))
			continue
		}
		emitValueRaw("status", target, line{{"logical", st.Logical}, {"physical", st.Physical}})
	}
}

// emitValueRaw emits an ok line whose value is an ordered object.
func emitValueRaw(op, target string, value line) {
	b, _ := value.MarshalJSON()
	emitValueJSON(op, target, raw(b))
}

func emitValueJSON(op, target string, value raw) {
	emitLine(append(begin(op, target, true), field{"value", value}))
}

func svcNameLists(c *mms.Conn) {
	nameList := func(target string, class mms.ObjectClass, domain string) {
		ctx, cancel := reqCtx()
		names, err := c.GetNameList(ctx, class, domain)
		cancel()
		if err != nil {
			emitFail("get-name-list", target, errName(err))
		} else {
			emitNames(target, names)
		}
	}
	nameList("interop/journals", mms.ClassJournal, "interop")
	nameList("vmd/variables", mms.ClassNamedVariable, "")
	// expected to fail: the server must reject an unknown domain
	nameList("unknown-domain/variables", mms.ClassNamedVariable, "unknown-domain")
}

func svcTypeSpecs(c *mms.Conn) {
	for _, item := range []string{"array-rw", "structure-rw"} {
		target := "interop/" + item
		ctx, cancel := reqCtx()
		ts, err := c.GetVariableAccessAttributes(ctx, "interop", item)
		cancel()
		if err != nil {
			emitFail("get-var-access-attr", target, errName(err))
			continue
		}
		emitLine(append(begin("get-var-access-attr", target, true),
			field{"type", specTypeName(ts.Kind)}, field{"value", specJSON(ts)}))
	}
}

// svcAlternateAccess: the element, range and component accesses have no
// call in mms.Conn; the whole-variable reads that close the group do.
func svcAlternateAccess(cl *client) {
	emitUnsupported("read-array-elements", "interop/array[1:4]")
	emitUnsupported("read-array-elements", "interop/array[2]")
	emitUnsupported("read-component", "interop/structure-rw$count")
	emitUnsupported("write-array-elements", "interop/array-rw[1]")
	emitUnsupported("write-array-elements", "interop/array-rw[2:4]")
	emitUnsupported("write-component", "interop/structure-rw$count")

	v, err := cl.readRaw("interop", "array-rw")
	emitRead("read", "interop/array-rw", v, err)
	v, err = cl.readRaw("interop", "structure-rw")
	emitRead("read", "interop/structure-rw", v, err)
}

// readRaw reads one variable, keeping a data access error as the value.
func (cl *client) readRaw(domain, item string) (*mms.Value, error) {
	ctx, cancel := reqCtx()
	defer cancel()
	vals, err := cl.c.Read(ctx, domain, item)
	if err != nil {
		return nil, err
	}
	if len(vals) != 1 {
		return nil, fmt.Errorf("read: %d results for one variable", len(vals))
	}
	return vals[0], nil
}

// nvlAttrs emits get-nvl-attrs for a domain ("" for VMD) list.
func nvlAttrs(c *mms.Conn, target, domain, name string) {
	ctx, cancel := reqCtx()
	members, deletable, err := c.GetNamedVariableListMembers(ctx, domain, name)
	cancel()
	if err != nil {
		emitFail("get-nvl-attrs", target, errName(err))
		return
	}
	refs := make([]string, len(members))
	for i, m := range members {
		refs[i] = refString(m.VarRef)
	}
	emitValueRaw("get-nvl-attrs", target, line{{"deletable", deletable}, {"variables", refs}})
}

func svcNamedVariableLists(c *mms.Conn) {
	define := func(target, domain, name string) {
		ctx, cancel := reqCtx()
		err := c.DefineNamedVariableList(ctx, domain, name, memberRefs)
		cancel()
		emitResult("define-nvl", target, err)
	}
	readNVL := func(target, domain, name string) {
		ctx, cancel := reqCtx()
		vals, err := c.ReadNamedVariableList(ctx, domain, name)
		cancel()
		emitList("read-nvl", target, vals, err)
	}
	deleteNVL := func(target, domain, name string) {
		ctx, cancel := reqCtx()
		err := c.DeleteNamedVariableList(ctx, domain, name)
		cancel()
		if err != nil {
			emitFail("delete-nvl", target, deleteErrName(err, errName))
		} else {
			emitOK("delete-nvl", target)
		}
	}

	// Domain scope
	define("interop/svclist", "interop", "svclist")
	{
		ctx, cancel := reqCtx()
		names, err := c.GetNameList(ctx, mms.ClassNamedVariableList, "interop")
		cancel()
		if err != nil {
			emitFail("get-name-list", "interop/nvl", errName(err))
		} else {
			emitNames("interop/nvl", names)
		}
	}
	nvlAttrs(c, "interop/svclist", "interop", "svclist")
	{
		ctx, cancel := reqCtx()
		_, vals, err := c.ReadNamedVariableListWithSpec(ctx, "interop", "svclist")
		cancel()
		emitList("read-nvl", "interop/svclist?spec", vals, err)
	}
	emitUnsupported("write-nvl", "interop/svclist")
	readNVL("interop/svclist", "interop", "svclist")
	deleteNVL("interop/svclist", "interop", "svclist")

	// VMD scope
	define("vmd/vmdlist", "", "vmdlist")
	nvlAttrs(c, "vmd/vmdlist", "", "vmdlist")
	readNVL("vmd/vmdlist", "", "vmdlist")
	deleteNVL("vmd/vmdlist", "", "vmdlist")

	// Server-defined list behind the fixture's named-list InformationReport
	nvlAttrs(c, "vmd/interop-report", "", "interop-report")
	readNVL("vmd/interop-report", "", "interop-report")

	// Association scope: no aa-specific object names in mms.Conn
	emitUnsupported("define-nvl", "aa/aalist")
	emitUnsupported("get-name-list", "aa/nvl")
	emitUnsupported("get-nvl-attrs", "aa/aalist")
	emitUnsupported("read-nvl", "aa/aalist")
	emitUnsupported("write-nvl", "aa/aalist")
	emitUnsupported("delete-nvl", "aa/aalist")
}

// --- File services ---------------------------------------------------------

func fileDirectory(c *mms.Conn, spec, target string) {
	ctx, cancel := reqCtx()
	entries, err := c.FileDirectory(ctx, spec)
	cancel()
	if err != nil {
		emitFail("file-directory", target, errName(err))
		return
	}
	s := "["
	for i, e := range entries {
		if i > 0 {
			s += ","
		}
		var ms int64
		if !e.LastModified.IsZero() {
			ms = e.LastModified.UnixMilli()
		}
		b, _ := line{{"name", e.Name}, {"size", e.Size}, {"lastModified", ms}}.MarshalJSON()
		s += string(b)
	}
	emitValueJSON("file-directory", target, raw(s+"]"))
}

// fileRead opens, reads to the end and closes a file. Emits "file-read"
// with {size, lastModified, length, fnv1a, data (hex, small files only)}
// and "file-close".
func fileRead(c *mms.Conn, name string, position uint32, target string) {
	ctx, cancel := reqCtx()
	frsm, size, err := c.FileOpenAt(ctx, name, position)
	cancel()
	if err != nil {
		emitFail("file-read", target, errName(err))
		return
	}

	var data []byte
	more := true
	for more && err == nil {
		var chunk []byte
		ctx, cancel := reqCtx()
		chunk, more, err = c.FileRead(ctx, frsm)
		cancel()
		data = append(data, chunk...)
	}
	if err != nil {
		emitFail("file-read", target, errName(err))
	} else {
		h := fnv.New64a()
		h.Write(data)
		value := line{
			{"size", size},
			// mms.Conn.FileOpenAt does not return the lastModified
			// attribute of the FileOpen response.
			{"lastModified", 0},
			{"length", len(data)},
			{"fnv1a", fmt.Sprintf("%016x", h.Sum64())},
		}
		if len(data) <= 256 {
			value = append(value, field{"data", fmt.Sprintf("%x", data)})
		}
		emitValueRaw("file-read", target, value)
	}

	ctx, cancel = reqCtx()
	err = c.FileClose(ctx, frsm)
	cancel()
	emitResult("file-close", target, err)
}

// obtainSource is the local file store the server reads an ObtainFile
// source from: libiec61850's default virtual file store directory.
const obtainSource = "vmd-filestore"

func svcFiles(c *mms.Conn) {
	fileDirectory(c, "", "")
	fileDirectory(c, "COMTRADE", "COMTRADE")
	// name of the zero-size entry a per-level server lists for the subdirectory
	fileDirectory(c, `COMTRADE\`, `COMTRADE\`)

	fileRead(c, "REC01.CFG", 0, "REC01.CFG")
	fileRead(c, "REC01.DAT", 4, "REC01.DAT@4")
	fileRead(c, "LARGE.BIN", 0, "LARGE.BIN")
	fileRead(c, "NOTIME.TXT", 0, "NOTIME.TXT")
	fileRead(c, "NO-SUCH.FILE", 0, "NO-SUCH.FILE")

	ctx, cancel := reqCtx()
	err := c.FileDelete(ctx, "DELETE.ME")
	cancel()
	emitResult("file-delete", "DELETE.ME", err)

	ctx, cancel = reqCtx()
	err = c.FileRename(ctx, "RENAME.ME", "RENAMED.ME")
	cancel()
	emitResult("file-rename", "RENAME.ME->RENAMED.ME", err)

	// The server reads REC01.CFG from this client while the request is
	// outstanding.
	ctx, cancel = context.WithTimeout(context.Background(), 4*requestTimeout)
	err = c.ObtainFile(ctx, os.DirFS(obtainSource), "REC01.CFG", "OBTAINED.CFG")
	cancel()
	emitResult("obtain-file", "REC01.CFG->OBTAINED.CFG", err)

	fileDirectory(c, "", "after-changes")
}

// --- Journals --------------------------------------------------------------

func emitJournal(target string, entries []mms.JournalEntry, err error) {
	if err != nil {
		emitFail("read-journal", target, errName(err))
		return
	}
	s := "["
	for i, e := range entries {
		if i > 0 {
			s += ","
		}
		vars := "["
		for k, jv := range e.Variables {
			if k > 0 {
				vars += ","
			}
			b, _ := line{{"tag", jv.Tag}, {"value", valueJSON(jv.Value)}}.MarshalJSON()
			vars += string(b)
		}
		b, _ := line{
			{"id", valueJSON(mms.NewOctetString(e.EntryID))},
			{"time", e.OccurrenceTime.UnixMilli()},
			{"variables", raw(vars + "]")},
		}.MarshalJSON()
		s += string(b)
	}
	// the library follows moreFollows itself: the list is complete
	emitValueRaw("read-journal", target, line{{"moreFollows", false}, {"entries", raw(s + "]")}})
}

func svcJournals(c *mms.Conn) {
	// Fixture log1 entries 1..4 are at 2024-01-01T00:00:01..04Z.
	t1 := time.UnixMilli(1704067201000).UTC()
	t4 := time.UnixMilli(1704067204000).UTC()

	ctx, cancel := reqCtx()
	entries, err := c.ReadJournalByTime(ctx, "interop", "log1", t1, t4)
	cancel()
	emitJournal("interop/log1", entries, err)

	// start after entry 2 (2024-01-01T00:00:02Z, id 2 as 8 LE bytes)
	ctx, cancel = reqCtx()
	entries, err = c.ReadJournalAfter(ctx, "interop", "log1", t1.Add(time.Second), []byte{2, 0, 0, 0, 0, 0, 0, 0})
	cancel()
	emitJournal("interop/log1@after-2", entries, err)

	ctx, cancel = reqCtx()
	entries, err = c.ReadJournalByTime(ctx, "interop", "nolog", t1, t4)
	cancel()
	emitJournal("interop/nolog", entries, err)
}

// --- InformationReport -----------------------------------------------------

func svcReports(cl *client) {
	before := reportCount.Load()
	writeErr := cl.writeItem("interop", "report-trigger", mms.NewInt32(1))
	emitWrite("write", "interop/report-trigger", writeErr)

	// Wait up to 3 s for reports, then 300 ms of quiet after the last.
	last, quiet := before, 0
	for waited := 0; waited < 3000 && quiet < 300; waited += 50 {
		time.Sleep(50 * time.Millisecond)
		n := reportCount.Load()
		switch {
		case n != last:
			last, quiet = n, 0
		case n > before:
			quiet += 50
		}
	}

	// The association must survive any unconfirmed PDUs sent along.
	v, err := cl.readRaw("interop", "boolean")
	emitRead("read", "interop/boolean#after-reports", v, err)
}

// emitWrite is emit_write of mms_client_services.c.
func emitWrite(op, target string, err error) {
	if err == nil {
		emitOK(op, target)
		return
	}
	var dae mms.DataAccessError
	if errors.As(err, &dae) {
		emitFail(op, target, daeName(dae))
		return
	}
	emitFail(op, target, errName(err))
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

// runServices runs the fixture mms-v2 service sequence.
func runServices(cl *client) {
	svcStatus(cl.c)
	svcNameLists(cl.c)
	svcTypeSpecs(cl.c)
	svcAlternateAccess(cl)
	svcNamedVariableLists(cl.c)
	svcFiles(cl.c)
	svcJournals(cl.c)
	svcReports(cl)
}

// runAbort opens a separate association, reads one variable and aborts it.
func runAbort(addr string) {
	conn, err := dial(addr)
	if err != nil {
		emitFail("abort", "", errName(err))
		return
	}
	cl := &client{c: conn}
	v, err := cl.readRaw("interop", "boolean")
	emitRead("read", "interop/boolean#before-abort", v, err)
	emitResult("abort", "", conn.Abort())
}

// runServerAbort opens a separate association and writes -1 to
// interop/report-trigger, which asks the server to abort it. The
// association ending is the real signal; the write itself must fail.
func runServerAbort(addr string) {
	conn, err := dial(addr)
	if err != nil {
		emitFail("server-abort", "", errName(err))
		return
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, werr := conn.Write(ctx, "interop", []string{"report-trigger"}, []*mms.Value{mms.NewInt32(-1)})
	cancel()

	lostConn := false
	select {
	case <-conn.Done():
		lostConn = true
	case <-time.After(200 * time.Millisecond):
	}
	switch {
	case lostConn && werr != nil:
		emitOK("server-abort", "")
	case !lostConn:
		emitFail("server-abort", "", "connection-not-lost")
	default:
		emitFail("server-abort", "", "write-succeeded")
	}
}
