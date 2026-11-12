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
	// is allowed over it. No install command carries a pin for it, and a welcome
	// still lists the install's self-signed pin (D-19).
	tlsKindSupplied
	// tlsKindACME is a Let's Encrypt certificate TierPlan obtained and renews itself. No install
	// command carries a pin (the certificate chains to the public roots). A welcome still lists the
	// self-signed pin, so agents keep the key they were installed with and never start a PinOverlap
	// countdown on it (D-19). HSTS is not sent in this phase.
	tlsKindACME
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
// Only Build and the ACME installer (acmeserve.go) call Install (D-12): no setting,
// flag or environment variable reaches it.
type httpsRuntime struct {
	cur atomic.Pointer[liveTLS]
	cfg *tls.Config
	// selfPin is the SPKI pin of the install's self-signed identity, settled once by Build. It is
	// never read from the served certificate, so a Let's Encrypt leaf's pin is never published and
	// a renewal never changes what welcomes list (D-19).
	selfPin atomic.Pointer[string]
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

// SetSelfSignedPin records the pin of the install's self-signed identity. Build calls it once,
// before any goroutine starts: the key is never rotated while the server runs (13.1 D-12).
func (r *httpsRuntime) SetSelfSignedPin(pin string) {
	if pin == "" {
		r.selfPin.Store(nil)
		return
	}
	r.selfPin.Store(&pin)
}

// WelcomePins is the SPKI pin list a welcome, a control-stream catch-up and an address push carry:
// the self-signed identity's pin, whatever certificate is served, so no agent sees its pin
// disappear and starts a PinOverlap countdown (D-19). A runtime that was never given a pin and
// serves the self-signed kind falls back to the served certificate's pin. Otherwise it is nil.
func (r *httpsRuntime) WelcomePins() []string {
	if p := r.selfPin.Load(); p != nil {
		return []string{*p}
	}
	if r.Kind() != tlsKindSelfSigned {
		return nil
	}
	if p := spkiPin(r.cfg); p != "" {
		return []string{p}
	}
	return nil
}

// Pin is the pin an install, rotate or manual-update command carries: the self-signed pin while
// the self-signed certificate is served, and "" under Let's Encrypt or cert files, whose
// certificates chain to a root the host already trusts or are the operator's own (TLS-08, D-18).
func (r *httpsRuntime) Pin() string {
	if r.Kind() != tlsKindSelfSigned {
		return ""
	}
	if p := r.WelcomePins(); len(p) > 0 {
		return p[0]
	}
	return ""
}

// HSTSAllowed reports whether a response over TLS may carry
// Strict-Transport-Security: only with a supplied certificate, never with the
// generated one that a browser cannot be asked to trust for a year (D-04).
func (r *httpsRuntime) HSTSAllowed() bool { return r.Kind() == tlsKindSupplied }

// servedChain is the installed kind and the certificates a handshake would present, leaf first, parsed
// from what GetCertificate answers and never from a stored copy. Both come from one read of the
// installed source, so a swap while the caller works never pairs one source's kind with another's
// chain. The chain is nil when nothing is installed or the certificate cannot be read; a certificate
// after the leaf that cannot be parsed is left out.
func (r *httpsRuntime) servedChain() (tlsKind, []*x509.Certificate) {
	l := r.cur.Load()
	if l == nil || l.get == nil {
		return 0, nil
	}
	cert, err := l.get(&tls.ClientHelloInfo{})
	if err != nil || cert == nil || len(cert.Certificate) == 0 {
		return l.kind, nil
	}
	chain := make([]*x509.Certificate, 0, len(cert.Certificate))
	for i, der := range cert.Certificate {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			if i == 0 {
				return l.kind, nil
			}
			continue
		}
		chain = append(chain, c)
	}
	return l.kind, chain
}

// agentTrust is how an agent installed now trusts the server, read from one read of the served
// certificate (pin, public or unverified).
func (r *httpsRuntime) agentTrust(tc *trustChecker) string {
	kind, chain := r.servedChain()
	return tc.AgentTrust(kind, chain)
}

// served is the certificate a browser receives now: the installed kind mapped to a source and the
// leaf parsed from what GetCertificate answers. Nothing installed, or a certificate that cannot be
// parsed, gives a nil Leaf.
func (r *httpsRuntime) served() api.ServedCertificate { return r.servedWith(nil) }

// servedWith is served with the trust verdict of the same read attached, so the certificate a status
// shows and the verdict beside it can never come from two different certificates (SET-01). A nil
// checker leaves the verdict unchecked.
func (r *httpsRuntime) servedWith(tc *trustChecker) api.ServedCertificate {
	kind, chain := r.servedChain()
	var s api.ServedCertificate
	switch kind {
	case tlsKindSelfSigned:
		s.Source = api.SourceSelfSigned
	case tlsKindSupplied:
		s.Source = api.SourceCertFiles
	case tlsKindACME:
		s.Source = api.SourceLetsEncrypt
	default:
		return s
	}
	if len(chain) == 0 {
		return s
	}
	s.Leaf = chain[0]
	if tc != nil {
		v := tc.verdictFor(kind, chain)
		s.Trust = api.CertTrust{Checked: v.Checked, Verified: v.Verified, Reason: v.Reason, Host: v.Host}
	}
	return s
}

func (r *httpsRuntime) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	l := r.cur.Load()
	if l == nil || l.get == nil {
		return nil, errors.New("no certificate is installed")
	}
	return l.get(hello)
}
