// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares the JSON of v with testdata/<name>.json byte for byte. The
// files are the contract: a node parses exactly these bytes, so a failure here is
// a change of the frozen format, not a test to refresh. -update writes the files
// for a new message and must not be used to make a failure pass.
func golden(t *testing.T, name string, v any) []byte {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // a golden file in the repository
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a golden file in the repository
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches its golden file (this format is frozen for all of 0.x):\n got: %s\nwant: %s", name, got, want)
	}
	return want
}

// withExtra adds an unknown field to a JSON object, as a later version would.
func withExtra(t *testing.T, line []byte) []byte {
	t.Helper()
	s := strings.TrimSpace(string(line))
	if !strings.HasSuffix(s, "}") {
		t.Fatalf("not an object: %s", s)
	}
	return []byte(strings.TrimSuffix(s, "}") + `,"futureField":{"a":[1,2]},"another":"x"}`)
}

// seq is n fixed bytes counting up from start, so a golden file holds the same
// bytes on every run.
func seq(n int, start byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

const (
	fixedNodeID     = "018f0000-0000-7000-8000-000000000002"
	fixedInstanceID = "018f0000-0000-7000-8000-000000000003"
	fixedEnvID      = "018f0000-0000-7000-8000-000000000001"
	fixedToken      = TokenPrefix + fixedEnvID + "_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq"
	fixedBundleID   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func fixedChallenge() Challenge {
	return Challenge{T: TypeChallenge, Proto: ProtoVersion, Nonce: seq(32, 0), TS: 1767225600000, ServerVersion: "0.2.0", Features: []string{"x-future"}}
}

func fixedAuthEnrol() Auth {
	return Auth{
		T: TypeAuth, Mode: ModeEnrol, Proto: ProtoVersion, NodeVersion: "0.2.0",
		Name: "node-1", SwarmNodeID: "abcdefghijklmnopqrstuvwxy",
		Aud: "tierplan.example.org:7480", Nonce: seq(32, 0), TS: 1767225600500,
		Pub: seq(32, 64), Token: fixedToken, EKM: seq(32, 128), Sig: seq(64, 160),
	}
}

func fixedAuthConnect() Auth {
	return Auth{
		T: TypeAuth, Mode: ModeConnect, Proto: ProtoVersion, NodeVersion: "0.2.0",
		NodeID: fixedNodeID,
		Aud:    "tierplan.example.org:7480", Nonce: seq(32, 0), TS: 1767225600500,
		Pub: seq(32, 64), EKM: seq(32, 128), Sig: seq(64, 160),
	}
}

func fixedWelcome() Welcome {
	return Welcome{T: TypeWelcome, NodeID: fixedNodeID, EnvID: fixedEnvID, InstanceID: fixedInstanceID, HealthMs: 30000, Pins: []string{"sha256/AAAA"}, ServerTime: 1767225601000}
}

func fixedRefuse() Refuse {
	return Refuse{T: TypeRefuse, Code: RefuseNodeUnverified, RetryAfterMs: 15000}
}

func TestIngressFrozenHandshakeMessagesMatchTheirGoldenFiles(t *testing.T) {
	golden(t, "challenge", fixedChallenge())
	golden(t, "auth_enrol", fixedAuthEnrol())
	golden(t, "auth_connect", fixedAuthConnect())
	golden(t, "welcome", fixedWelcome())
	golden(t, "refuse", fixedRefuse())
}

func TestIngressFrozenMessagesIgnoreUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		want any
		got  func() any
	}{
		{"challenge", fixedChallenge(), func() any { return new(Challenge) }},
		{"auth_enrol", fixedAuthEnrol(), func() any { return new(Auth) }},
		{"auth_connect", fixedAuthConnect(), func() any { return new(Auth) }},
		{"welcome", fixedWelcome(), func() any { return new(Welcome) }},
		{"refuse", fixedRefuse(), func() any { return new(Refuse) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := golden(t, c.name, c.want)
			got := c.got()
			if err := json.Unmarshal(withExtra(t, line), got); err != nil {
				t.Fatalf("an unknown field must not fail the parse: %v", err)
			}
			if !reflect.DeepEqual(reflect.ValueOf(got).Elem().Interface(), c.want) {
				t.Fatalf("known fields lost:\n got: %+v\nwant: %+v", got, c.want)
			}
		})
	}
}

// TestIngressWireImportsNoWire pins that the ingress contract and the agent
// contract cannot break each other: no non-test file imports proto/wire.
func TestIngressWireImportsNoWire(t *testing.T) {
	const forbidden = "github.com/ianglennon/tierplan/proto/wire"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // G304: a Go file of this package, found by Glob
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
			}
			if strings.HasPrefix(path, forbidden) {
				t.Errorf("%s imports %s: the ingress contract must not import proto/wire", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go file was checked")
	}
}

// fixedHello is a frozen vector: its schemas [1] are historic wire data (what a Phase 22 node sent), and the
// pinned bytes do not change when the supported schema does.
func fixedHello() Hello {
	return Hello{T: TypeHello, Version: "0.2.0", Schemas: []int{1}, AppliedID: fixedBundleID}
}

func fixedApplied() Applied { return Applied{T: TypeApplied, ID: fixedBundleID} }

func fixedRejected() Rejected {
	return Rejected{T: TypeRejected, ID: fixedBundleID, Reason: "traefik_reload_failed"}
}

func fixedHealth() Health {
	return Health{T: TypeHealth, TraefikOK: true, AppliedID: fixedBundleID}
}

func fixedEnvelope(t *testing.T) BundleEnvelope {
	t.Helper()
	// The envelope's golden file is frozen for all of 0.x and carries a schema 1 bundle as data, so the
	// vector names the old schema explicitly; the envelope's shape does not depend on the schema number.
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 3, Certs: 2, Own: 1},
		[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: []byte("http:\n  routers: {}\n")}}, nil)
	if err != nil {
		t.Fatalf("build the fixed bundle: %v", err)
	}
	return BundleEnvelope{T: TypeBundle, InstanceID: fixedInstanceID, Bundle: b}
}

func TestIngressFrozenNodeMessagesMatchTheirGoldenFiles(t *testing.T) {
	golden(t, "hello", fixedHello())
	golden(t, "applied", fixedApplied())
	golden(t, "rejected", fixedRejected())
	golden(t, "health", fixedHealth())
	golden(t, "bundle_envelope", fixedEnvelope(t))
}

// TestFrozenIngressNamesDoNotChange pins every string and number a node in the
// field compares against or sends.
func TestFrozenIngressNamesDoNotChange(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"Subprotocol", Subprotocol, "tierplan.ingress.v1"},
		{"TypeChallenge", TypeChallenge, "challenge"},
		{"TypeAuth", TypeAuth, "auth"},
		{"TypeWelcome", TypeWelcome, "welcome"},
		{"TypeRefuse", TypeRefuse, "refuse"},
		{"TypeHello", TypeHello, "hello"},
		{"TypeBundle", TypeBundle, "bundle"},
		{"TypeApplied", TypeApplied, "applied"},
		{"TypeRejected", TypeRejected, "rejected"},
		{"TypeHealth", TypeHealth, "health"},
		{"ModeEnrol", ModeEnrol, "enrol"},
		{"ModeConnect", ModeConnect, "connect"},
		{"TokenPrefix", TokenPrefix, "tpit_"},
		{"AgentTokenPrefix", AgentTokenPrefix, "tpjt_"},
		{"RefuseAuthRejected", RefuseAuthRejected, "auth_rejected"},
		{"RefuseWrongTokenKind", RefuseWrongTokenKind, "wrong_token_kind"},
		{"RefuseRevoked", RefuseRevoked, "revoked"},
		{"RefuseNodeRevoked", RefuseNodeRevoked, "node_revoked"},
		{"RefuseNodeNotInSwarm", RefuseNodeNotInSwarm, "node_not_in_swarm"},
		{"RefuseNodeUnverified", RefuseNodeUnverified, "node_unverified"},
		{"RefuseNodeAlreadyEnrolled", RefuseNodeAlreadyEnrolled, "node_already_enrolled"},
		{"RefuseRateLimited", RefuseRateLimited, "rate_limited"},
		{"RefuseInsecureTransport", RefuseInsecureTransport, "insecure_transport"},
		{"RefuseInternal", RefuseInternal, "internal_error"},
		{"CloseProtocolError", CloseProtocolError, "protocol_error"},
		{"CloseRevoked", CloseRevoked, "revoked"},
		{"CloseReplaced", CloseReplaced, "replaced"},
		{"CloseRemoved", CloseRemoved, "removed"},
		{"CloseTimeout", CloseTimeout, "timeout"},
		{"CloseShutdown", CloseShutdown, "shutdown"},
	} {
		if c.got != c.want {
			t.Errorf("a frozen name changed: %s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name      string
		got, want int64
	}{
		{"ProtoVersion", ProtoVersion, 1},
		{"MaxHandshakeBytes", MaxHandshakeBytes, 16 << 10},
		{"HandshakeTimeout", int64(HandshakeTimeout), int64(10_000_000_000)},
		{"MaxNodeMessageBytes", MaxNodeMessageBytes, 16 << 10},
		{"MaxBundleMessageBytes", MaxBundleMessageBytes, 64 << 20},
		{"HealthInterval", int64(HealthInterval), int64(30_000_000_000)},
		{"MaxNameLen", MaxNameLen, 128},
		{"MaxVersionLen", MaxVersionLen, 64},
		{"MaxSchemas", MaxSchemas, 16},
		{"MaxReasonLen", MaxReasonLen, 512},
		{"MaxIdentityLen", MaxIdentityLen, 128},
	} {
		if c.got != c.want {
			t.Errorf("a frozen limit changed: %s = %d, want %d", c.name, c.got, c.want)
		}
	}
}
