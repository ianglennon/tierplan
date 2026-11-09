// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// plainExchange dials addr over plain TCP, writes request and returns every byte the server sent
// before it closed the connection. A reset after the close counts as the close. A server that never
// closes ends the read at the test's deadline.
func plainExchange(t *testing.T, addr, request string) string {
	t.Helper()
	ctx := testsupport.Context(t)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	t.Cleanup(func() {
		stop()
		_ = conn.Close()
	})
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("reading until the server closed: %v", err)
	}
	return string(got)
}

func TestPlainNonChallengeRequestIsClosedWithNoBytes(t *testing.T) {
	w := startWire(t, nil, "")
	for _, tc := range []struct{ name, request string }{
		{"the root", "GET / HTTP/1.1\r\nHost: x\r\n\r\n"},
		{"healthz", "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"},
		{"readyz", "GET /readyz HTTP/1.1\r\nHost: x\r\n\r\n"},
		{"a request a trusted proxy marks https", "GET /healthz HTTP/1.1\r\nHost: x\r\nX-Forwarded-Proto: https\r\n\r\n"},
		{"the challenge prefix without the leading slash", "GET .well-known/acme-challenge/x HTTP/1.1\r\nHost: x\r\n\r\n"},
		{"a dotted path that only cleans to the challenge", "GET /x/../.well-known/acme-challenge/t HTTP/1.1\r\nHost: x\r\n\r\n"},
		{"a malformed request line", "NONSENSE\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := plainExchange(t, w.addr, tc.request); got != "" {
				t.Fatalf("a plain non-challenge request was answered with %q, want no bytes", got)
			}
		})
	}
}

func TestPlainChallengeRequestGets404(t *testing.T) {
	w := startWire(t, nil, "")
	got := plainExchange(t, w.addr, "GET "+acmeChallengePrefix+"token HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	line, _, _ := strings.Cut(got, "\r\n")
	if line != "HTTP/1.1 404 Not Found" {
		t.Fatalf("a plain challenge request got %q, want HTTP/1.1 404 Not Found", line)
	}
}

func TestPlainSecondRequestOnAKeptAliveChallengeConnectionIsClosedUnanswered(t *testing.T) {
	w := startWire(t, nil, "")
	ctx := testsupport.Context(t)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", w.addr)
	if err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	t.Cleanup(func() {
		stop()
		_ = conn.Close()
	})
	if _, err := io.WriteString(conn, "GET "+acmeChallengePrefix+"a HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("first request status %d, want 404", resp.StatusCode)
	}
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(br)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Fatalf("a second, non-challenge request on the connection was answered with %q", rest)
	}
}

func TestTLSStillServesHealthz(t *testing.T) {
	w := startWire(t, nil, "")
	got := wireGet(t, trustedClient(t, w.cfg), httpsBase(w.addr)+"/healthz", "")
	if got.status != http.StatusOK {
		t.Fatalf("https /healthz = %d, want 200", got.status)
	}
}
