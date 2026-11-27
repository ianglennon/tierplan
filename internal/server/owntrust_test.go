// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	now    time.Time
	trust  *ownTrust
}

func newTrustRig(anchor *x509.Certificate, chain ...*x509.Certificate) *trustRig {
	r := &trustRig{chain: chain, anchor: anchor, now: fixedNow}
	r.trust = newOwnTrust(
		func() (tlsKind, []*x509.Certificate) { return tlsKindACME, r.chain },
		func() *x509.Certificate { return r.anchor },
		func() time.Time { return r.now },
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
	return slices.EqualFunc(got, want, (*x509.Certificate).Equal)
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

// has says whether the trust set holds c.
func (r *ownTrust) has(t *testing.T, c *x509.Certificate) bool {
	t.Helper()
	data, _ := r.PEM()
	return slices.ContainsFunc(pemCerts(t, data), c.Equal)
}

// serialsOf is the leaf serials of the PEM, for a failure message.
func serialsOf(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	for _, c := range pemCerts(t, data) {
		out = append(out, c.SerialNumber.String()[:6])
	}
	return out
}

// installer is an ACME installer over the self-signed source src, as Build wires it.
func installer(rt *httpsRuntime, src *certSource, dataDir string) *acmeInstaller {
	return &acmeInstaller{rt: rt, self: src, dataDir: dataDir, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// acmeChain is a chain and key a Let's Encrypt install would hand over, and the parsed leaf and issuer.
func acmeChain(t *testing.T, host string) (chainPEM, keyPEM []byte, leaf, issuer *x509.Certificate) {
	t.Helper()
	k, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := signCertificate(k, wantedNames("https://"+host, ""), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if keyPEM, err = encodeKey(k); err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	issuer = trustCert(t, "issuer."+host)
	chainPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw})...)
	return chainPEM, keyPEM, leaf, issuer
}

func TestOwnTrustFollowsEveryChange(t *testing.T) {
	// requireChange asserts that PEM moved to a new version, that the certificate now served is in
	// the set and that the one served before is still there, so no proxy is left with nothing to trust.
	requireChange := func(t *testing.T, tr *ownTrust, before int64, now, old *x509.Certificate) int64 {
		t.Helper()
		data, ver := tr.PEM()
		if ver == before {
			t.Fatalf("version stayed %d after the served certificate changed; PEM serials %v", ver, serialsOf(t, data))
		}
		got := pemCerts(t, data)
		if !slices.ContainsFunc(got, now.Equal) {
			t.Error("the certificate served now is not in the trust set")
		}
		if !slices.ContainsFunc(got, old.Equal) {
			t.Error("the certificate served before is not kept in the trust set")
		}
		return ver
	}

	t.Run("Let's Encrypt install and the fallback to self-signed", func(t *testing.T) {
		f := newSSFixture(t)
		src := f.source(t, "https://a.example.test", "", fixedNow)
		rt := newHTTPSRuntime()
		rt.Install(tlsKindSelfSigned, src.GetCertificate)
		now := fixedNow
		tr := newOwnTrust(rt.servedChain, src.Leaf, func() time.Time { return now })
		// The install hook runs with the source's lock held; it must not wait on that lock.
		rt.AddOnInstall(func() { tr.Observe() })
		inst := installer(rt, src, f.cfg.DataDir)
		self := src.Leaf()
		_, v0 := tr.PEM()
		if !tr.has(t, self) {
			t.Fatal("the self-signed leaf is not in the set")
		}

		chainPEM, keyPEM, le, issuer := acmeChain(t, "le.example.test")
		if err := inst.ServeACME(chainPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		v1 := requireChange(t, tr, v0, le, self)
		if !tr.has(t, issuer) {
			t.Error("the issuer of the served chain is not in the set")
		}

		// The fallback serves the anchor, which was in the set all along, and the Let's Encrypt chain
		// is kept as previous, so the set holds the same members in the same order and the content
		// fingerprint does not move: the proxy has nothing to catch up on, and no moment without a
		// trusted member. The version moves when the grace period ends and the chain drops out.
		if err := inst.ServeSelfSigned(); err != nil {
			t.Fatal(err)
		}
		if data, v := tr.PEM(); v != v1 {
			t.Errorf("version = %d after the fallback, want %d: the trust set holds the same certificates; PEM serials %v", v, v1, serialsOf(t, data))
		}
		if !tr.has(t, self) || !tr.has(t, le) || !tr.has(t, issuer) {
			t.Error("after the fallback the set must hold the anchor and the Let's Encrypt chain as previous")
		}
		now = fixedNow.Add(ownTrustGrace)
		requireChange(t, tr, v1, self, self)
		if tr.has(t, le) || tr.has(t, issuer) {
			t.Error("the Let's Encrypt chain is still in the set after the grace period")
		}
	})

	t.Run("self-signed re-signed for a new public URL with the same key", func(t *testing.T) {
		f := newSSFixture(t)
		public := "https://a.example.test"
		src, _, err := newCertSource(context.Background(), f.cfg, f.store, func() string { return public }, str(""), nowFn(fixedNow))
		if err != nil {
			t.Fatal(err)
		}
		rt := newHTTPSRuntime()
		rt.Install(tlsKindSelfSigned, src.GetCertificate)
		tr := newOwnTrust(rt.servedChain, src.Leaf, nowFn(fixedNow))
		old := src.Leaf()
		_, v0 := tr.PEM()

		// Nothing changed, and a Refresh that finds the names covered changes nothing either.
		src.Refresh(fixedNow.Add(selfSignedNameCheck))
		if _, v := tr.PEM(); v != v0 || tr.Observe() {
			t.Fatal("the version moved although the served certificate did not")
		}

		public = "https://new.example.test"
		src.Refresh(fixedNow.Add(2 * selfSignedNameCheck))
		now := src.Leaf()
		if now.Equal(old) {
			t.Fatal("the source did not sign a new certificate")
		}
		// The re-sign installs nothing, so no hook runs: the poll is what sees it.
		requireChange(t, tr, v0, now, old)
	})

	t.Run("supplied files replaced", func(t *testing.T) {
		dir := t.TempDir()
		crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
		a := writePair(t, crt, key, "a.example.com")
		src, err := newSuppliedSource(crt, key, nowFn(fixedNow))
		if err != nil {
			t.Fatal(err)
		}
		rt := newHTTPSRuntime()
		rt.Install(tlsKindSupplied, src.GetCertificate)
		anchor := trustCert(t, "self.example.test")
		tr := newOwnTrust(rt.servedChain, func() *x509.Certificate { return anchor }, nowFn(fixedNow))
		_, v0 := tr.PEM()
		if !tr.has(t, a) || !tr.has(t, anchor) {
			t.Fatal("the supplied leaf and the self-signed anchor must both be in the set")
		}

		b := writePair(t, crt, key, "b.example.com")
		src.Refresh(fixedNow.Add(2 * suppliedRecheck))
		requireChange(t, tr, v0, b, a)
		if !tr.has(t, anchor) {
			t.Error("the self-signed anchor left the set")
		}
	})

	t.Run("a Refresh that changes nothing leaves the version", func(t *testing.T) {
		f := newSSFixture(t)
		src := f.source(t, "https://a.example.test", "", fixedNow)
		rt := newHTTPSRuntime()
		rt.Install(tlsKindSelfSigned, src.GetCertificate)
		tr := newOwnTrust(rt.servedChain, src.Leaf, nowFn(fixedNow))
		_, v0 := tr.PEM()
		for i := 1; i <= 3; i++ {
			src.Refresh(fixedNow.Add(time.Duration(i) * selfSignedNameCheck))
		}
		if _, v := tr.PEM(); v != v0 {
			t.Errorf("version = %d after refreshes that changed nothing, want %d", v, v0)
		}
	})
}

func TestOwnTrustKeepsThePreviousChainForAGracePeriod(t *testing.T) {
	s, a, b := trustCert(t, "self.example.test"), trustCert(t, "a.example.test"), trustCert(t, "b.example.test")
	r := newTrustRig(s, a)
	_, v0 := r.trust.PEM()

	r.chain = []*x509.Certificate{b}
	_, v1 := r.trust.PEM()
	if v1 == v0 || !r.trust.has(t, a) || !r.trust.has(t, b) || !r.trust.has(t, s) {
		t.Fatal("after the change the set must hold the anchor, the new chain and the previous chain")
	}

	r.now = fixedNow.Add(ownTrustGrace - time.Second)
	if _, v := r.trust.PEM(); v != v1 || !r.trust.has(t, a) {
		t.Error("the previous chain left before its grace period ended")
	}

	r.now = fixedNow.Add(ownTrustGrace)
	data, v2 := r.trust.PEM()
	if got := pemCerts(t, data); !sameCerts(got, s, b) {
		t.Errorf("after the grace period the PEM holds %d certificates, want the anchor and the served chain only", len(got))
	}
	if v2 == v1 {
		t.Error("the version did not change when the previous chain dropped out")
	}
	if _, v3 := r.trust.PEM(); v3 != v2 {
		t.Error("the version changed again with nothing new")
	}
}

func TestOnInstallStillReachesHSTS(t *testing.T) {
	rt := newHTTPSRuntime()
	s := trustCert(t, "self.example.test")
	tr := newOwnTrust(rt.servedChain, func() *x509.Certificate { return s }, nowFn(fixedNow))
	var order []string
	rt.SetOnInstall(func() { order = append(order, "hsts") }) // what Build registers: hsts.Wake
	rt.AddOnInstall(func() { order = append(order, "owntrust"); tr.Observe() })
	rt.AddOnInstall(nil) // ignored

	le := trustCert(t, "le.example.test")
	rt.Install(tlsKindACME, getter(tlsCertOf(le)))
	if !slices.Equal(order, []string{"hsts", "owntrust"}) {
		t.Errorf("one Install called %v, want the HSTS hook and then ownTrust's, once each", order)
	}
	if !tr.has(t, le) {
		t.Error("ownTrust did not see the installed certificate")
	}

	// SetOnInstall still replaces the hook, and nil removes it.
	order = nil
	rt.SetOnInstall(nil)
	rt.Install(tlsKindACME, getter(tlsCertOf(le)))
	if len(order) != 0 {
		t.Errorf("hooks %v ran after SetOnInstall(nil)", order)
	}
}

// tlsCertOf is a tls.Certificate that serves only c's DER, enough for servedChain.
func tlsCertOf(c *x509.Certificate) tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{c.Raw}}
}
