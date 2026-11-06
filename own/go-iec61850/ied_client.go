// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	iec61850 "github.com/otfabric/go-iec61850"
	mms "github.com/otfabric/go-mms"
)

// iedTimeout bounds one client command (connect and whole sequence).
const iedTimeout = 60 * time.Second

// concludeTimeout bounds the wait for the Conclude response. iec61850bean's
// server never answers Conclude (go-iec61850 INTEROP.md), so after it the
// association is aborted instead.
const concludeTimeout = 5 * time.Second

// conclude ends the association: Conclude, or Abort when the server does not
// answer it in time. As the reference clients, the conclude line follows
// either way; stderr says which.
func conclude(name string, c *iec61850.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), concludeTimeout)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		logf("%s: conclude: %v; aborting the association", name, err)
		actx, acancel := context.WithTimeout(context.Background(), concludeTimeout)
		defer acancel()
		_ = c.Abort(actx)
	}
	emit(opLine("conclude", "", true, nil))
}

// clientFlags are the flags every IED client command takes.
type clientFlags struct {
	fs        *flag.FlagSet
	host      string
	port      int
	localTSel string
	remoteTSe string
}

func newClientFlags(name string) *clientFlags {
	c := &clientFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	c.fs.SetOutput(stderr)
	c.fs.StringVar(&c.host, "host", "127.0.0.1", "server host")
	c.fs.IntVar(&c.port, "port", 1102, "server port")
	return c
}

func (c *clientFlags) parse(args []string) int {
	if err := c.fs.Parse(args); err != nil {
		return exitArgs
	}
	if c.fs.NArg() > 0 {
		return argError("%s: unexpected argument %q", c.fs.Name(), c.fs.Arg(0))
	}
	if c.port <= 0 || c.port > 65535 {
		return argError("%s: invalid port %d", c.fs.Name(), c.port)
	}
	return -1
}

func (c *clientFlags) addr() string { return fmt.Sprintf("%s:%d", c.host, c.port) }

// parseTSel reads a T-selector of 1 to 4 octets written in hex.
func parseTSel(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) < 1 || len(b) > 4 {
		return nil, fmt.Errorf("invalid T-selector %q (1 to 4 octets in hex)", s)
	}
	return b, nil
}

// dial connects with go-iec61850's defaults, or with the T-selectors given.
func dial(ctx context.Context, addr string, local, remote []byte) (*iec61850.Client, error) {
	var opts iec61850.DialOptions
	if local != nil {
		opts.MMS.Transport.LocalTSelector = local
	}
	if remote != nil {
		opts.MMS.Transport.RemoteTSelector = remote
	}
	return iec61850.Dial(ctx, addr, opts)
}

// errString is the error of an operation line: the libiec61850 name of an
// MMS data access error, as the reference adapters print it, else the
// library's message. The text is not part of the contract.
func errString(err error) string {
	var dae *mms.DataAccessError
	if errors.As(err, &dae) {
		switch dae.Code {
		case mms.DataAccessErrorObjectAccessDenied:
			return "access-denied"
		case mms.DataAccessErrorTypeInconsistent:
			return "type-inconsistent"
		case mms.DataAccessErrorObjectAccessUnsupported:
			return "object-access-unsupported"
		case mms.DataAccessErrorObjectNonExistent, mms.DataAccessErrorObjectUndefined:
			return "object-does-not-exist"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return err.Error()
}

// opLine is one operation line of a fixed-sequence client.
func opLine(op, target string, ok bool, extra map[string]any) map[string]any {
	l := map[string]any{"operation": op}
	if target != "" {
		l["target"] = target
	}
	l["ok"] = ok
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func opError(op, target string, err error) map[string]any {
	return opLine(op, target, false, map[string]any{"error": errString(err)})
}

// resolveLD picks the logical device of the fixture from the server's
// directory: "InteropLD", else a name ending in it, else the first.
func resolveLD(names []string) string {
	for _, n := range names {
		if n == ldInst {
			return n
		}
	}
	for _, n := range names {
		if strings.HasSuffix(n, ldInst) {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ldInst
}

// ldOf returns the fixture's logical device as the server names it.
func ldOf(ctx context.Context, c *iec61850.Client) (string, []string, error) {
	lds, err := c.ListLogicalDevices(ctx)
	if err != nil {
		return ldInst, nil, err
	}
	names := make([]string, len(lds))
	for i, ld := range lds {
		names[i] = ld.Name
	}
	return resolveLD(names), names, nil
}

// formatG is C's %g (six significant digits), as the reference clients
// print floats.
func formatG(f float64) string {
	return strconv.FormatFloat(f, 'g', 6, 64)
}

// plain is the plain JSON value of a read, as the fixed-sequence clients
// report it (docs/CONTAINER_CONTRACT.md); a Dbpos as its enumeration.
func plain(v *iec61850.Value) (any, error) {
	m := v.MMS()
	switch m.Type() {
	case mms.ValueTypeBoolean:
		b, _ := m.Bool()
		return b, nil
	case mms.ValueTypeInteger:
		n, _ := m.Int64()
		return n, nil
	case mms.ValueTypeUnsigned:
		n, err := v.Uint64()
		return n, err
	case mms.ValueTypeFloat:
		f, _ := m.Float64()
		return jsonNumber(formatG(f)), nil
	case mms.ValueTypeVisibleString:
		return v.VisibleString()
	case mms.ValueTypeMmsString:
		return v.MmsString()
	case mms.ValueTypeBitString:
		b, err := v.BitString()
		if err != nil || len(b) == 0 {
			return nil, fmt.Errorf("unexpected-type")
		}
		return int(b[0]>>6) & 3, nil
	}
	return nil, fmt.Errorf("unexpected-type %v", m.Type())
}

// jsonNumber is a number emitted as written.
type jsonNumber string

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// runIEDClient is otfabric-ied-client: the fixed IED sequence of the
// contract on go-iec61850's client.
func runIEDClient(args []string) int {
	cf := newClientFlags("otfabric-ied-client")
	cf.fs.StringVar(&cf.localTSel, "local-tsel", "", "calling T-selector, 1 to 4 octets in hex")
	cf.fs.StringVar(&cf.remoteTSe, "remote-tsel", "", "called T-selector, 1 to 4 octets in hex")
	if code := cf.parse(args); code >= 0 {
		return code
	}
	var local, remote []byte
	var err error
	if cf.localTSel != "" {
		if local, err = parseTSel(cf.localTSel); err != nil {
			return argError("ied-client: --local-tsel: %v", err)
		}
	}
	if cf.remoteTSe != "" {
		if remote, err = parseTSel(cf.remoteTSe); err != nil {
			return argError("ied-client: --remote-tsel: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), iedTimeout)
	defer cancel()
	c, err := dial(ctx, cf.addr(), local, remote)
	if err != nil {
		logf("ied-client: connect %s: %v", cf.addr(), err)
		emit(opError("associate", "", err))
		return exitConnect
	}
	emit(opLine("associate", "", true, nil))
	failed := iedSequence(ctx, c)
	conclude("ied-client", c)
	if failed {
		return exitOpFailed
	}
	return exitOK
}

// iedSequence runs the operations between associate and conclude and
// reports whether one emitted ok:false.
func iedSequence(ctx context.Context, c *iec61850.Client) bool {
	failed := false
	fail := func(line map[string]any) {
		emit(line)
		failed = true
	}

	ld, names, err := ldOf(ctx, c)
	if err != nil {
		fail(opError("get-server-directory", "", err))
	} else {
		emit(opLine("get-server-directory", "", true, map[string]any{"names": names}))
	}

	if lns, err := c.ListLogicalNodes(ctx, ld); err != nil {
		fail(opError("get-ld-directory", ld, err))
	} else {
		n := make([]string, len(lns))
		for i, ln := range lns {
			n[i] = ln.Name
		}
		emit(opLine("get-ld-directory", ld, true, map[string]any{"names": n}))
	}

	if dos, err := c.ListDataObjects(ctx, ld, "GGIO1"); err != nil {
		fail(opError("get-ln-directory", ld+"/GGIO1", err))
	} else {
		n := make([]string, len(dos))
		for i, do := range dos {
			n[i] = do.Name
		}
		emit(opLine("get-ln-directory", ld+"/GGIO1", true, map[string]any{"names": n}))
	}

	for _, r := range []string{
		"GGIO1.SPS1.stVal[ST]", "MMXU1.TotW.mag.f[MX]", "GGIO1.DPS1.stVal[ST]",
		"MMTR1.TotVAh.actVal[ST]", "LLN0.Mod.ctlModel[CF]", "LLN0.Mod.d[DC]",
	} {
		target := ld + "/" + r
		ref, err := iec61850.ParseRef(target)
		if err != nil {
			fail(opError("read", target, err))
			continue
		}
		v, err := c.Read(ctx, ref)
		if err != nil {
			fail(opError("read", target, err))
			continue
		}
		p, err := plain(v)
		if err != nil {
			fail(opError("read", target, err))
			continue
		}
		emit(opLine("read", target, true, map[string]any{"value": p}))
	}

	target := ld + "/LLN0.Mod.stVal[ST]"
	if ref, err := iec61850.ParseRef(target); err != nil {
		fail(opError("write", target, err))
	} else if err := c.Write(ctx, ref, mms.NewInteger(5)); err != nil {
		fail(opError("write", target, err))
	} else {
		emit(opLine("write", target, true, nil))
	}

	target = ld + "/LLN0$dsInterop"
	if vals, err := c.ReadDataSet(ctx, ld, "LLN0$dsInterop"); err != nil {
		fail(opError("read-dataset", target, err))
	} else {
		out := make([]any, len(vals))
		for i, dv := range vals {
			if dv.Err != nil || dv.Value == nil {
				continue
			}
			if p, err := plain(dv.Value); err == nil {
				out[i] = p
			}
		}
		emit(opLine("read-dataset", target, true, map[string]any{"values": out}))
	}
	return failed
}
