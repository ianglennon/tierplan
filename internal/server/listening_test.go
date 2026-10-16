// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"github.com/ianglennon/bayplan/proto/testsupport"
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
		"BAYPLAN_DATA_DIR": t.TempDir() + "/data",
		"BAYPLAN_LISTEN":   addr,
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
		"BAYPLAN_DATA_DIR": t.TempDir() + "/data",
		"BAYPLAN_LISTEN":   taken.Addr().String(),
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
