// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package authn

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
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
