// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package connect

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/wire"
)

// swappableServer is a fake TLS server whose certificate the test replaces while it runs, as
// TierPlan does when it switches between its self-signed certificate and a public one. A switch
// takes effect for the next handshake: dropAll ends the connections that are open, so the agent
// dials again and meets the new certificate.
type swappableServer struct {
	*fakeServer
	cert atomic.Pointer[tls.Certificate]

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// newSwappableServerTLS starts a fake server over TLS. The certificate it presents is set with
// serve, before the first agent dials. The TLS configuration comes from GetConfigForClient rather
// than GetCertificate, because httptest puts a certificate of its own in Certificates, and Go
// prefers that one when the client sends no server name, as an agent dialling an IP address does.
func newSwappableServerTLS(t *testing.T, script func(n int, a wire.Auth) reply) *swappableServer {
	t.Helper()
	s := &swappableServer{fakeServer: newFakeCore(script), conns: map[net.Conn]struct{}{}}
	s.ts = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		// A hijacked connection (the WebSocket) never reaches StateClosed and httptest stops
		// tracking it, so the server keeps its own list to be able to end it.
		s.mu.Lock()
		defer s.mu.Unlock()
		switch st {
		case http.StateNew:
			s.conns[c] = struct{}{}
		case http.StateClosed:
			delete(s.conns, c)
		default:
		}
	}
	s.ts.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{Certificates: []tls.Certificate{*s.cert.Load()}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, nil
		},
	}
	return s
}

// start serves cert and begins listening.
func (s *swappableServer) start(t *testing.T, cert *tls.Certificate) {
	t.Helper()
	s.cert.Store(cert)
	s.ts.StartTLS()
	t.Cleanup(func() {
		s.dropAll()
		s.ts.Close()
	})
}

// serve makes cert the certificate of every handshake from now on.
func (s *swappableServer) serve(cert *tls.Certificate) { s.cert.Store(cert) }

// dropAll closes every open connection, as a server restart does.
func (s *swappableServer) dropAll() {
	s.mu.Lock()
	open := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		open = append(open, c)
	}
	clear(s.conns)
	s.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

// awaitConnection waits for the agent's nth handshake and for its answer to the fence the script
// sent after the welcome. The welcome, and so the pins in it, has been handled by then.
func (s *swappableServer) awaitConnection(t *testing.T, n int) {
	t.Helper()
	s.authN(t, n)
	s.awaitFence(t)
}

// tlsOf is the certificate and key of c as a server presents them, followed by the rest of the chain.
func tlsOf(c certPair, rest ...certPair) tls.Certificate {
	ders := [][]byte{c.cert.Raw}
	for _, r := range rest {
		ders = append(ders, r.cert.Raw)
	}
	return tls.Certificate{Certificate: ders, PrivateKey: c.key}
}

// newSelfSigned is the self-signed identity of the server: a certificate that no root vouches for.
func newSelfSigned(t *testing.T) (certPair, tls.Certificate) {
	t.Helper()
	c := issue(t, "tierplan self-signed", false, nil)
	return c, tlsOf(c)
}

// newTestChain is a public-style chain for host: a root, an intermediate under it and a leaf that
// names host, all ECDSA P-256 and valid for a day from the real clock. Certificate validity is
// judged by the agent on the wall clock (verifyServer passes no CurrentTime), so only the pin
// deadline follows the test's clock. The chain is served as leaf plus intermediate, so a client
// holding only the root builds the path through the intermediate the server sends.
func newTestChain(t *testing.T, host string) (root *x509.Certificate, chain tls.Certificate) {
	t.Helper()
	rootPair := issue(t, "test root", true, nil)
	inter := issue(t, "test intermediate", true, &rootPair)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, inter.cert, &key.PublicKey, inter.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return rootPair.cert, tlsOf(certPair{cert: leaf, key: key}, inter)
}
