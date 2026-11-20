// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// kick is one call of the recording kicker.
type kick struct{ nodeID, code string }

// recordingKicker records every Kick, and runs onKick (when set) while the call is made, so a test can look at
// the database at that instant.
type recordingKicker struct {
	mu     sync.Mutex
	kicks  []kick
	onKick func(nodeID string)
}

func (k *recordingKicker) Kick(nodeID, code string) {
	k.mu.Lock()
	k.kicks = append(k.kicks, kick{nodeID, code})
	k.mu.Unlock()
	if k.onKick != nil {
		k.onKick(nodeID)
	}
}

func (k *recordingKicker) calls() []kick {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.kicks)
}

// row reads a node's row from the database.
func (f *fixture) row(t *testing.T, nodeID string) sqlc.IngressNode {
	t.Helper()
	r, err := sqlc.New(f.st.Read).GetIngressNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("GetIngressNode: %v", err)
	}
	return r
}

func TestRevokeBlocksReenrol(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	got, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{Reason: "  lost\x00 the host "}, "192.0.2.1", "test-ua")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got.NodeID != a.res.NodeID || !got.RotateTokenRecommended || !got.RevokedAt.Equal(f.clock.Now()) {
		t.Errorf("Revoked = %+v", got)
	}

	r := f.row(t, a.res.NodeID)
	if r.Status != "revoked" || r.ReenrolBlocked != 1 || r.RevokedBy != "u-admin" || r.RevokeReason != "lost the host" || r.RevokedAt != f.clock.Now().UnixNano() {
		t.Errorf("row after revoke = status %q blocked %d by %q reason %q at %d", r.Status, r.ReenrolBlocked, r.RevokedBy, r.RevokeReason, r.RevokedAt)
	}

	// The join token is still mounted in the node's container: enrolling again with it is refused.
	_, err = f.svc.Enrol(ctx, enrolRequest(t, a.key, tok.Token, "edge-a", "node-1"))
	var re *ingresshub.RefuseError
	if !errors.As(err, &re) || re.Code != ingresswire.RefuseNodeRevoked {
		t.Fatalf("Enrol after revoke error = %v, want node_revoked", err)
	}
}

func TestRevokeAllowReenrol(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	if _, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{AllowReenrol: true}, "", ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if r := f.row(t, a.res.NodeID); r.ReenrolBlocked != 0 {
		t.Errorf("reenrol_blocked = %d, want 0 with AllowReenrol", r.ReenrolBlocked)
	}
	again := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	if again.res.NodeID == a.res.NodeID {
		t.Errorf("the node enrolled again under the revoked row's ID")
	}
	// The old row stays revoked and cannot come back.
	if f.svc.StillActive(a.res.NodeID) {
		t.Error("the revoked row is active again")
	}
}

func TestRevokeKicksAfterCommitAndOnlyThatNode(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	b := f.enrolNode(t, envID, tok.Token, "edge-b", "node-2")
	if err := f.svc.RecordSent(ctx, b.res.NodeID, []string{"b.example"}); err != nil {
		t.Fatalf("RecordSent: %v", err)
	}
	beforeB := f.row(t, b.res.NodeID)

	var statusAtKick string
	k := &recordingKicker{}
	k.onKick = func(nodeID string) { statusAtKick = f.row(t, nodeID).Status }
	if f.svc.WithKicker(k) != f.svc {
		t.Fatal("WithKicker does not return the service")
	}

	if _, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{}, "", ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	calls := k.calls()
	if len(calls) != 1 || calls[0] != (kick{a.res.NodeID, ingresswire.CloseRevoked}) {
		t.Fatalf("kicks = %+v, want one (A, %q)", calls, ingresswire.CloseRevoked)
	}
	if statusAtKick != "revoked" {
		t.Errorf("the row was %q when Kick ran, want revoked (the commit comes first)", statusAtKick)
	}
	if afterB := f.row(t, b.res.NodeID); !reflect.DeepEqual(beforeB, afterB) {
		t.Errorf("node B's row changed:\n before %+v\n after  %+v", beforeB, afterB)
	}
	if !f.svc.StillActive(b.res.NodeID) {
		t.Error("node B is no longer active")
	}

	// Without a kicker the revoke still commits.
	f2 := newFixture(t)
	env2, tok2 := f2.swarmEnv(t, "node-1")
	c := f2.enrolNode(t, env2, tok2.Token, "edge-c", "node-1")
	if _, err := f2.svc.Revoke(ctx, testAdmin, env2, c.res.NodeID, ingresshub.RevokeOptions{}, "", ""); err != nil {
		t.Fatalf("Revoke without a kicker: %v", err)
	}
	if f2.svc.StillActive(c.res.NodeID) {
		t.Error("the node is still active after a revoke without a kicker")
	}
}

func TestRevokeTwiceIsAlreadyRevoked(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	otherEnv, _ := f.swarmEnv(t, "node-9")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	k := &recordingKicker{}
	f.svc.WithKicker(k)

	if _, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{Reason: "first"}, "", ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{Reason: "second"}, "", ""); !errors.Is(err, ingresshub.ErrAlreadyRevoked) {
		t.Fatalf("second Revoke error = %v, want ErrAlreadyRevoked", err)
	}
	if rows := f.auditRows(t, audit.ActionIngressNodeRevoked); len(rows) != 1 {
		t.Errorf("%d revoke audit rows, want 1", len(rows))
	}
	if r := f.row(t, a.res.NodeID); r.RevokeReason != "first" {
		t.Errorf("revoke reason = %q, the first revoke's must stand", r.RevokeReason)
	}
	if n := len(k.calls()); n != 1 {
		t.Errorf("%d kicks, want 1", n)
	}

	// A node of another environment is not found, and an unknown environment says so.
	if _, err := f.svc.Revoke(ctx, testAdmin, otherEnv, a.res.NodeID, ingresshub.RevokeOptions{}, "", ""); !errors.Is(err, ingresshub.ErrNodeNotFound) {
		t.Errorf("Revoke through another environment error = %v, want ErrNodeNotFound", err)
	}
	if _, err := f.svc.Revoke(ctx, testAdmin, "00000000-0000-0000-0000-000000000000", a.res.NodeID, ingresshub.RevokeOptions{}, "", ""); !errors.Is(err, ingresshub.ErrEnvironmentNotFound) {
		t.Errorf("Revoke in an unknown environment error = %v, want ErrEnvironmentNotFound", err)
	}
	if _, err := f.svc.Revoke(ctx, testAdmin, envID, "00000000-0000-0000-0000-000000000001", ingresshub.RevokeOptions{}, "", ""); !errors.Is(err, ingresshub.ErrNodeNotFound) {
		t.Errorf("Revoke of an unknown node error = %v, want ErrNodeNotFound", err)
	}
}

func TestRevokeAuditsHeldCountAndSample(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	// Forty long hostnames: together far more than a 512 byte audit detail.
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("service-%02d-%s.example.com", i, strings.Repeat("x", 40)))
	}
	if err := f.svc.RecordSent(ctx, a.res.NodeID, names); err != nil {
		t.Fatalf("RecordSent: %v", err)
	}

	got, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{Reason: "decommissioned"}, "192.0.2.1", "ua")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !slices.Equal(got.HeldHostnames, names) {
		t.Errorf("HeldHostnames = %d names, want all %d sorted", len(got.HeldHostnames), len(names))
	}

	rows := f.auditRows(t, audit.ActionIngressNodeRevoked)
	if len(rows) != 1 {
		t.Fatalf("%d revoke audit rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ActorID != "u-admin" || r.TargetType != "ingress_node" || r.TargetID != a.res.NodeID || r.EnvID != envID {
		t.Errorf("audit row = %+v", r)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(r.Details), &d); err != nil {
		t.Fatalf("details %q: %v", r.Details, err)
	}
	if d["node"] != "edge-a" || d["swarm_node"] != "node-1" || d["reason"] != "decommissioned" || d["held"] != "40" || d["reenrol"] != "blocked" {
		t.Errorf("details = %v", d)
	}
	sample := d["sample"]
	if sample == "" || len(sample) > 480 {
		t.Fatalf("sample is %d bytes, want 1..480", len(sample))
	}
	parts := strings.Split(sample, ",")
	if len(parts) >= 40 {
		t.Errorf("the sample holds %d names; it should be cut", len(parts))
	}
	if !slices.Equal(parts, names[:len(parts)]) {
		t.Errorf("sample is not a whole-name prefix of the sorted list: %q", sample)
	}
	for k := range d {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "token") {
			t.Errorf("detail key %q names a secret", k)
		}
	}

	// AllowReenrol is told in the audit too.
	f2 := newFixture(t)
	env2, tok2 := f2.swarmEnv(t, "node-1")
	c := f2.enrolNode(t, env2, tok2.Token, "edge-c", "node-1")
	if _, err := f2.svc.Revoke(ctx, testAdmin, env2, c.res.NodeID, ingresshub.RevokeOptions{AllowReenrol: true}, "", ""); err != nil {
		t.Fatal(err)
	}
	var d2 map[string]string
	if err := json.Unmarshal([]byte(f2.auditRows(t, audit.ActionIngressNodeRevoked)[0].Details), &d2); err != nil {
		t.Fatal(err)
	}
	if d2["reenrol"] != "allowed" || d2["held"] != "0" {
		t.Errorf("details = %v, want reenrol allowed and held 0", d2)
	}
	if _, ok := d2["reason"]; ok {
		t.Errorf("reason present without one given: %v", d2)
	}
}

func TestHeldHostnamesGrowSortedAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	if err := f.svc.RecordSent(ctx, a.res.NodeID, []string{"b.example", "a.example", "b.example"}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RecordSent(ctx, a.res.NodeID, []string{"a.example", "c.example"}); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, a.res.NodeID).HeldHostnames; got != `["a.example","b.example","c.example"]` {
		t.Errorf("held_hostnames = %s", got)
	}

	// A send that adds nothing writes nothing: a sentinel placed straight in the column survives.
	const sentinel = `["a.example","b.example","c.example"]  `
	if _, err := f.st.Write.Exec(`UPDATE ingress_nodes SET held_hostnames = ? WHERE id = ?`, sentinel, a.res.NodeID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RecordSent(ctx, a.res.NodeID, []string{"c.example", "a.example"}); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, a.res.NodeID).HeldHostnames; got != sentinel {
		t.Errorf("held_hostnames = %q after a send that adds nothing, want it untouched", got)
	}
	// An empty send writes nothing either, and the set only grows.
	if err := f.svc.RecordSent(ctx, a.res.NodeID, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, a.res.NodeID).HeldHostnames; got != sentinel {
		t.Errorf("held_hostnames = %q after an empty send", got)
	}

	got, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.example", "b.example", "c.example"}; !slices.Equal(got.HeldHostnames, want) {
		t.Errorf("Revoked.HeldHostnames = %v, want %v", got.HeldHostnames, want)
	}
}

func TestHeldHostnamesEmptyIsAnEmptyList(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")

	got, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldHostnames == nil || len(got.HeldHostnames) != 0 {
		t.Errorf("HeldHostnames = %#v, want an empty non-nil list", got.HeldHostnames)
	}
	if b, _ := json.Marshal(got.HeldHostnames); string(b) != "[]" {
		t.Errorf("HeldHostnames encodes as %s, want []", b)
	}
	if !got.RotateTokenRecommended {
		t.Error("RotateTokenRecommended is false")
	}
}

// TestRecordSentRefusesARevokedNode is CR-A-02: once a revoke has committed, a bundle's hostnames are not written
// to the row and the hub is told not to send it, whether the send would grow the held set or not. The held set
// the revoke answered with stays the whole truth.
func TestRecordSentRefusesARevokedNode(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	if err := f.svc.RecordSent(ctx, a.res.NodeID, []string{"a.example"}); err != nil {
		t.Fatalf("RecordSent while active: %v", err)
	}

	got, err := f.svc.Revoke(ctx, testAdmin, envID, a.res.NodeID, ingresshub.RevokeOptions{}, "", "")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if want := []string{"a.example"}; !slices.Equal(got.HeldHostnames, want) {
		t.Fatalf("Revoked.HeldHostnames = %v, want %v", got.HeldHostnames, want)
	}

	// A bundle that would grow the set: the write transaction finds the row revoked.
	if err := f.svc.RecordSent(ctx, a.res.NodeID, []string{"a.example", "b.example"}); !errors.Is(err, ingresshub.ErrNodeNotActive) {
		t.Errorf("RecordSent of a new name after the revoke error = %v, want ErrNodeNotActive", err)
	}
	// A bundle that adds nothing: refused all the same, so the hub does not send it.
	if err := f.svc.RecordSent(ctx, a.res.NodeID, []string{"a.example"}); !errors.Is(err, ingresshub.ErrNodeNotActive) {
		t.Errorf("RecordSent of a held name after the revoke error = %v, want ErrNodeNotActive", err)
	}
	if held := f.row(t, a.res.NodeID).HeldHostnames; held != `["a.example"]` {
		t.Errorf("held_hostnames = %s after the revoke, want the list the revoke answered with", held)
	}
}
