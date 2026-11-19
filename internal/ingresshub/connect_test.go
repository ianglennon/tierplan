// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// enrolled is a node that has enrolled into a fixture's environment.
type enrolled struct {
	res ingresshub.EnrolResult
	pub ed25519.PublicKey
	key ed25519.PrivateKey
}

// enrolNode enrols a swarm node with its own key into envID and returns it.
func (f *fixture) enrolNode(t *testing.T, envID, token, name, swarmNode string) enrolled {
	t.Helper()
	pub, key := newKey(t)
	res, err := f.svc.Enrol(context.Background(), enrolRequest(t, key, token, name, swarmNode))
	if err != nil {
		t.Fatalf("Enrol %s: %v", name, err)
	}
	if res.EnvID != envID {
		t.Fatalf("enrolled into %s, want %s", res.EnvID, envID)
	}
	return enrolled{res: res, pub: pub, key: key}
}

// swarmEnv is a swarm environment whose node list holds the given swarm nodes, with an issued join token.
func (f *fixture) swarmEnv(t *testing.T, swarmNodes ...string) (envID string, tok ingresshub.Token) {
	t.Helper()
	envID = f.insertEnv(t, "swarm")
	f.freshList(envID, swarmNodes...)
	return envID, f.issue(t, envID)
}

// markRevoked turns a node's row revoked directly, so the connect tests do not depend on Revoke.
func (f *fixture) markRevoked(t *testing.T, nodeID string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`UPDATE ingress_nodes SET status = 'revoked', revoked_at = 1 WHERE id = ?`, nodeID); err != nil {
		t.Fatalf("mark revoked: %v", err)
	}
}

// connectRequest is what the hub hands Connect for a node: its ID, the key it presents and a signature by key over
// the connect transcript of the hub's nonces.
func connectRequest(nodeID string, pub []byte, key ed25519.PrivateKey) ingresshub.ConnectRequest {
	transcript := authn.IngressTranscript(ingresswire.ModeConnect, ingresswire.ProtoVersion, testServerNonce, testNodeNonce, testAudience, pub, testEKM)
	return ingresshub.ConnectRequest{
		NodeID: nodeID, Pub: pub, Sig: ed25519.Sign(key, transcript), Transcript: transcript,
		Version: "0.1.0", IP: "192.0.2.10",
	}
}

func TestConnectProvesTheStoredKey(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	res, err := f.svc.Connect(context.Background(), connectRequest(n.res.NodeID, n.pub, n.key))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if res.NodeID != n.res.NodeID || res.EnvID != envID || res.Name != "edge-a" {
		t.Errorf("result = %+v, want node %s, environment %s, name edge-a", res, n.res.NodeID, envID)
	}
	// A successful connect is not audited by the service: the hub has nothing to say about it (D-18).
	if rows := f.auditRows(t, audit.ActionIngressNodeEnrolRefused); len(rows) != 0 {
		t.Errorf("%d refusal audit rows after a good connect", len(rows))
	}
}

func TestConnectUniformRefusal(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	b := f.enrolNode(t, envID, tok.Token, "edge-b", "node-2")

	// Material for the cases: another key pair, and a signature that fails against the right key.
	otherPub, otherKey := newKey(t)
	badSig := connectRequest(a.res.NodeID, a.pub, a.key)
	badSig.Sig = append([]byte(nil), badSig.Sig...)
	badSig.Sig[0] ^= 0xff

	cases := []struct {
		name   string
		req    ingresshub.ConnectRequest
		reason string
	}{
		{"not a UUID", connectRequest("not-a-uuid", a.pub, a.key), "malformed"},
		{"empty ID", connectRequest("", a.pub, a.key), "malformed"},
		{"unknown node", connectRequest(uuid.NewString(), otherPub, otherKey), "unknown_node"},
		{"another key for a real node", connectRequest(a.res.NodeID, otherPub, otherKey), "key_mismatch"},
		{"another node's key", connectRequest(a.res.NodeID, b.pub, b.key), "key_mismatch"},
		{"bad signature", badSig, "bad_signature"},
	}
	var detail string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.svc.Connect(context.Background(), c.req)
			var re *ingresshub.RefuseError
			if !errors.As(err, &re) {
				t.Fatalf("Connect error = %v, want a RefuseError", err)
			}
			if re.Code != ingresswire.RefuseAuthRejected {
				t.Errorf("code = %q, want %q", re.Code, ingresswire.RefuseAuthRejected)
			}
			if re.AuditReason != c.reason {
				t.Errorf("audit reason = %q, want %q", re.AuditReason, c.reason)
			}
			if re.RetryAfter != 0 {
				t.Errorf("RetryAfter = %v on a refusal that does not pass", re.RetryAfter)
			}
			// One text for every case, so a probe learns nothing from it.
			if detail == "" {
				detail = re.Detail
			}
			if re.Detail == "" || re.Detail != detail {
				t.Errorf("detail = %q, want the one shared text %q", re.Detail, detail)
			}
		})
	}
}

func TestConnectRevokedIsToldOnlyToTheKeyHolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	f.markRevoked(t, n.res.NodeID)

	// The holder of the key, with a valid signature, is told.
	_, err := f.svc.Connect(ctx, connectRequest(n.res.NodeID, n.pub, n.key))
	var re *ingresshub.RefuseError
	if !errors.As(err, &re) {
		t.Fatalf("Connect error = %v, want a RefuseError", err)
	}
	if re.Code != ingresswire.RefuseRevoked || re.AuditReason != "revoked" || re.Detail != "an administrator revoked this ingress node" {
		t.Errorf("refusal = %+v, want code revoked, reason revoked and the revoked text", re)
	}

	// Anyone else gets the uniform answer: a wrong key, and the right key with a bad signature.
	otherPub, otherKey := newKey(t)
	for name, req := range map[string]ingresshub.ConnectRequest{
		"wrong key": connectRequest(n.res.NodeID, otherPub, otherKey),
		"bad signature": func() ingresshub.ConnectRequest {
			r := connectRequest(n.res.NodeID, n.pub, n.key)
			r.Transcript = append([]byte("x"), r.Transcript...)
			return r
		}(),
	} {
		_, err := f.svc.Connect(ctx, req)
		if !errors.As(err, &re) || re.Code != ingresswire.RefuseAuthRejected {
			t.Errorf("%s: error = %v, want auth_rejected", name, err)
		}
	}
}

func TestConnectStillActive(t *testing.T) {
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	b := f.enrolNode(t, envID, tok.Token, "edge-b", "node-2")
	f.markRevoked(t, b.res.NodeID)
	for _, c := range []struct {
		name string
		id   string
		want bool
	}{
		{"active", a.res.NodeID, true},
		{"revoked", b.res.NodeID, false},
		{"unknown", uuid.NewString(), false},
		{"empty", "", false},
	} {
		if got := f.svc.StillActive(c.id); got != c.want {
			t.Errorf("StillActive(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}
