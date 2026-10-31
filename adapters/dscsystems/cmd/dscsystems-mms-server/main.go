// SPDX-License-Identifier: GPL-3.0-only

// dscsystems-mms-server — generic MMS server for iec61850-interop on
// github.com/dscsystems/go-iec61850.
//
// Port of adapters/libiec61850/src/mms_server.c. The library's server
// package serves an IEC 61850 model, which cannot express the fixture's
// plain MMS domain; this command therefore answers requests itself on top
// of the association layer (mms.AcceptConnOpts / ServerConn.Serve), using
// the library's codecs for data, type specifications, file names and
// alternate access.
//
// Lifecycle:
//  1. Parse --fixture, --port and --filestore arguments.
//  2. Load fixture JSON.
//  3. Build the domain model: variables, journals, InformationReports.
//  4. Write the fixture files into the file store (a fresh temporary
//     directory unless --filestore is given).
//  5. Listen, emit {"event":"ready","address":"0.0.0.0:<port>",...}.
//  6. Serve associations until SIGTERM/SIGINT, then clean up.
//
// Services beyond plain read/write, as in the C adapter:
//   - alternate access reads and writes (array element, structure
//     component, and "name$component" flattened names);
//   - writing interop/report-trigger sends the fixture's
//     InformationReports to the writing connection;
//   - named variable lists in domain, VMD and association scope;
//   - ReadJournal over the fixture journals;
//   - file services (directory, open/read/close, delete, rename, obtain).
//
// Exit codes:
//
//	0  clean shutdown
//	1  startup / argument error
//	3  fixture load error
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/otfabric/iec61850-interop/adapters/dscsystems/internal/jl"
)

func main() {
	jl.HandleMetaFlags()

	fixturePath := "/fixtures/mms/interop.json"
	filestoreArg := ""
	port := 1102

	// argument parsing: "--flag value" pairs, unknown flags ignored as in C
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--fixture":
			fixturePath = args[i+1]
		case "--port":
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				jl.Fatal(1, "error: invalid --port %q", args[i+1])
			}
			port = p
		case "--filestore":
			filestoreArg = args[i+1]
		}
	}

	fix, err := loadFixture(fixturePath)
	if err != nil {
		jl.Fatal(3, "error: fixture load failed: %s: %v", fixturePath, err)
	}
	jl.Log("loaded fixture %s: %d domain(s), %d file(s)", fix.revision, len(fix.domains), len(fix.files))

	// file store: a fresh directory per run so delete/rename never leak
	storeDir := filestoreArg
	if storeDir == "" {
		if storeDir, err = os.MkdirTemp("", "mms-filestore-"); err != nil {
			jl.Fatal(1, "error: mkdtemp failed: %v", err)
		}
	} else if err := os.MkdirAll(storeDir, 0o755); err != nil {
		jl.Fatal(1, "error: cannot create file store %s: %v", storeDir, err)
	}
	if err := populateFilestore(fix, storeDir); err != nil {
		jl.Fatal(1, "error: cannot populate file store %s: %v", storeDir, err)
	}
	jl.Log("file store at %s", storeDir)
	cleanup := func() {
		if filestoreArg == "" {
			_ = os.RemoveAll(storeDir)
		}
	}

	srv, err := newServer(fix, storeDir)
	if err != nil {
		cleanup()
		jl.Fatal(1, "error: %v", err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		cleanup()
		jl.Fatal(1, "error: server failed to start on port %d: %v", port, err)
	}

	jl.Ready(fmt.Sprintf("0.0.0.0:%d", port), fix.revision, "")
	jl.Log("listening on port %d", port)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		_ = ln.Close()
	}()

	for {
		raw, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			jl.Log("accept: %v", err)
			continue
		}
		go srv.serve(raw)
	}

	jl.Log("shutting down")
	srv.close()
	cleanup()
}

// populateFilestore writes the fixture files below dir and sets their
// modification times; files without one keep the time of writing.
func populateFilestore(f *fixtureDef, dir string) error {
	for _, fd := range f.files {
		path := filepath.Join(dir, filepath.FromSlash(fd.name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, fd.data, 0o644); err != nil {
			return err
		}
		if !fd.lastModified.IsZero() {
			if err := os.Chtimes(path, fd.lastModified, fd.lastModified); err != nil {
				return err
			}
		}
		jl.Log("  file %s (%d bytes)", fd.name, len(fd.data))
	}
	return nil
}

// acceptOptions answers the Initiate with the services this server
// implements.
func acceptOptions() mms.AcceptOptions {
	init := mms.DefaultServerInitiate()
	init.Services = mms.NewServiceSupport(
		mms.ServiceStatus, mms.ServiceGetNameList, mms.ServiceIdentify,
		mms.ServiceRead, mms.ServiceWrite, mms.ServiceGetVariableAccessAttributes,
		mms.ServiceDefineNamedVariableList, mms.ServiceGetNamedVariableListAttributes,
		mms.ServiceDeleteNamedVariableList, mms.ServiceObtainFile, mms.ServiceReadJournal,
		mms.ServiceFileOpen, mms.ServiceFileRead, mms.ServiceFileClose,
		mms.ServiceFileRename, mms.ServiceFileDelete, mms.ServiceFileDirectory,
		mms.ServiceInformationReport, mms.ServiceConclude,
	)
	return mms.AcceptOptions{Initiate: &init, HandshakeTimeout: 10 * time.Second}
}
