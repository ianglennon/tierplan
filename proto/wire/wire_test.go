// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package wire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func roundTrip[T any](t *testing.T, in T) T {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal %T: %v", in, err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %T from %s: %v", in, b, err)
	}
	return out
}

func TestEveryMessageRoundTripsThroughJSON(t *testing.T) {
	nonce := bytes.Repeat([]byte{7}, 32)
	facts := Facts{
		EngineID: "ENG:1", Hostname: "node-1", EngineReachable: true, APIVersion: "1.52", EngineVersion: "29.1.0",
		Swarm:  &SwarmFacts{State: "active", NodeID: "n1", ClusterID: "c1", Control: true, ManagerIDs: []string{"n1", "n2"}},
		Policy: PolicyFacts{ReadOnly: true, SelfUpdate: false},
	}
	cases := map[string]any{
		"challenge": Challenge{T: TypeChallenge, Proto: ProtoVersion, Nonce: nonce, TS: 1700000000000, ServerVersion: "0.2.0", Features: []string{"x"}},
		"auth": Auth{T: TypeAuth, Mode: ModeEnrol, Proto: ProtoVersion, AgentVersion: "0.2.0", Aud: "bayplan.example:443", //nolint:gosec // the token is a placeholder in a round-trip test
			Nonce: nonce, TS: 1700000000001, Pub: []byte{1, 2, 3}, Token: "bpjt_x_y", EKM: nonce, Sig: []byte{9, 9}, Facts: facts},
		"auth hello": Auth{T: TypeAuth, Mode: ModeHello, Proto: ProtoVersion, AgentID: "a1", Aud: "h", Nonce: nonce, Cert: []byte{4, 5}, Sig: []byte{1}},
		"welcome": Welcome{T: TypeWelcome, AgentID: "a1", EnvID: "e1", Cert: []byte{1}, HeartbeatMs: DefaultHeartbeatMs,
			URLs: []string{"wss://h/agent/connect"}, Pins: []string{"sha256/abc"}, ServerTime: 1700000000002},
		"refuse": Refuse{T: TypeRefuse, Code: ReasonAuthRejected, Detail: "the join token was not accepted", RetryAfterMs: 5000},
		"facts":  facts,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var out any
			switch v := in.(type) {
			case Challenge:
				out = roundTrip(t, v)
			case Auth:
				out = roundTrip(t, v)
			case Welcome:
				out = roundTrip(t, v)
			case Refuse:
				out = roundTrip(t, v)
			case Facts:
				out = roundTrip(t, v)
			}
			if !reflect.DeepEqual(in, out) {
				t.Fatalf("round trip changed the message:\n in: %+v\nout: %+v", in, out)
			}
		})
	}
}

func TestByteFieldsAreBase64OnTheWire(t *testing.T) {
	b, err := json.Marshal(Challenge{T: TypeChallenge, Nonce: []byte{0xff, 0xee}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"nonce":"/+4="`)) {
		t.Fatalf("nonce is not standard base64 in %s", b)
	}
}

func TestTheStreamKindAndSubprotocolAreFrozen(t *testing.T) {
	// These values are stored in, or spoken by, every deployed agent.
	if Subprotocol != "bayplan.agent.v1" || ProtoVersion != 1 || StreamKindDockerHTTP != 0x01 {
		t.Fatalf("a frozen wire constant changed: %q %d %#x", Subprotocol, ProtoVersion, StreamKindDockerHTTP)
	}
}

func TestEveryD40ReasonCodeIsDefinedOnce(t *testing.T) {
	codes := []string{
		ReasonAgentOffline, ReasonTLSUntrusted, ReasonAuthRejected, ReasonCertExpired, ReasonRevoked,
		ReasonBlocked, ReasonVersionIncompatible, ReasonClockSkew, ReasonTimeout, ReasonNotAManager,
		ReasonNoQuorum, ReasonClusterChanged, ReasonEngineUnreachable, ReasonUnsupportedEngine, ReasonAgentPolicyDenied,
	}
	if len(codes) != 15 {
		t.Fatalf("D-40 names 15 codes, the test lists %d", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if c == "" || seen[c] {
			t.Fatalf("reason code %q is empty or duplicated", c)
		}
		seen[c] = true
	}
	for _, c := range []string{
		RefuseDuplicateEnvironment, RefuseTypeMismatch, RefuseHostMismatch, RefuseTokenDisabled, RefuseTokenSuperseded, RefuseInsecureTransport, RefuseRateLimited, RefuseInternal,
	} {
		if c == "" || seen[c] {
			t.Fatalf("enrolment refusal %q is empty or collides with a reason code", c)
		}
		seen[c] = true
	}
}

func TestAHeartbeatRoundTripsAndTheStreamKindsDiffer(t *testing.T) {
	in := Heartbeat{T: TypeHeartbeat, EngineReachable: true, ClockMs: 1700000000003}
	if out := roundTrip(t, in); out != in {
		t.Fatalf("heartbeat = %+v, want %+v", out, in)
	}
	b, _ := json.Marshal(in)
	if !bytes.Contains(b, []byte(`"t":"heartbeat"`)) || !bytes.Contains(b, []byte(`"engineReachable":true`)) {
		t.Errorf("the heartbeat's JSON names changed: %s", b)
	}
	if StreamKindControl == StreamKindDockerHTTP || StreamKindControl != 0x02 {
		t.Errorf("control stream kind = %#x", StreamKindControl)
	}
}

func TestADeniedMessageHasTheShapeTheServerReads(t *testing.T) {
	in := Denied{T: TypeDenied, Method: "POST", Path: "/containers/create", Reason: DeniedReadOnly}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"t":"denied","method":"POST","path":"/containers/create","reason":"read_only_flag"}`
	if string(b) != want {
		t.Fatalf("denied = %s, want %s", b, want)
	}
	if out := roundTrip(t, in); out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	if DeniedAllowlist != "allowlist" || DeniedReadOnly != "read_only_flag" {
		t.Fatalf("reasons = %q, %q", DeniedAllowlist, DeniedReadOnly)
	}
}
