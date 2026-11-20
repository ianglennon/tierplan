// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// enrolOnce enrols one node with token and returns the result and error.
func (f *fixture) enrolOnce(t *testing.T, token, name, swarmNodeID string) (ingresshub.EnrolResult, error) {
	t.Helper()
	_, key := newKey(t)
	return f.svc.Enrol(context.Background(), enrolRequest(t, key, token, name, swarmNodeID))
}

// insertRevoked adds a revoked row for the node (swarmNodeID "" is the standalone slot) as an administrator's
// revoke would leave it; the Revoke service call itself is 22-12's.
func (f *fixture) insertRevoked(t *testing.T, envID, swarmNodeID string, reenrolBlocked int) {
	t.Helper()
	pub, _ := newKey(t)
	var node sql.NullString
	if swarmNodeID != "" {
		node = sql.NullString{String: swarmNodeID, Valid: true}
	}
	if _, err := f.st.Write.Exec(`INSERT INTO ingress_nodes
		(id, env_id, name, swarm_node_id, public_key, status, created_at, revoked_at, revoked_by, revoke_reason, reenrol_blocked)
		VALUES (?, ?, 'old', ?, ?, 'revoked', 1, 2, 'u-admin', 'test', ?)`,
		uuid.NewString(), envID, node, []byte(pub), reenrolBlocked); err != nil {
		t.Fatalf("insert a revoked node: %v", err)
	}
}

// activeNodes is the number of active rows of the environment.
func (f *fixture) activeNodes(t *testing.T, envID string) int {
	t.Helper()
	var n int
	if err := f.st.Write.QueryRow(`SELECT COUNT(*) FROM ingress_nodes WHERE env_id = ? AND status = 'active'`, envID).Scan(&n); err != nil {
		t.Fatalf("count active nodes: %v", err)
	}
	return n
}

func TestEnrolTwiceRefusesNodeAlreadyEnrolled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, swarmNodeN)
	tok := f.issue(t, envID)

	first, err := f.enrolOnce(t, tok.Token, "edge-a", swarmNodeN)
	if err != nil {
		t.Fatalf("first Enrol: %v", err)
	}
	before, err := sqlc.New(f.st.Read).GetIngressNode(ctx, first.NodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}

	f.clock.Advance(time.Minute)
	f.freshList(envID, swarmNodeN)
	_, err = f.enrolOnce(t, tok.Token, "edge-b", swarmNodeN)
	if err == nil {
		t.Fatal("a second enrolment of the same swarm node was accepted")
	}
	re := refusal(t, err)
	if re.Code != ingresswire.RefuseNodeAlreadyEnrolled || re.AuditReason != "node_already_enrolled" {
		t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
	}
	if !strings.Contains(re.Detail, "already enrolled") {
		t.Errorf("detail = %q", re.Detail)
	}

	after, err := sqlc.New(f.st.Read).GetIngressNode(ctx, first.NodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the first row changed:\n before %+v\n after  %+v", before, after)
	}
	if n := f.countNodes(t, envID); n != 1 {
		t.Errorf("%d node rows, want 1", n)
	}
	if rows := f.auditRows(t, audit.ActionIngressNodeEnrolled); len(rows) != 1 {
		t.Errorf("%d %s rows, want 1", len(rows), audit.ActionIngressNodeEnrolled)
	}
}

// TestEnrolTwiceConcurrentlyOneWinner releases two enrolments of one node at once. Whichever of the pre-check and
// the unique index sees the other first, exactly one wins and the other is node_already_enrolled.
func TestEnrolTwiceConcurrentlyOneWinner(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, swarmNodeN)
	tok := f.issue(t, envID)

	// The requests are built before the goroutines start, so a failure in one is reported by the test itself.
	const racers = 2
	reqs := make([]ingresshub.EnrolRequest, racers)
	for i := range reqs {
		_, key := newKey(t)
		reqs[i] = enrolRequest(t, key, tok.Token, "edge", swarmNodeN)
	}
	start := make(chan struct{})
	errs := make(chan error, racers)
	for _, req := range reqs {
		go func() {
			<-start
			_, err := f.svc.Enrol(context.Background(), req)
			errs <- err
		}()
	}
	close(start)

	var wins, already int
	for range racers {
		err := <-errs
		switch err {
		case nil:
			wins++
		default:
			if re := refusal(t, err); re.Code != ingresswire.RefuseNodeAlreadyEnrolled || re.AuditReason != "node_already_enrolled" {
				t.Errorf("loser refused with code %q, audit %q", re.Code, re.AuditReason)
			} else {
				already++
			}
		}
	}
	if wins != 1 || already != 1 {
		t.Errorf("%d successes and %d node_already_enrolled, want 1 and 1", wins, already)
	}
	if n := f.activeNodes(t, envID); n != 1 {
		t.Errorf("%d active rows, want 1", n)
	}
	if n := f.countEnrolledAudits(t); n != 1 {
		t.Errorf("%d %s rows, want 1", n, audit.ActionIngressNodeEnrolled)
	}
}

func TestEnrolTwoNodesOfOneSwarmBothEnrol(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, "swarm-node-1", "swarm-node-2")
	tok := f.issue(t, envID)

	a, err := f.enrolOnce(t, tok.Token, "edge-a", "swarm-node-1")
	if err != nil {
		t.Fatalf("Enrol of the first node: %v", err)
	}
	b, err := f.enrolOnce(t, tok.Token, "edge-b", "swarm-node-2")
	if err != nil {
		t.Fatalf("Enrol of the second node: %v", err)
	}
	if a.NodeID == b.NodeID {
		t.Error("two nodes got one ID")
	}
	if n := f.activeNodes(t, envID); n != 2 {
		t.Errorf("%d active rows, want 2", n)
	}
}

func TestEnrolStandaloneOneSlot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "standalone")
	other := f.insertEnv(t, "standalone")
	tok := f.issue(t, envID)
	otherTok := f.issue(t, other)

	// A standalone node has no swarm node: it is stored NULL even if the node reported an ID.
	first, err := f.enrolOnce(t, tok.Token, "edge-a", "")
	if err != nil {
		t.Fatalf("first Enrol: %v", err)
	}
	row, err := sqlc.New(f.st.Read).GetIngressNode(ctx, first.NodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	if row.SwarmNodeID.Valid {
		t.Errorf("swarm_node_id = %+v, want NULL", row.SwarmNodeID)
	}

	for _, claimed := range []string{"", swarmNodeN} {
		_, err = f.enrolOnce(t, tok.Token, "edge-b", claimed)
		if err == nil {
			t.Fatalf("a second standalone enrolment (claiming %q) was accepted", claimed)
		}
		if re := refusal(t, err); re.Code != ingresswire.RefuseNodeAlreadyEnrolled || re.AuditReason != "node_already_enrolled" {
			t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
		}
	}
	if n := f.activeNodes(t, envID); n != 1 {
		t.Errorf("%d active rows, want 1", n)
	}

	// Another environment has a slot of its own.
	if _, err := f.enrolOnce(t, otherTok.Token, "edge-c", ""); err != nil {
		t.Fatalf("a standalone node of another environment: %v", err)
	}
}

func TestEnrolUnknownEnvironmentTypeIsRetryable(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "")
	tok := f.issue(t, envID)

	_, err := f.enrolOnce(t, tok.Token, "edge-a", swarmNodeN)
	if err == nil {
		t.Fatal("Enrol accepted a node of an environment whose type is unknown")
	}
	re := refusal(t, err)
	if re.Code != ingresswire.RefuseNodeUnverified || re.AuditReason != "environment_type_unknown" {
		t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
	}
	if re.RetryAfter != 15*time.Second {
		t.Errorf("RetryAfter = %v, want 15s", re.RetryAfter)
	}
	f.requireNothingWritten(t, envID)
}

func TestEnrolLocalEnvironmentIsNotRetryable(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnvKind(t, "local", "")
	tok := f.issue(t, envID)

	_, err := f.enrolOnce(t, tok.Token, "edge-a", swarmNodeN)
	if err == nil {
		t.Fatal("Enrol accepted a node of the local-socket environment")
	}
	re := refusal(t, err)
	if re.Code != ingresswire.RefuseAuthRejected || re.AuditReason != "environment_kind_local" {
		t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
	}
	if re.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want none: no wait makes it succeed", re.RetryAfter)
	}
	if !strings.Contains(re.Detail, "local-socket environment") {
		t.Errorf("detail = %q, want it to name the local-socket environment", re.Detail)
	}
	f.requireNothingWritten(t, envID)
}

func TestEnrolBlockedNodeIsRefusedNodeRevoked(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		envType  string
		claimed  string // the swarm node ID the node reports
		revoked  string // the swarm node ID of the revoked row, "" for the standalone slot
		blocked  int
		wantCode string // "" when the node enrols
	}{
		{"swarm node, blocked", "swarm", swarmNodeN, swarmNodeN, 1, ingresswire.RefuseNodeRevoked},
		{"swarm node, re-enrolment allowed", "swarm", swarmNodeN, swarmNodeN, 0, ""},
		{"swarm node, another node of the swarm is blocked", "swarm", swarmNodeN, "swarm-node-other", 1, ""},
		{"standalone slot, blocked", "standalone", "", "", 1, ingresswire.RefuseNodeRevoked},
		{"standalone slot, re-enrolment allowed", "standalone", "", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			envID := f.insertEnv(t, tc.envType)
			f.freshList(envID, swarmNodeN, "swarm-node-other")
			tok := f.issue(t, envID)
			f.insertRevoked(t, envID, tc.revoked, tc.blocked)

			res, err := f.enrolOnce(t, tok.Token, "edge-a", tc.claimed)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Enrol: %v", err)
				}
				row, err := sqlc.New(f.st.Read).GetIngressNode(ctx, res.NodeID)
				if err != nil || row.Status != "active" {
					t.Errorf("row = %+v (%v), want an active row", row, err)
				}
				return
			}
			if err == nil {
				t.Fatal("a blocked node was admitted again")
			}
			re := refusal(t, err)
			if re.Code != tc.wantCode || re.AuditReason != "node_revoked" {
				t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
			}
			if !strings.Contains(re.Detail, "revoked") {
				t.Errorf("detail = %q", re.Detail)
			}
			if n := f.activeNodes(t, envID); n != 0 {
				t.Errorf("%d active rows after a refused enrolment, want 0", n)
			}
			if n := f.countEnrolledAudits(t); n != 0 {
				t.Errorf("%d %s rows, want 0", n, audit.ActionIngressNodeEnrolled)
			}
		})
	}

	// The block is reached only after the token and the proof of possession: a bad token or proof is the uniform
	// refusal, which does not say the node was revoked.
	t.Run("a bad proof of possession is not told the node is blocked", func(t *testing.T) {
		f := newFixture(t)
		envID := f.insertEnv(t, "swarm")
		f.freshList(envID, swarmNodeN)
		tok := f.issue(t, envID)
		f.insertRevoked(t, envID, swarmNodeN, 1)

		_, key := newKey(t)
		req := enrolRequest(t, key, tok.Token, "edge-a", swarmNodeN)
		req.Sig = make([]byte, len(req.Sig))
		_, err := f.svc.Enrol(ctx, req)
		if err == nil {
			t.Fatal("Enrol accepted a bad proof of possession")
		}
		if re := refusal(t, err); re.Code != ingresswire.RefuseAuthRejected || re.AuditReason != "bad_proof_of_possession" {
			t.Errorf("refusal = code %q, audit %q", re.Code, re.AuditReason)
		}
	})
}

// TestEnrolAgainWithTheSameKeyGetsTheSameRow is CR-A-01: a node whose enrolment committed but whose welcome never
// arrived enrols again with the same key and is given the row it already has, for a swarm node and for the
// standalone slot. The repeat is audited and changes nothing on the row; another key is still refused (D-05).
func TestEnrolAgainWithTheSameKeyGetsTheSameRow(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, envType, claimed string
	}{
		{"swarm node", "swarm", swarmNodeN},
		{"standalone slot", "standalone", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			envID := f.insertEnv(t, tc.envType)
			f.freshList(envID, swarmNodeN)
			tok := f.issue(t, envID)
			_, key := newKey(t)

			first, err := f.svc.Enrol(ctx, enrolRequest(t, key, tok.Token, "edge-a", tc.claimed))
			if err != nil {
				t.Fatalf("first Enrol: %v", err)
			}
			before := f.row(t, first.NodeID)

			// The welcome was lost: the node only knows its key and enrols again, under another name even.
			f.clock.Advance(time.Minute)
			again, err := f.svc.Enrol(ctx, enrolRequest(t, key, tok.Token, "edge-renamed", tc.claimed))
			if err != nil {
				t.Fatalf("Enrol again with the same key: %v", err)
			}
			if again != first {
				t.Errorf("Enrol again = %+v, want the first enrolment %+v", again, first)
			}
			if after := f.row(t, first.NodeID); !reflect.DeepEqual(before, after) {
				t.Errorf("the row changed:\n before %+v\n after  %+v", before, after)
			}
			if n := f.countNodes(t, envID); n != 1 {
				t.Errorf("%d node rows, want 1", n)
			}
			rows := f.auditRows(t, audit.ActionIngressNodeEnrolled)
			if len(rows) != 2 {
				t.Fatalf("%d %s rows, want 2", len(rows), audit.ActionIngressNodeEnrolled)
			}
			if rows[1].TargetID != first.NodeID || !strings.Contains(rows[1].Details, `"repeated":"true"`) {
				t.Errorf("the repeat's audit row = %+v, want the same node with repeated true", rows[1])
			}
			if strings.Contains(rows[0].Details, "repeated") {
				t.Errorf("the first enrolment's audit row says repeated: %s", rows[0].Details)
			}

			// Another key for the same slot is still refused, and replaces nothing.
			if _, err := f.enrolOnce(t, tok.Token, "edge-b", tc.claimed); err == nil {
				t.Fatal("another key enrolled into a live slot")
			} else if re := refusal(t, err); re.Code != ingresswire.RefuseNodeAlreadyEnrolled {
				t.Errorf("another key refusal = %q, want node_already_enrolled", re.Code)
			}
			if after := f.row(t, first.NodeID); !reflect.DeepEqual(before, after) {
				t.Errorf("another key changed the row:\n before %+v\n after  %+v", before, after)
			}
		})
	}
}

// TestEnrolTwiceConcurrentlySameKeyOneRow releases two enrolments of one node with one key at once, as a retry
// that overlaps the first attempt does. Whichever of the pre-check and the unique index sees the other first,
// both are given the one row.
func TestEnrolTwiceConcurrentlySameKeyOneRow(t *testing.T) {
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")
	f.freshList(envID, swarmNodeN)
	tok := f.issue(t, envID)
	_, key := newKey(t)
	req := enrolRequest(t, key, tok.Token, "edge", swarmNodeN)

	const racers = 2
	start := make(chan struct{})
	type outcome struct {
		res ingresshub.EnrolResult
		err error
	}
	out := make(chan outcome, racers)
	for range racers {
		go func() {
			<-start
			res, err := f.svc.Enrol(context.Background(), req)
			out <- outcome{res, err}
		}()
	}
	close(start)
	a, b := <-out, <-out
	if a.err != nil || b.err != nil {
		t.Fatalf("Enrol errors = %v and %v, want both enrolled", a.err, b.err)
	}
	if a.res.NodeID != b.res.NodeID {
		t.Errorf("node IDs %q and %q, want one row for both", a.res.NodeID, b.res.NodeID)
	}
	if n := f.activeNodes(t, envID); n != 1 {
		t.Errorf("%d active rows, want 1", n)
	}
}
