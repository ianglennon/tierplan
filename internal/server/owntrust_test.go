// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// trustCert is a fresh certificate for host, generated here and unrelated to any other.
func trustCert(t *testing.T, host string) *x509.Certificate {
	t.Helper()
	k, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := signCertificate(k, wantedNames("https://"+host, ""), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// trustRig is an ownTrust over a served chain and an anchor the test changes by hand.
type trustRig struct {
	chain  []*x509.Certificate
	anchor *x509.Certificate
	trust  *ownTrust
}

func newTrustRig(anchor *x509.Certificate, chain ...*x509.Certificate) *trustRig {
	r := &trustRig{chain: chain, anchor: anchor}
	r.trust = newOwnTrust(
		func() (tlsKind, []*x509.Certificate) { return tlsKindACME, r.chain },
		func() *x509.Certificate { return r.anchor },
		nowFn(fixedNow),
	)
	return r
}

// pemCerts decodes every block of data, failing on anything that is not a certificate.
func pemCerts(t *testing.T, data []byte) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for len(data) > 0 {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			t.Fatalf("PEM block of type %q, want only CERTIFICATE", b.Type)
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatalf("a block does not parse as a certificate: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func sameCerts(got []*x509.Certificate, want ...*x509.Certificate) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			return false
		}
	}
	return true
}

func TestOwnTrustPEMOrderAndVersion(t *testing.T) {
	s, l, i := trustCert(t, "self.example.test"), trustCert(t, "le.example.test"), trustCert(t, "issuer.example.test")

	r := newTrustRig(s, l, i)
	data, ver := r.trust.PEM()
	if got := pemCerts(t, data); !sameCerts(got, s, l, i) {
		t.Errorf("PEM holds %d certificates, want the anchor, then the served leaf, then its issuer, in that order", len(got))
	}
	if ver <= 0 {
		t.Errorf("version = %d, want a positive number", ver)
	}

	// The same certificates give the same version, as a restart would.
	_, again := newTrustRig(s, l, i).trust.PEM()
	if again != ver {
		t.Errorf("version = %d for the same certificates, want %d", again, ver)
	}

	// Serving the anchor alone lists it once.
	solo := newTrustRig(s, s)
	data, soloVer := solo.trust.PEM()
	if got := pemCerts(t, data); !sameCerts(got, s) {
		t.Errorf("PEM holds %d certificates when the anchor is the served leaf, want it once", len(got))
	}
	if soloVer == ver {
		t.Error("two different sets share one version")
	}
}

func TestOwnTrustHoldsCertificatesOnly(t *testing.T) {
	s, l := trustCert(t, "self.example.test"), trustCert(t, "le.example.test")
	data, _ := newTrustRig(s, l).trust.PEM()
	if strings.Contains(string(data), "PRIVATE KEY") {
		t.Fatal("the trust PEM carries a private key")
	}
	if n := strings.Count(string(data), "-----BEGIN "); n != 2 {
		t.Errorf("PEM has %d blocks, want 2", n)
	}
	pemCerts(t, data) // every block is a CERTIFICATE that parses
}
