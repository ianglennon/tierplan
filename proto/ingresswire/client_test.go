// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

const testAud = "tierplan.example.org:443"

func testChallenge(t *testing.T) Challenge {
	t.Helper()
	n, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return Challenge{T: TypeChallenge, Proto: ProtoVersion, Nonce: n, TS: 1_700_000_000}
}

func testEKM(b byte) []byte { return bytes.Repeat([]byte{b}, authn.EKMLength) }

func testBundle(t *testing.T) ingressbundle.Bundle {
	t.Helper()
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 5},
		[]ingressbundle.File{
			{Path: ingressbundle.DynamicFile, Data: []byte("http: {}\n")},
			{Path: ingressbundle.CertsDir + "/a.key", Data: []byte("key")},
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testEnvelope(t *testing.T, instance string, b ingressbundle.Bundle) []byte {
	t.Helper()
	data, err := json.Marshal(BundleEnvelope{T: TypeBundle, InstanceID: instance, Bundle: b})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// serverRebuild is what the server does: the transcript of the challenge nonce it
// sent, the auth's own fields and the given exporter.
func serverRebuild(ch Challenge, a Auth, ekm []byte) []byte {
	return authn.IngressTranscript(a.Mode, a.Proto, ch.Nonce, a.Nonce, a.Aud, a.Pub, ekm)
}

func TestNewNodeKeySignsAVerifiableEnrolment(t *testing.T) {
	k1, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != ed25519.PrivateKeySize || len(k2) != ed25519.PrivateKeySize {
		t.Fatalf("key sizes %d and %d, want %d", len(k1), len(k2), ed25519.PrivateKeySize)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("two NewNodeKey calls returned the same key")
	}
	ch := testChallenge(t)
	ekm := testEKM(7)
	a, err := EnrolAuth(ch, EnrolOptions{Token: TokenPrefix + "env_secret", Name: "n", SwarmNodeID: "abc", Version: "0.2.0", Aud: testAud, Key: k1, EKM: ekm, Now: time.Unix(1_700_000_001, 0)})
	if err != nil {
		t.Fatal(err)
	}
	pub1 := k1.Public().(ed25519.PublicKey)
	if !bytes.Equal(a.Pub, pub1) {
		t.Fatal("Auth.Pub is not the key's public half")
	}
	if !authn.Verify(pub1, serverRebuild(ch, a, a.EKM), a.Sig) {
		t.Fatal("the server's rebuild does not verify the enrolment")
	}
	pub2 := k2.Public().(ed25519.PublicKey)
	if authn.Verify(pub2, serverRebuild(ch, a, a.EKM), a.Sig) {
		t.Fatal("the second key's public half verified the first key's signature")
	}
}

func TestEnrolAuthVerifiesAgainstTheServersRebuild(t *testing.T) {
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	ch := testChallenge(t)
	ekm := testEKM(9)
	now := time.Unix(1_700_000_042, 0)
	a, err := EnrolAuth(ch, EnrolOptions{Token: TokenPrefix + "env_secret", Name: "edge-1", SwarmNodeID: "swarmid1", Version: "0.2.0", Aud: testAud, Key: key, EKM: ekm, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if a.T != TypeAuth || a.Mode != ModeEnrol || a.Proto != ProtoVersion {
		t.Fatalf("header = %q %q %d", a.T, a.Mode, a.Proto)
	}
	if a.Token != TokenPrefix+"env_secret" || a.Name != "edge-1" || a.SwarmNodeID != "swarmid1" || a.NodeVersion != "0.2.0" || a.Aud != testAud || a.NodeID != "" {
		t.Fatalf("fields = %+v", a)
	}
	if a.TS != now.Unix() {
		t.Fatalf("TS = %d, want %d", a.TS, now.Unix())
	}
	if len(a.Nonce) != 32 {
		t.Fatalf("nonce is %d bytes", len(a.Nonce))
	}
	if !bytes.Equal(a.EKM, ekm) {
		t.Fatal("Auth.EKM is not o.EKM byte for byte")
	}
	pub := key.Public().(ed25519.PublicKey)

	// Trusted proxy: the server folds in the reported EKM field.
	if !authn.Verify(a.Pub, serverRebuild(ch, a, a.EKM), a.Sig) {
		t.Fatal("rebuild with the reported EKM was refused")
	}
	// Direct peer: the server substitutes its own, equal exporter bytes.
	own := append([]byte(nil), ekm...)
	if !authn.Verify(pub, serverRebuild(ch, a, own), a.Sig) {
		t.Fatal("rebuild with the server's own equal exporter was refused")
	}
	if authn.Verify(pub, serverRebuild(ch, a, testEKM(1)), a.Sig) {
		t.Fatal("rebuild with another exporter verified")
	}
	if authn.Verify(pub, serverRebuild(ch, a, nil), a.Sig) {
		t.Fatal("rebuild with no exporter verified")
	}
	agent := authn.Transcript(a.Mode, a.Proto, ch.Nonce, a.Nonce, a.Aud, a.Pub, a.EKM)
	if authn.Verify(pub, agent, a.Sig) {
		t.Fatal("the agent transcript verified an ingress signature")
	}
}

func TestEnrolAuthRefusesBadInput(t *testing.T) {
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	ch := testChallenge(t)
	good := EnrolOptions{Token: TokenPrefix + "env_secret", Name: "n", SwarmNodeID: "abc", Aud: testAud, Key: key, EKM: testEKM(2), Now: time.Now()}
	cases := map[string]func(o *EnrolOptions){
		"no prefix":   func(o *EnrolOptions) { o.Token = "nope" },
		"agent token": func(o *EnrolOptions) { o.Token = AgentTokenPrefix + "env_secret" },
		"empty aud":   func(o *EnrolOptions) { o.Aud = "" },
		"short key":   func(o *EnrolOptions) { o.Key = o.Key[:10] },
		"nil key":     func(o *EnrolOptions) { o.Key = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			o := good
			mut(&o)
			a, err := EnrolAuth(ch, o)
			if err == nil {
				t.Fatalf("EnrolAuth succeeded: %+v", a)
			}
			if len(a.Sig) != 0 {
				t.Fatal("a refused EnrolAuth returned a signature")
			}
			if name == "agent token" && !strings.Contains(err.Error(), "agent") {
				t.Fatalf("err = %v, want it to name an agent join token", err)
			}
		})
	}
}

func TestDecodeBundleChecksTheInstancePin(t *testing.T) {
	b := testBundle(t)
	data := testEnvelope(t, "inst-1", b)
	got, err := DecodeBundle(data, "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != b.ID {
		t.Fatalf("ID = %q, want %q", got.ID, b.ID)
	}
	for _, pinned := range []string{"inst-2", ""} {
		if _, err := DecodeBundle(data, pinned); !errors.Is(err, ErrInstanceMismatch) {
			t.Fatalf("pinned %q: err = %v, want ErrInstanceMismatch", pinned, err)
		}
	}
}

func TestConnectAuthVerifies(t *testing.T) {
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	ch := testChallenge(t)
	ekm := testEKM(4)
	now := time.Unix(1_700_000_099, 0)
	a, err := ConnectAuth(ch, ConnectOptions{NodeID: "node-1", Version: "0.2.0", Aud: testAud, Key: key, EKM: ekm, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if a.T != TypeAuth || a.Mode != ModeConnect || a.Proto != ProtoVersion || a.NodeID != "node-1" || a.Token != "" || a.Aud != testAud || a.NodeVersion != "0.2.0" {
		t.Fatalf("fields = %+v", a)
	}
	if a.TS != now.Unix() || len(a.Nonce) != 32 {
		t.Fatalf("TS %d nonce %d bytes", a.TS, len(a.Nonce))
	}
	if !bytes.Equal(a.EKM, ekm) {
		t.Fatal("Auth.EKM is not o.EKM byte for byte")
	}
	pub := key.Public().(ed25519.PublicKey)
	if !bytes.Equal(a.Pub, pub) {
		t.Fatal("Auth.Pub is not the key's public half")
	}
	if !authn.Verify(a.Pub, serverRebuild(ch, a, a.EKM), a.Sig) {
		t.Fatal("the server's rebuild does not verify the connect auth")
	}
	if authn.Verify(pub, serverRebuild(ch, a, testEKM(5)), a.Sig) {
		t.Fatal("rebuild with another exporter verified")
	}
	if _, err := ConnectAuth(ch, ConnectOptions{Aud: testAud, Key: key, Now: now}); err == nil {
		t.Fatal("ConnectAuth with an empty NodeID succeeded")
	}
	if _, err := ConnectAuth(ch, ConnectOptions{NodeID: "n", Key: key, Now: now}); err == nil {
		t.Fatal("ConnectAuth with an empty Aud succeeded")
	}
	if _, err := ConnectAuth(ch, ConnectOptions{NodeID: "n", Aud: testAud, Key: key[:3], Now: now}); err == nil {
		t.Fatal("ConnectAuth with a short key succeeded")
	}
}

func TestCheckWelcomePinsOnEnrolAndRefusesAChange(t *testing.T) {
	w := Welcome{T: TypeWelcome, NodeID: "n", EnvID: "e", InstanceID: "inst-1"}
	if got, err := CheckWelcome(w, ""); err != nil || got != "inst-1" {
		t.Fatalf("unpinned: %q, %v", got, err)
	}
	if got, err := CheckWelcome(w, "inst-1"); err != nil || got != "inst-1" {
		t.Fatalf("equal pin: %q, %v", got, err)
	}
	if got, err := CheckWelcome(w, "inst-2"); !errors.Is(err, ErrInstanceMismatch) || got != "" {
		t.Fatalf("different pin: %q, %v", got, err)
	}
	w.InstanceID = ""
	if _, err := CheckWelcome(w, ""); err == nil {
		t.Fatal("an empty instance ID in the welcome was accepted")
	}
	if _, err := CheckWelcome(w, "inst-1"); err == nil {
		t.Fatal("an empty instance ID in the welcome was accepted against a pin")
	}
}

func TestParseServerMessage(t *testing.T) {
	cases := []struct {
		name string
		line string
		want any
		err  error
	}{
		{"challenge", `{"t":"challenge","proto":1,"nonce":"AQID","ts":5}`, Challenge{T: TypeChallenge, Proto: 1, Nonce: []byte{1, 2, 3}, TS: 5}, nil},
		{"welcome", `{"t":"welcome","nodeId":"n","envId":"e","instanceId":"i","healthMs":30000}`, Welcome{T: TypeWelcome, NodeID: "n", EnvID: "e", InstanceID: "i", HealthMs: 30000}, nil},
		{"refuse", `{"t":"refuse","code":"auth_rejected"}`, Refuse{T: TypeRefuse, Code: RefuseAuthRejected}, nil},
		{"hello", `{"t":"hello"}`, nil, ErrUnknownMessage},
		{"bundle", `{"t":"bundle"}`, nil, ErrUnknownMessage},
		{"auth", `{"t":"auth"}`, nil, ErrUnknownMessage},
		{"no type", `{"nonce":"AQID"}`, nil, ErrMalformed},
		{"not JSON", `{"t":`, nil, ErrMalformed},
		{"wrong field type", `{"t":"welcome","healthMs":"x"}`, nil, ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseServerMessage([]byte(c.line))
			if c.err != nil {
				if !errors.Is(err, c.err) || got != nil {
					t.Fatalf("got %+v, %v; want %v", got, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestDecodeBundleRefusesOversizeAndUnverifiable(t *testing.T) {
	big := make([]byte, MaxBundleMessageBytes+1)
	if _, err := DecodeBundle(big, "inst-1"); !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("oversize: err = %v, want ErrBundleTooLarge", err)
	}
	b := testBundle(t)
	b.Manifest.Files = append([]ingressbundle.FileEntry(nil), b.Manifest.Files...)
	b.Manifest.Files[0].SHA256 = strings.Repeat("0", 64)
	_, err := DecodeBundle(testEnvelope(t, "inst-1", b), "inst-1")
	var ve *ingressbundle.VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("a tampered checksum: err = %v, want a *VerifyError", err)
	}
	if _, err := DecodeBundle([]byte(`{"t":"hello"}`), "inst-1"); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("not a bundle: err = %v", err)
	}
	if _, err := DecodeBundle([]byte(`{"t":`), "inst-1"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad JSON: err = %v", err)
	}
}

func TestDecodeBundleAcceptsALowerVersion(t *testing.T) {
	mk := func(cfg int64, data string) ingressbundle.Bundle {
		b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: cfg},
			[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: []byte(data)}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	high, low := mk(9, "http: {a: 1}\n"), mk(3, "http: {b: 2}\n")
	got, err := DecodeBundle(testEnvelope(t, "inst-1", high), "inst-1")
	if err != nil || got.Version.Config != 9 {
		t.Fatalf("high: %+v, %v", got.Version, err)
	}
	got, err = DecodeBundle(testEnvelope(t, "inst-1", low), "inst-1")
	if err != nil || got.Version.Config != 3 || got.ID != low.ID {
		t.Fatalf("a lower version was refused or altered: %+v, %v", got.Version, err)
	}
}

type recordingLimiter struct{ calls []int64 }

func (r *recordingLimiter) SetReadLimit(n int64) { r.calls = append(r.calls, n) }

func TestPrepareNodeConnSetsTheBundleReadLimit(t *testing.T) {
	r := &recordingLimiter{}
	PrepareNodeConn(r)
	if len(r.calls) != 1 || r.calls[0] != MaxBundleMessageBytes {
		t.Fatalf("calls = %v, want exactly [%d]", r.calls, int64(MaxBundleMessageBytes))
	}
}
