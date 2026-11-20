// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

var testAdmin = audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}

// refusalReason enrols with token on a fresh key and returns the audit reason of the refusal, or "" when it
// enrolled.
func refusalReason(t *testing.T, f *fixture, token, swarmNode string) string {
	t.Helper()
	_, key := newKey(t)
	_, err := f.svc.Enrol(context.Background(), enrolRequest(t, key, token, "edge", swarmNode))
	if err == nil {
		return ""
	}
	var re *ingresshub.RefuseError
	if !errors.As(err, &re) {
		t.Fatalf("Enrol: %v, want a refusal or success", err)
	}
	return re.AuditReason
}

func TestJoinTokenRotateDisableStatus(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1", "node-2", "node-3")

	// No token yet: nothing to report, nothing to disable, and nothing audited for it.
	st, err := f.svc.TokenStatus(ctx, envID)
	if err != nil || st.Exists {
		t.Fatalf("TokenStatus before any token = %+v, %v", st, err)
	}
	if err := f.svc.DisableToken(ctx, testAdmin, envID, "10.0.0.1", "test"); err != nil {
		t.Fatalf("DisableToken with no token: %v", err)
	}
	if rows := f.auditRows(t, audit.ActionIngressJoinTokenDisabled); len(rows) != 0 {
		t.Fatalf("%d disabled rows for an environment with no token", len(rows))
	}
	if _, err := f.svc.TokenStatus(ctx, uuid.NewString()); !errors.Is(err, ingresshub.ErrEnvironmentNotFound) {
		t.Errorf("TokenStatus of an unknown environment = %v, want ErrEnvironmentNotFound", err)
	}
	if _, err := f.svc.IssueToken(ctx, testAdmin, uuid.NewString(), "", ""); !errors.Is(err, ingresshub.ErrEnvironmentNotFound) {
		t.Errorf("IssueToken for an unknown environment = %v, want ErrEnvironmentNotFound", err)
	}

	// Generation 1.
	first := f.issue(t, envID)
	if first.Generation != 1 || first.Rotated {
		t.Fatalf("first = %+v", first)
	}
	if rows := f.auditRows(t, audit.ActionIngressJoinTokenCreated); len(rows) != 1 || rows[0].Details != `{"generation":"1"}` ||
		rows[0].TargetType != "environment" || rows[0].TargetID != envID || rows[0].EnvID != envID || rows[0].ActorID != "u-admin" {
		t.Fatalf("created rows = %+v", rows)
	}
	st, err = f.svc.TokenStatus(ctx, envID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Generation != 1 || st.CreatedBy != "u-admin" || st.Disabled || !st.CreatedAt.Equal(f.clock.Now()) {
		t.Errorf("status = %+v", st)
	}

	// Generation 2 supersedes it: the old token no longer enrols, the new one does.
	f.clock.Advance(time.Minute)
	second := f.issue(t, envID)
	if second.Generation != 2 || !second.Rotated || second.Token == first.Token {
		t.Fatalf("second = %+v", second)
	}
	if rows := f.auditRows(t, audit.ActionIngressJoinTokenRotated); len(rows) != 1 || rows[0].Details != `{"generation":"2"}` {
		t.Fatalf("rotated rows = %+v", rows)
	}
	if got := refusalReason(t, f, first.Token, "node-1"); got != "token_superseded" {
		t.Errorf("the superseded token: audit reason %q, want token_superseded", got)
	}
	if got := refusalReason(t, f, second.Token, "node-1"); got != "" {
		t.Errorf("the new token was refused: %q", got)
	}

	// Disable it: one audit row, a second call writes nothing, the status says so, and it enrols no more.
	if err := f.svc.DisableToken(ctx, testAdmin, envID, "10.0.0.1", "test"); err != nil {
		t.Fatalf("DisableToken: %v", err)
	}
	if err := f.svc.DisableToken(ctx, testAdmin, envID, "10.0.0.1", "test"); err != nil {
		t.Fatalf("second DisableToken: %v", err)
	}
	if rows := f.auditRows(t, audit.ActionIngressJoinTokenDisabled); len(rows) != 1 || rows[0].Details != `{"generation":"2"}` {
		t.Fatalf("disabled rows = %+v", rows)
	}
	if st, err = f.svc.TokenStatus(ctx, envID); err != nil || !st.Disabled || st.Generation != 2 {
		t.Errorf("status after disable = %+v, %v", st, err)
	}
	if got := refusalReason(t, f, second.Token, "node-2"); got != "token_disabled" {
		t.Errorf("the disabled token: audit reason %q, want token_disabled", got)
	}

	// A new token after a disable is the next generation and a rotation of the disabled one.
	third := f.issue(t, envID)
	if third.Generation != 3 || !third.Rotated {
		t.Fatalf("third = %+v", third)
	}
	if st, err = f.svc.TokenStatus(ctx, envID); err != nil || st.Disabled || st.Generation != 3 {
		t.Errorf("status after the third token = %+v, %v", st, err)
	}

	// Only the hash is stored: the secret is in no audit row, and the stored hash is the secret's SHA-256.
	_, secret, ok := ingresshub.ParseToken(third.Token)
	if !ok {
		t.Fatal("the issued token does not parse")
	}
	var leaked int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE details LIKE '%' || ? || '%' OR reason LIKE '%' || ? || '%'`, secret, secret).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("the token secret appears in %d audit rows", leaked)
	}
	var stored []byte
	if err := f.st.Read.QueryRow(`SELECT token_hash FROM ingress_join_tokens WHERE env_id = ? AND generation = 3`, envID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256([]byte(secret)); !bytes.Equal(stored, want[:]) {
		t.Errorf("stored hash = %x, want the SHA-256 of the secret", stored)
	}
}

func TestRotationLeavesEnrolledNodesAlone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "node-1")
	tok := f.issue(t, envID)
	_, key := newKey(t)
	res, err := f.svc.Enrol(ctx, enrolRequest(t, key, tok.Token, "edge-a", "node-1"))
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	q := sqlc.New(f.st.Read)
	before, err := q.GetIngressNode(ctx, res.NodeID)
	if err != nil {
		t.Fatal(err)
	}

	f.clock.Advance(time.Hour)
	f.issue(t, envID)
	if err := f.svc.DisableToken(ctx, testAdmin, envID, "", ""); err != nil {
		t.Fatal(err)
	}
	f.issue(t, envID)

	after, err := q.GetIngressNode(ctx, res.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the node row changed across rotations and a disable:\n before %+v\n after  %+v", before, after)
	}
	if after.Status != "active" {
		t.Errorf("status = %q, want active", after.Status)
	}
}

func TestParseTokenOnlyAcceptsIngressTokens(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	tok := f.issue(t, envID)

	gotEnv, secret, ok := ingresshub.ParseToken(tok.Token)
	if !ok || gotEnv != envID {
		t.Fatalf("ParseToken(issued) = %q, %q, %v", gotEnv, secret, ok)
	}
	if !strings.HasPrefix(tok.Token, "tpit_"+envID+"_") || len(secret) != 43 {
		t.Errorf("token %q does not have the shape tpit_<env>_<43 characters> (secret %d characters)", tok.Token, len(secret))
	}

	bad := map[string]string{
		"agent prefix":    strings.Replace(tok.Token, "tpit_", "tpjt_", 1),
		"no prefix":       strings.TrimPrefix(tok.Token, "tpit_"),
		"not a uuid":      "tpit_not-a-uuid_" + secret,
		"no separator":    "tpit_" + envID,
		"short secret":    "tpit_" + envID + "_" + secret[:42],
		"long secret":     "tpit_" + envID + "_" + secret + "A",
		"not base64url":   "tpit_" + envID + "_" + strings.Repeat("+", 43),
		"empty secret":    "tpit_" + envID + "_",
		"empty":           "",
		"prefix only":     "tpit_",
		"uuid and secret": envID + "_" + secret,
	}
	for name, token := range bad {
		if _, _, ok := ingresshub.ParseToken(token); ok {
			t.Errorf("%s: ParseToken accepted %q", name, token)
		}
	}

	// The agent's parser refuses the ingress token, the other half of keeping the two kinds apart.
	if _, _, ok := agents.ParseToken(tok.Token); ok {
		t.Errorf("agents.ParseToken accepted the ingress token %q", tok.Token)
	}
}
