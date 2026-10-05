// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"slices"

	"github.com/ianglennon/bayplan/proto/authn"
)

// This file is the agent's only TLS configuration, and the only place in the
// repository that may switch Go's built-in certificate verification off
// (scripts/insecure-tls-check.sh, part of make lint, fails on any other file).
//
// The agent trusts a server in exactly three ways (D-06):
//
//  1. With no pin at all, the ordinary way: the certificate chains to a root
//     the system trusts and names the host the agent dialled.
//  2. A pin whose SPKI hash is the hash of the server's own certificate. A
//     self-signed Bayplan cannot be verified by a chain, so the pin is the
//     verification.
//  3. A pin from BAYPLAN_AGENT_PIN on a certificate authority in the chain the
//     server presents, for a private-CA proxy: the pinned certificate becomes the
//     only trust anchor and the server's certificate must verify against it, for
//     the host that was dialled. Merely sending the authority's certificate along
//     is not enough, because it is public.
//
// With pins set, a certificate that verifies against the system roots is still
// accepted. A pin never makes an agent distrust what any client would trust, so
// an administrator who moves Bayplan from its self-signed certificate to a public
// one does not strand the agents that stored the old pin.
//
// Go checks a self-signed certificate through VerifyConnection only when the
// built-in verification is off, so the pinned configuration turns it off and puts
// VerifyConnection, which is mandatory here and accepts only the three cases
// above, in its place. For wss:// there is no other way to a verified-by-pin
// connection and no setting, variable or error path that leaves a connection
// unverified. Plain ws:// is not TLS at all: it is dialed only to a loopback host,
// or to another host when BAYPLAN_AGENT_ALLOW_INSECURE is set on the agent, which
// is for development (T-02-31). Nothing learned over such a session is kept
// (pushes are taken only over a verified wss:// session).

const (
	// untrustedCode is the code an operator searches the agent's log for.
	untrustedCode = "tls_untrusted"
	// untrustedHint is what to do about it.
	untrustedHint = "Use a certificate from a public CA, or reinstall the agent with the command from Show install command. It carries the certificate pin."
)

// UntrustedError reports a server certificate the agent does not trust. It starts
// with the code so a log line or an error chain carries it.
type UntrustedError struct{ Detail string }

func (e *UntrustedError) Error() string { return untrustedCode + ": " + e.Detail }

// asUntrusted returns err as an UntrustedError when it is a failure to verify the
// server, whether from the pin check below or from Go's own verification, and nil
// for any other error.
func asUntrusted(err error) *UntrustedError {
	if err == nil {
		return nil
	}
	var u *UntrustedError
	if errors.As(err, &u) {
		return u
	}
	var cv *tls.CertificateVerificationError
	if errors.As(err, &cv) {
		return &UntrustedError{Detail: "the server's certificate is not trusted: " + cv.Err.Error()}
	}
	return nil
}

// tlsConfig is the configuration the agent dials with. pins are the pins stored
// from welcomes and installPins the ones from BAYPLAN_AGENT_PIN; roots replaces
// the system roots, for a test, and is nil in production. host is the name or
// address being dialled, the one a certificate must be valid for: the connection
// state does not carry it when the agent dials an IP address, so it is given here.
func tlsConfig(pins, installPins []string, roots *x509.CertPool, host string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: host}
	if len(pins) == 0 && len(installPins) == 0 {
		return cfg
	}
	// Go's own verification is switched off here, and VerifyConnection below
	// replaces it entirely: it runs on every connection and never returns nil for
	// a server that neither matches a pin nor verifies against a trusted root.
	cfg.InsecureSkipVerify = true //nolint:gosec // VerifyConnection replaces verification entirely: SPKI pin, pinned authority or system roots, else an error
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		return verifyServer(cs, pins, installPins, roots, host)
	}
	return cfg
}

// verifyServer accepts a connection only for the three reasons in the file
// comment and returns an *UntrustedError otherwise.
func verifyServer(cs tls.ConnectionState, pins, installPins []string, roots *x509.CertPool, host string) error {
	if len(cs.PeerCertificates) == 0 {
		return &UntrustedError{Detail: "the server presented no certificate"}
	}
	leaf := cs.PeerCertificates[0]
	leafPin := authn.SPKIPin(leaf)

	// The handshake has already shown that the server holds the leaf's key, so a
	// leaf that carries a pinned public key is the pinned server.
	if slices.Contains(pins, leafPin) || slices.Contains(installPins, leafPin) {
		return nil
	}

	// Without the name that was dialled, no chain can be checked.
	if host == "" {
		return &UntrustedError{Detail: "the server's certificate " + leafPin + " matches no pin"}
	}
	rest := cs.PeerCertificates[1:]
	intermediates := x509.NewCertPool()
	for _, c := range rest {
		intermediates.AddCert(c)
	}

	// A pinned authority in the presented chain is a trust anchor, and the leaf
	// must verify against it.
	for _, c := range rest {
		if !slices.Contains(installPins, authn.SPKIPin(c)) {
			continue
		}
		anchor := x509.NewCertPool()
		anchor.AddCert(c)
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: anchor, Intermediates: intermediates, DNSName: host,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err == nil {
			return nil
		}
	}

	// What any client would trust.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, DNSName: host,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return &UntrustedError{Detail: "the server's certificate " + leafPin + " matches no pin and is not trusted: " + err.Error()}
	}
	return nil
}

// newHTTPClient is the client the agent dials Bayplan with. It honours
// HTTPS_PROXY, HTTP_PROXY and NO_PROXY from the environment, and uses tlsConfig
// for host.
func newHTTPClient(pins, installPins []string, roots *x509.CertPool, host string) *http.Client {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = http.ProxyFromEnvironment
	tr.TLSClientConfig = tlsConfig(pins, installPins, roots, host)
	// A WebSocket upgrade is HTTP/1.1.
	tr.ForceAttemptHTTP2 = false
	return &http.Client{
		Transport: tr,
		// The certificate is checked for the host in BAYPLAN_AGENT_URL, so the
		// agent never follows a redirect to another one.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
