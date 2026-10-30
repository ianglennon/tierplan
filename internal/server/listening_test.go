// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/proto/testsupport"
)

// pickAddress returns a loopback address that was free a moment ago.
func pickAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestListeningClosesOnceTheServerHasBoundItsAddress(t *testing.T) {
	addr := pickAddress(t)
	srv, err := Build(context.Background(), testConfig(t, map[string]string{
		"COAMING_DATA_DIR": t.TempDir() + "/data",
		"COAMING_LISTEN":   addr,
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Listening():
		t.Fatal("Listening was closed before Run was called")
	default:
	}

	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	testsupport.Await(t, "the server to bind its address", srv.Listening())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz straight after Listening closed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz straight after Listening closed = %d, want 200", resp.StatusCode)
	}
}

func TestListeningStaysOpenWhenTheAddressIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	srv, err := Build(context.Background(), testConfig(t, map[string]string{
		"COAMING_DATA_DIR": t.TempDir() + "/data",
		"COAMING_LISTEN":   taken.Addr().String(),
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Run(testsupport.Context(t)) }()
	if err := testsupport.Await(t, "Run to report that the address is taken", done); err == nil {
		t.Fatal("Run returned nil on an address that is taken")
	}
	select {
	case <-srv.Listening():
		t.Fatal("Listening was closed although Run could not bind its address")
	default:
	}
}

// listeningLines is a log destination that reports the address of every
// "coaming listening" line it is given, so a test learns that the line was
// written from the line itself instead of from a timer.
type listeningLines struct{ addrs chan string }

func (w listeningLines) Write(p []byte) (int, error) {
	var rec struct {
		Msg    string `json:"msg"`
		Addr   string `json:"addr"`
		Scheme string `json:"scheme"`
	}
	if json.Unmarshal(p, &rec) == nil && rec.Msg == "coaming listening" {
		select {
		case w.addrs <- rec.Scheme + "://" + rec.Addr:
		default:
		}
	}
	return len(p), nil
}

// WR-07, product half: a caller that binds port 0 learns the port from the
// server's own log line, in the JSON form cmd/coaming configures
// ({"msg":"coaming listening","addr":"127.0.0.1:<port>","scheme":"http"}), so
// scripts/smoke.sh does not probe for a free port and race the other runner on
// the same VM.
func TestTheListeningLogLineNamesTheAddressThatWasBound(t *testing.T) {
	lines := listeningLines{addrs: make(chan string, 1)}
	srv, err := Build(context.Background(), testConfig(t, map[string]string{
		"COAMING_DATA_DIR": t.TempDir() + "/data",
		"COAMING_LISTEN":   "127.0.0.1:0",
	}), slog.New(slog.NewJSONHandler(lines, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	got := testsupport.Await(t, "the listening log line", lines.addrs)
	base, found := strings.CutPrefix(got, "http://")
	if !found {
		t.Fatalf("listening line = %q, want an http:// address (no TLS is configured)", got)
	}
	host, port, err := net.SplitHostPort(base)
	if err != nil {
		t.Fatalf("listening address %q: %v", base, err)
	}
	if host != "127.0.0.1" || port == "0" || port == "" {
		t.Fatalf("listening address = %q, want 127.0.0.1 and the port the kernel assigned, not 0", base)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, got+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz on the address the server logged: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz on the logged address = %d, want 200", resp.StatusCode)
	}
}
