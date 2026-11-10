// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync/atomic"

	"github.com/ianglennon/tierplan/internal/api"
)

// tlsKind says where the live certificate comes from. It decides whether a pin
// is published and whether HSTS may be sent.
type tlsKind uint8

const (
	// tlsKindSelfSigned is the certificate TierPlan generated. Agents pin its
	// key, and HSTS is never sent with it (WR-09). The kinds start at 1, so the
	// zero value is no kind at all and never reads as self-signed.
	tlsKindSelfSigned tlsKind = iota + 1
	// tlsKindSupplied is a certificate the operator provided or obtained. HSTS
	// is allowed over it and no pin is published.
	tlsKindSupplied
)

// liveTLS is one installed certificate source.
type liveTLS struct {
	kind tlsKind
	get  func(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// httpsRuntime is the live TLS state that every handshake, pin and HSTS
// decision reads. It holds one stable tls.Config whose GetCertificate looks at
// the current source, so a certificate can change while the server runs. TLS is
// never off: once assemble has installed a source every handshake is answered
// with a certificate. Nothing in it takes a lock or touches a file: the handshake
// answers from memory.
//
// Nothing but Build and tests calls Install (D-12): no setting, flag or
// environment variable reaches it.
type httpsRuntime struct {
	cur atomic.Pointer[liveTLS]
	cfg *tls.Config
}

// newHTTPSRuntime returns a runtime with nothing installed.
func newHTTPSRuntime() *httpsRuntime {
	r := &httpsRuntime{}
	r.cfg = &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		// GetCertificate answers every handshake and Certificates stays empty on
		// purpose: Go answers from a static Certificates list, and never calls
		// GetCertificate, for a client that sends no server name, such as one
		// that connects to an IP address. That would pin such a client to the
		// first certificate for ever (WR-09).
		GetCertificate: r.getCertificate,
	}
	return r
}

// Install makes get the source of every handshake that starts from now on. A
// connection already open keeps the certificate it was set up with.
func (r *httpsRuntime) Install(kind tlsKind, get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) {
	r.cur.Store(&liveTLS{kind: kind, get: get})
}

// Kind is the kind of the installed source, 0 when nothing is installed. That
// is only the window between newHTTPSRuntime and assemble's Install, or a test
// rig that never installs.
func (r *httpsRuntime) Kind() tlsKind {
	if l := r.cur.Load(); l != nil {
		return l.kind
	}
	return 0
}

// Config is the stable tls.Config. spkiPin and fingerprint read the certificate
// through it.
func (r *httpsRuntime) Config() *tls.Config { return r.cfg }

// ServerConfig is the config a TLS listener serves with. It is never nil.
func (r *httpsRuntime) ServerConfig() *tls.Config { return r.cfg }

// Pins is the SPKI pin list a welcome carries: the generated certificate's pin
// and nothing else. A supplied certificate publishes none.
func (r *httpsRuntime) Pins() []string {
	if r.Kind() != tlsKindSelfSigned {
		return nil
	}
	if p := spkiPin(r.cfg); p != "" {
		return []string{p}
	}
	return nil
}

// Pin is the pin an install command carries: the first of Pins, or "".
func (r *httpsRuntime) Pin() string {
	if p := r.Pins(); len(p) > 0 {
		return p[0]
	}
	return ""
}

// HSTSAllowed reports whether a response over TLS may carry
// Strict-Transport-Security: only with a supplied certificate, never with the
// generated one that a browser cannot be asked to trust for a year (D-04).
func (r *httpsRuntime) HSTSAllowed() bool { return r.Kind() == tlsKindSupplied }

// served is the certificate a browser receives now: the installed kind mapped
// to a source and the leaf parsed from what GetCertificate answers, never a
// stored copy. Nothing installed, or a certificate that cannot be parsed, gives
// a nil Leaf.
func (r *httpsRuntime) served() api.ServedCertificate {
	var s api.ServedCertificate
	switch r.Kind() {
	case tlsKindSelfSigned:
		s.Source = api.SourceSelfSigned
	case tlsKindSupplied:
		s.Source = api.SourceCertFiles
	default:
		return s
	}
	cert, err := r.cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || cert == nil || len(cert.Certificate) == 0 {
		return s
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return s
	}
	s.Leaf = leaf
	return s
}

func (r *httpsRuntime) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	l := r.cur.Load()
	if l == nil || l.get == nil {
		return nil, errors.New("no certificate is installed")
	}
	return l.get(hello)
}
