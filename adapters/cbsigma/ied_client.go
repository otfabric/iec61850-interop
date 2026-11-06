// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	iec61850 "github.com/cB-Sigma/iec61850"
	"github.com/cB-Sigma/iec61850/mms"
)

// iedTimeout bounds one client command (connect and whole sequence).
const iedTimeout = 60 * time.Second

// concludeTimeout bounds the Conclude: iec61850bean's server never answers
// it, and Client.Close then closes the connection all the same.
const concludeTimeout = 5 * time.Second

// conclude ends the association; the conclude line follows either way, as
// with the other adapters.
func conclude(c *iec61850.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), concludeTimeout)
	defer cancel()
	c.Close(ctx)
	emit(opLine("conclude", "", true, nil))
}

// clientFlags are the flags every IED client command takes.
type clientFlags struct {
	fs   *flag.FlagSet
	host string
	port int
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

// dial associates with cB-Sigma's defaults (T-, S- and P-selectors 0001,
// 0001, 00000001), or with the T-selectors given.
func dial(ctx context.Context, addr string, local, remote []byte) (*iec61850.Client, error) {
	o := mms.DialOptions{Timeout: 10 * time.Second}
	o.Params.LocalTSel, o.Params.RemoteTSel = local, remote
	return iec61850.Connect(ctx, addr, o)
}

// errString is the error of an operation line: the libiec61850 name of an
// MMS data access error, as the other adapters print it, else the
// library's message. The text is not part of the contract.
func errString(err error) string {
	var dae mms.DataAccessError
	if errors.As(err, &dae) {
		switch dae {
		case mms.ErrObjectAccessDenied:
			return "access-denied"
		case mms.ErrTypeInconsistent:
			return "type-inconsistent"
		case mms.ErrObjectAccessUnsupport:
			return "object-access-unsupported"
		case mms.ErrObjectNonExistent, mms.ErrObjectUndefined:
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
	names, err := c.LogicalDevices(ctx)
	if err != nil {
		return ldInst, nil, err
	}
	return resolveLD(names), names, nil
}

// formatG is C's %g (six significant digits), as the reference clients
// print floats.
func formatG(f float64) string {
	return strconv.FormatFloat(f, 'g', 6, 64)
}

// jsonNumber is a number emitted as written.
type jsonNumber string

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// plain is the plain JSON value of a read, as the fixed-sequence clients
// report it (docs/CONTAINER_CONTRACT.md); a Dbpos as its enumeration.
func plain(d mms.Data) (any, error) {
	switch d.Kind {
	case mms.KBool:
		return d.Bool, nil
	case mms.KInt:
		return d.Int, nil
	case mms.KUint:
		return d.Uint, nil
	case mms.KFloat:
		return jsonNumber(formatG(d.Float)), nil
	case mms.KVisible, mms.KMMSString:
		return d.Str, nil
	case mms.KBitString:
		if len(d.Bits.Bits) == 0 {
			return nil, errors.New("unexpected-type")
		}
		return int(d.Bits.Bits[0]>>6) & 3, nil
	}
	return nil, fmt.Errorf("unexpected-type %v", d.Kind)
}

// readRef reads one attribute given as "LD/LN.DO.DA[FC]".
func readRef(ctx context.Context, c *iec61850.Client, target string) (mms.Data, error) {
	ref, err := parseTarget(target)
	if err != nil {
		return mms.Data{}, err
	}
	return c.MMS().ReadOne(ctx, ref.MMS())
}

// parseTarget reads the contract's "LD/LN.DO.DA[FC]".
func parseTarget(target string) (iec61850.Ref, error) {
	s, fc, ok := strings.Cut(target, "[")
	if !ok || !strings.HasSuffix(fc, "]") {
		return iec61850.Ref{}, fmt.Errorf("reference %q has no [FC]", target)
	}
	return iec61850.ParseRef(s, strings.TrimSuffix(fc, "]"))
}

// runIEDClient is cbsigma-ied-client: the fixed IED sequence of the
// contract on cB-Sigma's client.
func runIEDClient(args []string) int {
	cf := newClientFlags("cbsigma-ied-client")
	localTSel := cf.fs.String("local-tsel", "", "calling T-selector, 1 to 4 octets in hex")
	remoteTSel := cf.fs.String("remote-tsel", "", "called T-selector, 1 to 4 octets in hex")
	if code := cf.parse(args); code >= 0 {
		return code
	}
	var local, remote []byte
	var err error
	if *localTSel != "" {
		if local, err = parseTSel(*localTSel); err != nil {
			return argError("ied-client: --local-tsel: %v", err)
		}
	}
	if *remoteTSel != "" {
		if remote, err = parseTSel(*remoteTSel); err != nil {
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
	conclude(c)
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

	// GetLogicalDeviceDirectory and GetLogicalNodeDirectory: the
	// library's discovery (GetNameList of the domain's named variables).
	if m, err := c.Discover(ctx, ld); err != nil {
		fail(opError("get-ld-directory", ld, err))
		fail(opError("get-ln-directory", ld+"/GGIO1", err))
	} else {
		lns := make([]string, 0, len(m.LogicalNodes))
		for ln := range m.LogicalNodes {
			lns = append(lns, ln)
		}
		sort.Strings(lns)
		emit(opLine("get-ld-directory", ld, true, map[string]any{"names": lns}))
		if fcs, ok := m.LogicalNodes["GGIO1"]; !ok {
			fail(opError("get-ln-directory", ld+"/GGIO1", errors.New("object-does-not-exist")))
		} else {
			seen := map[string]bool{}
			var dos []string
			for _, list := range fcs {
				for _, do := range list {
					if !seen[do] {
						seen[do] = true
						dos = append(dos, do)
					}
				}
			}
			sort.Strings(dos)
			emit(opLine("get-ln-directory", ld+"/GGIO1", true, map[string]any{"names": dos}))
		}
	}

	for _, r := range []string{
		"GGIO1.SPS1.stVal[ST]", "MMXU1.TotW.mag.f[MX]", "GGIO1.DPS1.stVal[ST]",
		"MMTR1.TotVAh.actVal[ST]", "LLN0.Mod.ctlModel[CF]", "LLN0.Mod.d[DC]",
	} {
		target := ld + "/" + r
		v, err := readRef(ctx, c, target)
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
	if ref, err := parseTarget(target); err != nil {
		fail(opError("write", target, err))
	} else if err := c.MMS().WriteOne(ctx, ref.MMS(), mms.Integer(5)); err != nil {
		fail(opError("write", target, err))
	} else {
		emit(opLine("write", target, true, nil))
	}

	target = ld + "/LLN0$dsInterop"
	if res, err := c.DataSetValues(ctx, target); err != nil {
		fail(opError("read-dataset", target, err))
	} else {
		out := make([]any, len(res))
		for i, r := range res {
			if r.Err != nil {
				continue
			}
			if p, err := plain(r.Data); err == nil {
				out[i] = p
			}
		}
		emit(opLine("read-dataset", target, true, map[string]any{"values": out}))
	}
	return failed
}
