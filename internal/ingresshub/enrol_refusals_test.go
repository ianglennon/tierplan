// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// refusal returns the RefuseError a service call failed with, or fails the test.
func refusal(t *testing.T, err error) *ingresshub.RefuseError {
	t.Helper()
	var re *ingresshub.RefuseError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want a *ingresshub.RefuseError", err)
	}
	return re
}

// countNodes is the number of ingress_nodes rows of the environment, read through the write connection so it
// still works when a test has closed the read pool.
func (f *fixture) countNodes(t *testing.T, envID string) int {
	t.Helper()
	var n int
	if err := f.st.Write.QueryRow(`SELECT COUNT(*) FROM ingress_nodes WHERE env_id = ?`, envID).Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return n
}

// countEnrolledAudits is the number of ingress.node_enrolled audit rows, read through the write connection.
func (f *fixture) countEnrolledAudits(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Write.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressNodeEnrolled).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// requireNothingWritten fails when an enrolment left a node row or an enrolment audit row behind.
func (f *fixture) requireNothingWritten(t *testing.T, envID string) {
	t.Helper()
	if n := f.countNodes(t, envID); n != 0 {
		t.Errorf("%d ingress_nodes rows after a refusal, want 0", n)
	}
	if n := f.countEnrolledAudits(t); n != 0 {
		t.Errorf("%d %s rows after a refusal, want 0", n, audit.ActionIngressNodeEnrolled)
	}
}

// secretOf returns the secret part of an issued token.
func secretOf(t *testing.T, token string) string {
	t.Helper()
	_, secret, ok := ingresshub.ParseToken(token)
	if !ok {
		t.Fatalf("issued token %q does not parse", token)
	}
	return secret
}

// TestEnrolRefusesAnAgentTokenNamingItsKind: a tpjt_ token pasted into an ingress node is named as the wrong
// kind, with a detail that names both kinds, and the refusal needs no database read (the read pool is closed).
func TestEnrolRefusesAnAgentTokenNamingItsKind(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1")
	_ = f.issue(t, envID)

	if err := f.st.Read.Close(); err != nil {
		t.Fatalf("close the read pool: %v", err)
	}
	agentToken := "tpjt_" + envID + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	_, key := newKey(t)
	_, err := f.svc.Enrol(context.Background(), enrolRequest(t, key, agentToken, "edge-a", "node-1"))
	if err == nil {
		t.Fatal("Enrol accepted an agent join token")
	}
	re := refusal(t, err)
	if re.Code != ingresswire.RefuseWrongTokenKind || re.AuditReason != "wrong_token_kind" {
		t.Errorf("refusal = code %q, audit %q, want wrong_token_kind for both", re.Code, re.AuditReason)
	}
	for _, want := range []string{"agent join token", "ingress join token"} {
		if !strings.Contains(re.Detail, want) {
			t.Errorf("detail %q does not name %q", re.Detail, want)
		}
	}
	if re.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want none", re.RetryAfter)
	}
	f.requireNothingWritten(t, envID)
}

// TestEnrolUniformRefusalForBadSupersededDisabledTokens: every way a token can be wrong is the one outward
// auth_rejected with one detail, and only the audit reason tells them apart.
func TestEnrolUniformRefusalForBadSupersededDisabledTokens(t *testing.T) {
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}

	cases := []struct {
		name  string
		token func(t *testing.T, f *fixture, envID string) string
		audit string
	}{
		{"empty", func(*testing.T, *fixture, string) string { return "" }, "bad_token"},
		{"prefix only", func(*testing.T, *fixture, string) string { return "tpit_" }, "bad_token"},
		{"environment is not a UUID", func(t *testing.T, f *fixture, envID string) string {
			return "tpit_not-a-uuid_" + secretOf(t, f.issue(t, envID).Token)
		}, "bad_token"},
		{"secret of 31 bytes", func(t *testing.T, f *fixture, envID string) string {
			_ = f.issue(t, envID)
			return "tpit_" + envID + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, 31))
		}, "bad_token"},
		{"secret of this environment under another environment's ID", func(t *testing.T, f *fixture, envID string) string {
			other := f.insertEnv(t, "swarm")
			_ = f.issue(t, other)
			return "tpit_" + other + "_" + secretOf(t, f.issue(t, envID).Token)
		}, "bad_token"},
		{"environment that does not exist", func(t *testing.T, f *fixture, envID string) string {
			return "tpit_" + uuid.NewString() + "_" + secretOf(t, f.issue(t, envID).Token)
		}, "bad_token"},
		{"right shape, wrong secret", func(t *testing.T, f *fixture, envID string) string {
			_ = f.issue(t, envID)
			return "tpit_" + envID + "_" + base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		}, "bad_token"},
		{"superseded by a rotation", func(t *testing.T, f *fixture, envID string) string {
			old := f.issue(t, envID)
			_ = f.issue(t, envID)
			return old.Token
		}, "token_superseded"},
		{"disabled", func(t *testing.T, f *fixture, envID string) string {
			tok := f.issue(t, envID)
			if err := f.svc.DisableToken(ctx, admin, envID, "10.0.0.1", "test"); err != nil {
				t.Fatalf("DisableToken: %v", err)
			}
			return tok.Token
		}, "token_disabled"},
	}

	var detail string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			envID := f.insertEnv(t, "swarm")
			f.freshList(envID, "node-1")
			token := tc.token(t, f, envID)

			_, key := newKey(t)
			_, err := f.svc.Enrol(ctx, enrolRequest(t, key, token, "edge-a", "node-1"))
			if err == nil {
				t.Fatal("Enrol accepted the token")
			}
			re := refusal(t, err)
			if re.Code != ingresswire.RefuseAuthRejected {
				t.Errorf("code = %q, want %q", re.Code, ingresswire.RefuseAuthRejected)
			}
			if re.AuditReason != tc.audit {
				t.Errorf("audit reason = %q, want %q", re.AuditReason, tc.audit)
			}
			if re.RetryAfter != 0 {
				t.Errorf("RetryAfter = %v, want none", re.RetryAfter)
			}
			// One detail for every row: a probe cannot tell the cases apart.
			if detail == "" {
				detail = re.Detail
			}
			if re.Detail != detail {
				t.Errorf("detail = %q, differs from the other rows' %q", re.Detail, detail)
			}
			f.requireNothingWritten(t, envID)
		})
	}
	if want := "the ingress join token was not accepted"; detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

// TestEnrolRefusesABadProofOfPossession: a valid token with a signature by another key, or over another
// transcript, is the same auth_rejected, with its own audit reason.
func TestEnrolRefusesABadProofOfPossession(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		edit func(t *testing.T, req *ingresshub.EnrolRequest)
	}{
		{"signature by another key", func(t *testing.T, req *ingresshub.EnrolRequest) {
			_, other := newKey(t)
			req.Sig = ed25519.Sign(other, req.Transcript)
		}},
		{"signature over another transcript", func(t *testing.T, req *ingresshub.EnrolRequest) {
			other := authn.IngressTranscript(ingresswire.ModeEnrol, ingresswire.ProtoVersion,
				[]byte("another-server-nonce-0123456789a"), testNodeNonce, testAudience, req.Pub, testEKM)
			req.Transcript = other
			// The signature stays the one over the original transcript.
		}},
		{"no signature", func(_ *testing.T, req *ingresshub.EnrolRequest) { req.Sig = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			envID := f.insertEnv(t, "swarm")
			f.freshList(envID, "node-1")
			tok := f.issue(t, envID)

			_, key := newKey(t)
			req := enrolRequest(t, key, tok.Token, "edge-a", "node-1")
			tc.edit(t, &req)
			_, err := f.svc.Enrol(ctx, req)
			if err == nil {
				t.Fatal("Enrol accepted a bad proof of possession")
			}
			re := refusal(t, err)
			if re.Code != ingresswire.RefuseAuthRejected || re.AuditReason != "bad_proof_of_possession" {
				t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
			}
			if re.Detail != "the ingress join token was not accepted" {
				t.Errorf("detail = %q, want the one every token refusal carries", re.Detail)
			}
			f.requireNothingWritten(t, envID)
		})
	}
}
