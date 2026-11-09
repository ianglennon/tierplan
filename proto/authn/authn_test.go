// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package authn

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

func inputs() (mode string, proto int, sn, an []byte, aud string, pub, ekm []byte) {
	return "enrol", 1, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), "tierplan.example:443",
		bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)
}

func TestTranscriptIsDeterministic(t *testing.T) {
	mode, proto, sn, an, aud, pub, ekm := inputs()
	a := Transcript(mode, proto, sn, an, aud, pub, ekm)
	b := Transcript(mode, proto, sn, an, aud, pub, ekm)
	if !bytes.Equal(a, b) || len(a) != 32 {
		t.Fatalf("transcripts differ or are not a SHA-256 digest: %x %x", a, b)
	}
}

func TestChangingAnySingleFieldChangesTheTranscript(t *testing.T) {
	mode, proto, sn, an, aud, pub, ekm := inputs()
	base := Transcript(mode, proto, sn, an, aud, pub, ekm)
	flip := func(b []byte) []byte { c := bytes.Clone(b); c[0] ^= 0xff; return c }
	variants := map[string][]byte{
		"mode":        Transcript("hello", proto, sn, an, aud, pub, ekm),
		"proto":       Transcript(mode, 2, sn, an, aud, pub, ekm),
		"serverNonce": Transcript(mode, proto, flip(sn), an, aud, pub, ekm),
		"agentNonce":  Transcript(mode, proto, sn, flip(an), aud, pub, ekm),
		"audience":    Transcript(mode, proto, sn, an, "evil.example:443", pub, ekm),
		"key bytes":   Transcript(mode, proto, sn, an, aud, flip(pub), ekm),
		"ekm":         Transcript(mode, proto, sn, an, aud, pub, flip(ekm)),
		"ekm absent":  Transcript(mode, proto, sn, an, aud, pub, nil),
	}
	for name, got := range variants {
		if bytes.Equal(got, base) {
			t.Errorf("changing the %s did not change the transcript", name)
		}
	}
}

func TestFieldBoundariesAreNotAmbiguous(t *testing.T) {
	// Moving a byte from one field to the next must not produce the same digest.
	a := Transcript("m", 1, []byte{1, 2}, []byte{3}, "x", nil, nil)
	b := Transcript("m", 1, []byte{1}, []byte{2, 3}, "x", nil, nil)
	if bytes.Equal(a, b) {
		t.Fatal("two different field splits give the same transcript")
	}
}

func TestAnOversizedFieldDoesNotCollideWithAShortOne(t *testing.T) {
	long := bytes.Repeat([]byte{9}, maxField+10)
	a := Transcript("m", 1, long, nil, "x", nil, nil)
	b := Transcript("m", 1, long[:100], nil, "x", nil, nil)
	if bytes.Equal(a, b) {
		t.Fatal("an oversized field collided with a short one")
	}
}

func TestASignatureMadeForOneAudienceDoesNotVerifyForAnother(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mode, proto, sn, an, _, _, ekm := inputs()
	good := Transcript(mode, proto, sn, an, "tierplan.example:443", pub, ekm)
	sig := Sign(priv, good)
	if !Verify(pub, good, sig) {
		t.Fatal("a signature does not verify over its own transcript")
	}
	other := Transcript(mode, proto, sn, an, "other.example:443", pub, ekm)
	if Verify(pub, other, sig) {
		t.Fatal("a signature made over one audience verified over another")
	}
}

func TestVerifyRejectsMalformedInputWithoutPanicking(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr := Transcript("enrol", 1, nil, nil, "h", pub, nil)
	sig := Sign(priv, tr)
	for name, c := range map[string]struct{ pub, sig []byte }{
		"short key":       {pub[:10], sig},
		"empty key":       {nil, sig},
		"short signature": {pub, sig[:10]},
		"empty signature": {pub, nil},
	} {
		if Verify(c.pub, tr, c.sig) {
			t.Errorf("%s verified", name)
		}
	}
}

func TestSPKIPinHasTheDocumentedShapeAndFollowsTheKeyNotTheCertificate(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	issue := func(serial int64) *x509.Certificate {
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "tierplan"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, second := issue(1), issue(2)
	pin := SPKIPin(first)
	if !strings.HasPrefix(pin, "sha256/") || len(pin) != len("sha256/")+44 {
		t.Fatalf("pin %q does not look like sha256/<base64 of 32 bytes>", pin)
	}
	if SPKIPin(second) != pin {
		t.Fatal("re-issuing a certificate over the same key changed the pin")
	}
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, otherPub, otherPriv)
	c, _ := x509.ParseCertificate(der)
	if SPKIPin(c) == pin {
		t.Fatal("a different key has the same pin")
	}
}

func TestEKMEqualNeverMatchesAbsentMaterial(t *testing.T) {
	if EKMEqual(nil, nil) || EKMEqual([]byte{}, []byte{}) {
		t.Fatal("no binding must not count as a match")
	}
	if !EKMEqual([]byte{1, 2}, []byte{1, 2}) || EKMEqual([]byte{1, 2}, []byte{1, 3}) {
		t.Fatal("EKMEqual compares values wrongly")
	}
	if EKM(nil) != nil {
		t.Fatal("EKM of no connection should be nil")
	}
}

func TestAudienceIsTheSameForAWebSocketURLAndThePublicURLItCameFrom(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"wss://TierPlan.Example/agent/connect", "tierplan.example"},
		{"wss://tierplan.example:443/base/agent/connect", "tierplan.example"},
		{"https://tierplan.example", "tierplan.example"},
		{"https://tierplan.example:8443/base", "tierplan.example:8443"},
		{"ws://127.0.0.1:7480/agent/connect", "127.0.0.1:7480"},
		{"http://127.0.0.1:80", "127.0.0.1"},
		{"https://[::1]:8443", "[::1]:8443"},
	} {
		got, err := Audience(c.in)
		if err != nil || got != c.want {
			t.Errorf("Audience(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := Audience("not a url"); err == nil {
		t.Error("a string with no host has an audience")
	}
	if _, err := Audience(""); err == nil {
		t.Error("an empty URL has an audience")
	}
}

func TestTheExporterLabelAndLengthAreFixedByTheProtocol(t *testing.T) {
	// Deployed agents compute the binding with these two values; changing either
	// is a new transcript version, not an edit (D-05).
	if EKMLabel != "EXPORTER-tierplan-agent-auth-v1" || EKMLength != 32 {
		t.Fatalf("EKMLabel %q, EKMLength %d", EKMLabel, EKMLength)
	}
}
