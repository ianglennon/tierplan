// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestSelfStubClientIP proves the stub reports the client address the way the real server derives it:
// from a peer inside the trusted ranges it takes the X-Forwarded-For entry, and from a peer outside
// them it ignores the header and reports the peer. The 28-36 forged-header test relies on both.
func TestSelfStubClientIP(t *testing.T) {
	t.Setenv(trustedEnv, "10.9.0.0/24")
	trusted, err := trustedFromEnv()
	if err != nil {
		t.Fatalf("read %s: %v", trustedEnv, err)
	}
	mux := newTLSMux(stubConfig{trusted: trusted})

	const forwarded = "203.0.113.9"
	for _, tc := range []struct {
		name, peer, wantClient string
	}{
		{"a peer inside the trusted range", "10.9.0.7:5000", forwarded},
		{"a peer outside the trusted range", "192.0.2.9:5000", "192.0.2.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
			req.RemoteAddr = tc.peer
			req.Header.Set("X-Forwarded-For", forwarded)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("/client-ip answered %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if want := "client=" + tc.wantClient + "\n"; !strings.Contains(body, want) {
				t.Errorf("/client-ip answered %q, want it to hold %q", body, want)
			}
			if want := "xff=" + forwarded + "\n"; !strings.Contains(body, want) {
				t.Errorf("/client-ip answered %q, want the raw header in it as %q", body, want)
			}
		})
	}
}

// TestSelfStubClientIPWithNoTrustedRanges is the default: with TIERPLAN_STUB_TRUSTED unset no peer is
// a proxy, so the header is never believed.
func TestSelfStubClientIPWithNoTrustedRanges(t *testing.T) {
	t.Setenv(trustedEnv, "")
	trusted, err := trustedFromEnv()
	if err != nil {
		t.Fatalf("read %s: %v", trustedEnv, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
	req.RemoteAddr = "10.9.0.7:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	newTLSMux(stubConfig{trusted: trusted}).ServeHTTP(rec, req)
	if want := "client=10.9.0.7\n"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("/client-ip answered %q, want it to hold %q", rec.Body.String(), want)
	}
}

// TestSelfStubPlainOnlyAnswersTheChallenge proves the plain side is the shape of the real server's: the
// challenge path is answered with the token, and anything else gets no HTTP answer at all.
func TestSelfStubPlainOnlyAnswersTheChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(plainChallenge))
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	resp, err := client.Get(srv.URL + acmeChallengePrefix + "abc")
	if err != nil {
		t.Fatalf("GET the challenge path: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "abc.stub" {
		t.Errorf("the challenge path answered %d %q, want 200 %q", resp.StatusCode, body, "abc.stub")
	}

	for _, p := range []string{"/", "/events", acmeChallengePrefix} {
		if resp, err := client.Get(srv.URL + p); err == nil {
			_ = resp.Body.Close()
			t.Errorf("a plain GET %s answered %d, want the connection closed with no answer", p, resp.StatusCode)
		}
	}
}

// TestSelfStubDispatchSplitsOnTheFirstByte proves one port serves both sides: a connection that opens
// with a TLS record goes to the TLS side and any other goes to the plain side, with its first byte
// still readable.
func TestSelfStubDispatchSplitsOnTheFirstByte(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	tlsSide, plainSide := newChanListener(ln.Addr()), newChanListener(ln.Addr())
	defer func() { _ = tlsSide.Close(); _ = plainSide.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go dispatch(conn, tlsSide, plainSide)
		}
	}()

	for _, tc := range []struct {
		name  string
		first byte
		side  *chanListener
	}{
		{"a TLS record", tlsFirstByte, tlsSide},
		{"a plain request", 'G', plainSide},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			if _, err := client.Write([]byte{tc.first}); err != nil {
				t.Fatal(err)
			}
			got := make(chan net.Conn, 1)
			go func() {
				c, err := tc.side.Accept()
				if err == nil {
					got <- c
				}
			}()
			conn := testsupport.Await(t, "the connection to reach its side", got)
			defer func() { _ = conn.Close() }()
			b := make([]byte, 1)
			if _, err := io.ReadFull(conn, b); err != nil || b[0] != tc.first {
				t.Errorf("the dispatched connection read %v (error %v), want its first byte %#x back", b, err, tc.first)
			}
		})
	}
}
