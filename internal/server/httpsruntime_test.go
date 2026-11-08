// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// getter is a source that always answers with one certificate.
func getter(c tls.Certificate) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &c, nil }
}

// serveEcho serves TLS with cfg on a loopback listener and echoes what each connection sends. It
// returns the address and stops, and closes every connection, when the test ends.
func serveEcho(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(raw, cfg)
	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Go(func() { _, _ = io.Copy(c, c) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return raw.Addr().String()
}

func dialTrusting(t *testing.T, addr string, certs ...*x509.Certificate) *tls.Conn {
	t.Helper()
	roots := x509.NewCertPool()
	for _, c := range certs {
		roots.AddCert(c)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("TLS handshake with %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// echo sends one byte and reads it back.
func echo(t *testing.T, c *tls.Conn) {
	t.Helper()
	if _, err := c.Write([]byte{'x'}); err != nil {
		t.Fatalf("write on an open connection: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read on an open connection: %v", err)
	}
}

func TestHTTPSRuntimeIsNotLiveUntilInstalled(t *testing.T) {
	rt := newHTTPSRuntime()
	if rt.Live() || rt.Kind() != tlsKindNone || rt.ServerConfig() != nil {
		t.Errorf("a new runtime: Live %v, Kind %v, ServerConfig %v; want not live, none and nil", rt.Live(), rt.Kind(), rt.ServerConfig())
	}
	cfg := rt.Config()
	if cfg == nil {
		t.Fatal("Config is nil; spkiPin and fingerprint need it before anything is installed")
	}
	if !slices.Equal(cfg.NextProtos, []string{"h2", "http/1.1"}) || cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("NextProtos %v, MinVersion %#x; want h2 then http/1.1 and TLS 1.2", cfg.NextProtos, cfg.MinVersion)
	}
	if len(cfg.Certificates) != 0 {
		t.Errorf("Certificates holds %d entries; it must stay empty so GetCertificate always answers", len(cfg.Certificates))
	}
	if cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); !errors.Is(err, errTLSNotLive) || cert != nil {
		t.Errorf("GetCertificate = %v, %v; want errTLSNotLive", cert, err)
	}
	if rt.Pins() != nil || rt.Pin() != "" || rt.HSTSAllowed() {
		t.Errorf("Pins %v, Pin %q, HSTSAllowed %v; want none, empty and false", rt.Pins(), rt.Pin(), rt.HSTSAllowed())
	}
	// A nil source, or the none kind, installs nothing.
	rt.Install(tlsKindSupplied, nil)
	rt.Install(tlsKindNone, getter(tls.Certificate{}))
	if rt.Live() {
		t.Error("Install with no source made the runtime live")
	}
}

func TestHTTPSRuntimeSwapReachesOnlyNewHandshakes(t *testing.T) {
	certA, a := genCert(t, "a.example.com")
	certB, b := genCert(t, "b.example.com")
	rt := newHTTPSRuntime()
	rt.Install(tlsKindSupplied, getter(certA))
	addr := serveEcho(t, rt.ServerConfig())

	if got := dialTrusting(t, addr, a, b).ConnectionState().PeerCertificates[0]; got.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Fatal("a fresh client does not see A")
	}
	held := dialTrusting(t, addr, a, b)
	echo(t, held)

	rt.Install(tlsKindSupplied, getter(certB))

	if got := dialTrusting(t, addr, a, b).ConnectionState().PeerCertificates[0]; got.SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("a handshake that starts after the install does not see B")
	}
	echo(t, held)
	if got := held.ConnectionState().PeerCertificates[0]; got.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Error("the connection opened before the swap does not still show A")
	}
}

func TestHTTPSRuntimeRemoveFailsAPendingHandshakeWithoutAPanic(t *testing.T) {
	cert, leaf := genCert(t, "a.example.com")
	rt := newHTTPSRuntime()
	rt.Install(tlsKindSupplied, getter(cert))
	captured := rt.ServerConfig()
	rt.Remove()

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	handshook := make(chan error, 1)
	go func() { handshook <- tls.Server(server, captured).Handshake() }()

	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if err := tls.Client(client, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}).Handshake(); err == nil {
		t.Error("the client completed a handshake with a server that has no TLS")
	}
	err := testsupport.Await(t, "the server handshake to end", handshook)
	if !errors.Is(err, errTLSNotLive) && (err == nil || !strings.Contains(err.Error(), errTLSNotLive.Error())) {
		t.Errorf("server handshake error = %v, want one that names errTLSNotLive", err)
	}
}

func TestHTTPSRuntimePinsAndHSTSFollowTheKind(t *testing.T) {
	rt := newHTTPSRuntime()

	src, _, err := newCertSource(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	rt.Install(tlsKindSelfSigned, src.GetCertificate)
	cert, _ := src.GetCertificate(nil)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.Pins(); len(got) != 1 || got[0] != authn.SPKIPin(leaf) || !strings.HasPrefix(got[0], "sha256/") {
		t.Errorf("self-signed Pins = %v, want exactly [%s]", got, authn.SPKIPin(leaf))
	}
	if rt.Pin() != authn.SPKIPin(leaf) || rt.Kind() != tlsKindSelfSigned {
		t.Errorf("self-signed Pin %q, Kind %v", rt.Pin(), rt.Kind())
	}
	if rt.HSTSAllowed() {
		t.Error("HSTS is allowed over the self-signed certificate (WR-09)")
	}

	supplied, _ := genCert(t, "a.example.com")
	rt.Install(tlsKindSupplied, getter(supplied))
	if rt.Pins() != nil || rt.Pin() != "" {
		t.Errorf("supplied Pins %v, Pin %q; want none", rt.Pins(), rt.Pin())
	}
	if !rt.HSTSAllowed() {
		t.Error("HSTS is not allowed over a supplied certificate")
	}

	rt.Remove()
	if rt.Pins() != nil || rt.HSTSAllowed() || rt.Live() || rt.Kind() != tlsKindNone {
		t.Errorf("after Remove: Pins %v, HSTSAllowed %v, Live %v, Kind %v; want none, false, false, none", rt.Pins(), rt.HSTSAllowed(), rt.Live(), rt.Kind())
	}
}

func TestCertSourceGetCertificateDoesNotWaitForARefresh(t *testing.T) {
	cert, _ := genCert(t, "a.example.com")
	entered, release := make(chan struct{}), make(chan struct{})
	src := &certSource{
		now:      nowFn(fixedNow),
		interval: time.Minute,
		refresh: func(time.Time, *x509.Certificate) (*tls.Certificate, error) {
			close(entered)
			<-release
			return nil, nil
		},
	}
	if err := src.set(cert, fixedNow); err != nil {
		t.Fatal(err)
	}

	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		src.Refresh(fixedNow.Add(time.Hour))
	}()
	testsupport.Await(t, "the refresh to start", entered)

	got := make(chan *tls.Certificate, 1)
	go func() {
		c, _ := src.GetCertificate(&tls.ClientHelloInfo{})
		got <- c
	}()
	if c := testsupport.Await(t, "a handshake to be answered while a refresh is blocked", got); c == nil || len(c.Certificate) == 0 || string(c.Certificate[0]) != string(cert.Certificate[0]) {
		t.Error("GetCertificate did not return the current certificate while a refresh was blocked")
	}

	close(release)
	testsupport.Await(t, "the refresh to finish", refreshed)
}

func TestCertRefreshLoopRefreshesOncePerIntervalAndStopsWithItsContext(t *testing.T) {
	cert, _ := genCert(t, "a.example.com")
	var calls atomic.Int32
	src := &certSource{
		now:      nowFn(fixedNow),
		interval: suppliedRecheck,
		refresh: func(time.Time, *x509.Certificate) (*tls.Certificate, error) {
			calls.Add(1)
			return nil, nil
		},
	}
	if err := src.set(cert, fixedNow); err != nil {
		t.Fatal(err)
	}

	clk := newStepClock(fixedNow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runCertRefresh(ctx, clk, src)
	}()

	clk.WaitArmed(t)
	if n := calls.Load(); n != 0 {
		t.Fatalf("refresh ran %d times before the first interval passed", n)
	}
	for want := int32(1); want <= 2; want++ {
		clk.Advance(suppliedRecheck)
		clk.WaitArmed(t) // the loop arms again once Refresh has run
		if n := calls.Load(); n != want {
			t.Fatalf("refresh ran %d times after %d intervals, want %d", n, want, want)
		}
	}

	cancel()
	testsupport.Await(t, "the refresh loop to stop with its context", stopped)
}
