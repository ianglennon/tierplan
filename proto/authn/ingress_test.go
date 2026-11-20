// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package authn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

func ingressInputs() (mode string, proto int, sn, nn []byte, aud string, pub, ekm []byte) {
	return "connect", 1, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), "tierplan.example:7480",
		bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)
}

// ingressGolden is the digest of ingressInputs. The ingress transcript is frozen
// with the first ingress release (design correction 22, D-07): a failure here is
// a change of the format and needs a new version, never a refreshed literal.
const ingressGolden = "b7a2d8492a3ecd06512ff0bfc49b6ec6000f639435b54f320b65e52f7e7f5b48"

func TestIngressTranscriptGoldenVector(t *testing.T) {
	mode, proto, sn, nn, aud, pub, ekm := ingressInputs()
	got := IngressTranscript(mode, proto, sn, nn, aud, pub, ekm)
	if len(got) != 32 {
		t.Fatalf("an ingress transcript is a SHA-256 digest, got %d bytes", len(got))
	}
	if hex.EncodeToString(got) != ingressGolden {
		t.Fatalf("ingress transcript is %x, the pinned vector is %s", got, ingressGolden)
	}
}

func TestIngressSignatureRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mode, proto, sn, nn, aud, _, ekm := ingressInputs()
	tr := IngressTranscript(mode, proto, sn, nn, aud, pub, ekm)
	sig := Sign(priv, tr)
	if !Verify(pub, tr, sig) {
		t.Fatal("a signature does not verify over its own ingress transcript")
	}
	changed := bytes.Clone(nn)
	changed[0] ^= 0xff
	if Verify(pub, IngressTranscript(mode, proto, sn, changed, aud, pub, ekm), sig) {
		t.Fatal("a signature verified after one input byte changed")
	}
}

func TestIngressTranscriptFieldBoundaries(t *testing.T) {
	mode, proto, sn, nn, aud, pub, ekm := ingressInputs()
	base := IngressTranscript(mode, proto, sn, nn, aud, pub, ekm)
	flip := func(b []byte) []byte { c := bytes.Clone(b); c[0] ^= 0xff; return c }
	for name, got := range map[string][]byte{
		"mode":        IngressTranscript("enrol", proto, sn, nn, aud, pub, ekm),
		"proto":       IngressTranscript(mode, 2, sn, nn, aud, pub, ekm),
		"serverNonce": IngressTranscript(mode, proto, flip(sn), nn, aud, pub, ekm),
		"nodeNonce":   IngressTranscript(mode, proto, sn, flip(nn), aud, pub, ekm),
		"audience":    IngressTranscript(mode, proto, sn, nn, "evil.example:7480", pub, ekm),
		"key bytes":   IngressTranscript(mode, proto, sn, nn, aud, flip(pub), ekm),
		"ekm":         IngressTranscript(mode, proto, sn, nn, aud, pub, flip(ekm)),
		"ekm absent":  IngressTranscript(mode, proto, sn, nn, aud, pub, nil),
	} {
		if bytes.Equal(got, base) {
			t.Errorf("changing the %s did not change the ingress transcript", name)
		}
	}

	// Moving a byte from one field to the next must not give the same digest.
	a := IngressTranscript("m", 1, []byte{1, 2}, []byte{3}, "x", nil, nil)
	b := IngressTranscript("m", 1, []byte{1}, []byte{2, 3}, "x", nil, nil)
	if bytes.Equal(a, b) {
		t.Fatal("two different field splits give the same ingress transcript")
	}

	// Two oversized fields that differ differ in the digest, and an oversized
	// field does not collide with a short one.
	long1 := bytes.Repeat([]byte{9}, maxField+10)
	long2 := bytes.Repeat([]byte{8}, maxField+10)
	if bytes.Equal(IngressTranscript("m", 1, long1, nil, "x", nil, nil), IngressTranscript("m", 1, long2, nil, "x", nil, nil)) {
		t.Fatal("two different oversized fields collided")
	}
	if bytes.Equal(IngressTranscript("m", 1, long1, nil, "x", nil, nil), IngressTranscript("m", 1, long1[:100], nil, "x", nil, nil)) {
		t.Fatal("an oversized field collided with a short one")
	}
}

func TestIngressAndAgentTranscriptsNeverCrossVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, proto, sn, nn, aud, _, ekm := ingressInputs()
	for _, mode := range []string{"enrol", "connect"} {
		agent := Transcript(mode, proto, sn, nn, aud, pub, ekm)
		ingress := IngressTranscript(mode, proto, sn, nn, aud, pub, ekm)
		if bytes.Equal(agent, ingress) {
			t.Fatalf("mode %s: the agent and ingress transcripts are the same digest", mode)
		}
		agentSig, ingressSig := Sign(priv, agent), Sign(priv, ingress)
		if !Verify(pub, agent, agentSig) || !Verify(pub, ingress, ingressSig) {
			t.Fatalf("mode %s: a signature does not verify over its own transcript", mode)
		}
		if Verify(pub, ingress, agentSig) {
			t.Errorf("mode %s: an agent signature verified at the ingress endpoint", mode)
		}
		if Verify(pub, agent, ingressSig) {
			t.Errorf("mode %s: an ingress signature verified at the agent endpoint", mode)
		}
	}
}

func TestIngressEKMLabelDiffersFromTheAgentLabel(t *testing.T) {
	if IngressEKMLabel != "EXPORTER-tierplan-ingress-auth-v1" {
		t.Fatalf("IngressEKMLabel %q", IngressEKMLabel)
	}
	if IngressEKMLabel == EKMLabel {
		t.Fatal("the ingress and agent exporter labels are the same")
	}
	if !strings.HasPrefix(IngressEKMLabel, "EXPORTER-") || !strings.HasPrefix(EKMLabel, "EXPORTER-") {
		t.Fatalf("an exporter label starts with EXPORTER-: %q, %q", IngressEKMLabel, EKMLabel)
	}
}

// tlsPair completes a TLS 1.3 handshake over net.Pipe, with the client verifying
// the server's in-memory self-signed certificate against a pool holding it, and
// returns both ends' connection state.
func tlsPair(t *testing.T) (client, server tls.ConnectionState) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tierplan.test"},
		DNSNames:  []string{"tierplan.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	cc, sc := net.Pipe()
	t.Cleanup(func() { _ = cc.Close(); _ = sc.Close() })
	srv := tls.Server(sc, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	cli := tls.Client(cc, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "tierplan.test"})

	ctx := testsupport.Context(t)
	done := make(chan error, 1)
	go func() { done <- srv.HandshakeContext(ctx) }()
	if err := cli.HandshakeContext(ctx); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := testsupport.Await(t, "the server handshake", done); err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return cli.ConnectionState(), srv.ConnectionState()
}

func TestEKMForExportsPerLabel(t *testing.T) {
	c, s := tlsPair(t)

	ingressC, ingressS := EKMFor(&c, IngressEKMLabel), EKMFor(&s, IngressEKMLabel)
	if len(ingressC) != EKMLength || len(ingressS) != EKMLength {
		t.Fatalf("exported %d and %d bytes, want %d", len(ingressC), len(ingressS), EKMLength)
	}
	if !EKMEqual(ingressC, ingressS) {
		t.Fatal("the two ends of one connection export different ingress material")
	}
	agentC, agentS := EKMFor(&c, EKMLabel), EKMFor(&s, EKMLabel)
	if !EKMEqual(agentC, agentS) {
		t.Fatal("the two ends of one connection export different agent material")
	}
	if bytes.Equal(ingressC, agentC) {
		t.Fatal("the ingress and agent labels export the same material")
	}
	if !bytes.Equal(agentC, EKM(&c)) {
		t.Fatal("EKMFor with the agent label is not EKM")
	}
	if EKMFor(nil, IngressEKMLabel) != nil {
		t.Fatal("EKMFor of no connection should be nil")
	}

	// A connection whose handshake has not run cannot export: the failure is nil,
	// never a panic and never material.
	cc, sc := net.Pipe()
	t.Cleanup(func() { _ = cc.Close(); _ = sc.Close() })
	fresh := tls.Client(cc, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "tierplan.test"}).ConnectionState()
	if EKMFor(&fresh, IngressEKMLabel) != nil {
		t.Fatal("EKMFor of a connection that has not handshaken should be nil")
	}
}
