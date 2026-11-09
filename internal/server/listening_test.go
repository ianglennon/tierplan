// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
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
	cfg := testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR": t.TempDir() + "/data",
		"TIERPLAN_LISTEN":   addr,
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := trustedClient(t, cfg).Do(req)
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
		"TIERPLAN_DATA_DIR": t.TempDir() + "/data",
		"TIERPLAN_LISTEN":   taken.Addr().String(),
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
// "tierplan listening" line it is given, so a test learns that the line was
// written from the line itself instead of from a timer.
type listeningLines struct{ addrs chan string }

func (w listeningLines) Write(p []byte) (int, error) {
	var rec struct {
		Msg    string `json:"msg"`
		Addr   string `json:"addr"`
		Scheme string `json:"scheme"`
	}
	if json.Unmarshal(p, &rec) == nil && rec.Msg == "tierplan listening" {
		select {
		case w.addrs <- rec.Scheme + "://" + rec.Addr:
		default:
		}
	}
	return len(p), nil
}

// WR-07, product half: a caller that binds port 0 learns the port from the
// server's own log line, in the JSON form cmd/tierplan configures
// ({"msg":"tierplan listening","addr":"127.0.0.1:<port>","scheme":"https"}), so
// scripts/smoke.sh does not probe for a free port and race the other runner on
// the same VM.
func TestTheListeningLogLineNamesTheAddressThatWasBound(t *testing.T) {
	lines := listeningLines{addrs: make(chan string, 1)}
	cfg := testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR": t.TempDir() + "/data",
		"TIERPLAN_LISTEN":   "127.0.0.1:0",
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(lines, nil)))
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
	base, found := strings.CutPrefix(got, "https://")
	if !found {
		t.Fatalf("listening line = %q, want an https:// address (every install serves HTTPS)", got)
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
	mirror, err := os.ReadFile(HealthcheckCertFile(cfg)) //nolint:gosec // the mirror under the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(mirror) {
		t.Fatal("the mirror holds no certificate")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz on the address the server logged: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz on the logged address = %d, want 200", resp.StatusCode)
	}
}
