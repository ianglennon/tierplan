// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The reasons a served certificate is not trusted for the public host (D-10). They are the values the
// status publishes.
const (
	trustSelfSigned       = "self_signed"
	trustUnknownAuthority = "unknown_authority"
	trustExpired          = "expired"
	trustNameMismatch     = "name_mismatch"
	trustNoPublicURL      = "no_public_url"
)

// trustVerdict is whether the certificate TierPlan serves would be trusted, for the public URL's host,
// by a client that knows only the system roots, which is how an agent without a pin judges it. Checked
// is false when nothing is served. Verified and Reason are exclusive: a verified verdict has no reason.
type trustVerdict struct {
	Checked, Verified bool
	Reason, Host      string
}

// verifyPublicTrust judges chain, the certificates a server presents with the leaf first, for host at
// now. It uses the options the agent uses (agent/internal/connect/tls.go): the same server-auth usage,
// the host as DNSName, the presented certificates after the leaf as intermediates. roots nil means the
// system pool, which in the distroless image is /etc/ssl/certs/ca-certificates.crt. A leaf sent without
// its intermediates is therefore unknown_authority, as it is for an agent.
func verifyPublicTrust(chain []*x509.Certificate, host string, roots *x509.CertPool, now time.Time) trustVerdict {
	v := trustVerdict{Checked: true, Host: host}
	if host == "" {
		v.Reason = trustNoPublicURL
		return v
	}
	if len(chain) == 0 {
		v.Checked = false
		return v
	}
	leaf := chain[0]
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, DNSName: host, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err == nil {
		v.Verified = true
		return v
	}
	v.Reason = trustReason(leaf, err)
	return v
}

// trustReason names why Verify refused leaf.
func trustReason(leaf *x509.Certificate, err error) string {
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var unknown x509.UnknownAuthorityError
	switch {
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return trustExpired
	case errors.As(err, &hostname):
		return trustNameMismatch
	case errors.As(err, &unknown) && isSelfSigned(leaf):
		return trustSelfSigned
	default:
		return trustUnknownAuthority
	}
}

// isSelfSigned reports whether the certificate is its own issuer and signed by its own key. It checks
// the signature with the certificate's own key directly, because CheckSignatureFrom refuses a parent
// that is not marked as a CA, and a self-signed server certificate usually is not.
func isSelfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

// trustKey is what one verdict was computed for. The minute of the injected clock is in it, so an
// expiry or a public URL change is seen within a minute and one minute's requests verify once.
type trustKey struct {
	leaf   [sha256.Size]byte
	host   string
	minute time.Time
}

// trustChecker decides, from a served chain, the verdict that gates HSTS and the trust an install
// command states. It keeps the last result.
type trustChecker struct {
	roots *x509.CertPool
	now   func() time.Time
	host  func() string
	// verify is verifyPublicTrust. A test wraps it to count the real verifications.
	verify func(chain []*x509.Certificate, host string, roots *x509.CertPool, now time.Time) trustVerdict

	mu      sync.Mutex
	have    bool
	key     trustKey
	verdict trustVerdict
}

// newTrustChecker returns a checker that judges against roots (nil: the system roots) at now, for the
// host publicHost returns.
func newTrustChecker(roots *x509.CertPool, now func() time.Time, publicHost func() string) *trustChecker {
	return &trustChecker{roots: roots, now: now, host: publicHost, verify: verifyPublicTrust}
}

// verdictFor is the verdict for the certificate of kind that chain was read from. The self-signed
// certificate is never trusted by a client without its pin and is not verified. Nothing served is
// unchecked.
func (c *trustChecker) verdictFor(kind tlsKind, chain []*x509.Certificate) trustVerdict {
	host := c.host()
	switch {
	case kind == tlsKindSelfSigned:
		return trustVerdict{Checked: true, Reason: trustSelfSigned, Host: host}
	case kind == 0 || len(chain) == 0:
		return trustVerdict{}
	}
	now := c.now()
	key := trustKey{leaf: sha256.Sum256(chain[0].Raw), host: host, minute: now.Truncate(time.Minute)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && c.key == key {
		return c.verdict
	}
	c.verdict = c.verify(chain, host, c.roots, now)
	c.key, c.have = key, true
	return c.verdict
}

// hostOf is the lower-cased host name of a public URL, without its port, or "" when there is none.
func hostOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
