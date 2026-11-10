// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
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
	// A server that has already closed on a head it will not accept may reset the write of a long one.
	// That is a close, not a failure: what the test asserts is the bytes read.
	if _, err := io.WriteString(conn, request); err != nil && !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE) {
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

// wantOnly404 fails unless got is exactly one 404 response and nothing after it.
func wantOnly404(t *testing.T, got string) {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(got))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("the reply %q is not a response: %v", got, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || string(body) != "404 page not found\n" {
		t.Fatalf("got status %d body %q, want 404 and the not-found body", resp.StatusCode, body)
	}
	if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
		t.Fatalf("bytes follow the 404 response in %q", got)
	}
}

func TestPlainWireMatrix(t *testing.T) {
	loopback := startWire(t, nil, "")
	proxied := startWire(t, map[string]string{"TIERPLAN_TRUSTED_PROXIES": "203.0.113.0/24"}, "203.0.113.9:40000")
	const closeAfter = "Connection: close\r\n"
	const challenge = "/.well-known/acme-challenge"
	oversize := "GET / HTTP/1.1\r\nHost: x\r\nX-Pad: " + strings.Repeat("a", plainHeadLimit+1024) + "\r\n\r\n"
	tests := []struct {
		name    string
		w       *wireServer
		request string
		want404 bool // otherwise the connection is closed with nothing written
	}{
		{"garbage", loopback, "GARBAGE\r\n\r\n", false},
		{"a head over the limit", loopback, oversize, false},
		{"the h2c preface", loopback, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", false},
		{"healthz from loopback", loopback, "GET /healthz HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", false},
		{"readyz from loopback", loopback, "GET /readyz HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", false},
		{"healthz from a trusted proxy marked https", proxied, "GET /healthz HTTP/1.1\r\nHost: x\r\nX-Forwarded-Proto: https\r\n" + closeAfter + "\r\n", false},
		{"the root from a trusted proxy marked https", proxied, "GET / HTTP/1.1\r\nHost: x\r\nX-Forwarded-Proto: https\r\n" + closeAfter + "\r\n", false},
		{"a challenge token", loopback, "GET " + challenge + "/x HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", true},
		{"a challenge token from a trusted proxy peer", proxied, "GET " + challenge + "/x HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", true},
		{"the challenge directory without its slash", loopback, "GET " + challenge + " HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", false},
		{"a path that only extends the challenge name", loopback, "GET " + challenge + "X HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", false},
		{"absolute-form challenge target", loopback, "GET http://x" + challenge + "/t HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", true},
		{"absolute-form target outside the challenge", loopback, "GET http://x/healthz HTTP/1.1\r\nHost: x\r\n" + closeAfter + "\r\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := plainExchange(t, tc.w.addr, tc.request)
			if tc.want404 {
				wantOnly404(t, got)
				return
			}
			if got != "" {
				t.Fatalf("the plain request was answered with %q, want no bytes", got)
			}
		})
	}

	t.Run("a challenge POST still gets only the challenge answer", func(t *testing.T) {
		// The token store, which answers the challenge path since Phase 14, refuses any method but GET
		// and HEAD with 405. That is the challenge handler's own answer, and nothing follows it.
		got := plainExchange(t, loopback.addr, "POST "+challenge+"/x HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n"+closeAfter+"\r\n")
		br := bufio.NewReader(strings.NewReader(got))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("the reply %q is not a response: %v", got, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("got status %d, want 405 from the challenge handler", resp.StatusCode)
		}
		if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
			t.Fatalf("bytes follow the response in %q", got)
		}
	})
	t.Run("a challenge request then a pipelined request outside the challenge", func(t *testing.T) {
		got := plainExchange(t, loopback.addr, "GET "+challenge+"/a HTTP/1.1\r\nHost: x\r\n\r\nGET / HTTP/1.1\r\nHost: x\r\n\r\n")
		wantOnly404(t, got)
	})
	t.Run("two pipelined challenge requests are both answered", func(t *testing.T) {
		got := plainExchange(t, loopback.addr, "GET "+challenge+"/a HTTP/1.1\r\nHost: x\r\n\r\nGET "+challenge+"/b HTTP/1.1\r\nHost: x\r\n"+closeAfter+"\r\n")
		br := bufio.NewReader(strings.NewReader(got))
		for i := range 2 {
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("response %d: %v", i, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("response %d status %d, want 404", i, resp.StatusCode)
			}
		}
		if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
			t.Fatalf("bytes follow the second response in %q", got)
		}
	})
}

func TestTLSStillServesHealthz(t *testing.T) {
	w := startWire(t, nil, "")
	got := wireGet(t, trustedClient(t, w.cfg), httpsBase(w.addr)+"/healthz", "")
	if got.status != http.StatusOK {
		t.Fatalf("https /healthz = %d, want 200", got.status)
	}
}

// startWireWithChallenge is startWire with an HTTP-01 handler installed before the listener
// starts serving, so the real sniffing listener routes the challenge prefix to it.
func startWireWithChallenge(t *testing.T, challenge http.Handler) string {
	t.Helper()
	cfg := testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": t.TempDir() + "/data"})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.challenge = challenge
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String()
}

// plainResponse sends one challenge GET on a fresh plain connection and returns the response
// with its body read.
func plainResponse(t *testing.T, addr, path string) (*http.Response, string) {
	t.Helper()
	got := plainExchange(t, addr, "GET "+path+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(got)), nil)
	if err != nil {
		t.Fatalf("the reply %q to %s is not a response: %v", got, path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestPlainChallengeServesAPresentedToken(t *testing.T) {
	ch := acme.NewChallenges()
	addr := startWireWithChallenge(t, ch.Handler())
	const token, keyAuth = "tok_en-1", "tok_en-1.thumbprint"
	if err := ch.Present(t.Context(), "tierplan.example.com", token, keyAuth); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		resp, body := plainResponse(t, addr, acmeChallengePrefix+token)
		if resp.StatusCode != http.StatusOK || body != keyAuth {
			t.Fatalf("request %d = %d %q, want 200 and the key authorization", i, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain" {
			t.Fatalf("Content-Type = %q, want text/plain", got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	}
	if resp, _ := plainResponse(t, addr, acmeChallengePrefix+"unknown"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown token = %d, want 404", resp.StatusCode)
	}
	if err := ch.CleanUp(t.Context(), "tierplan.example.com", token, keyAuth); err != nil {
		t.Fatal(err)
	}
	if resp, _ := plainResponse(t, addr, acmeChallengePrefix+token); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a cleaned-up token = %d, want 404", resp.StatusCode)
	}
	if got := plainExchange(t, addr, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); got != "" {
		t.Fatalf("a plain request outside the challenge was answered with %q, want no bytes", got)
	}
}
