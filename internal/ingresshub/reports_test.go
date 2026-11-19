// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// sentinelNS is an instant no clock of a test reaches, planted straight in a column to show a later call did not
// write it.
const sentinelNS = int64(999)

func (f *fixture) setInt(t *testing.T, column, nodeID string, v int64) {
	t.Helper()
	// The column name is a constant of the calling test, not input.
	if _, err := f.st.Write.Exec(`UPDATE ingress_nodes SET `+column+` = ? WHERE id = ?`, v, nodeID); err != nil {
		t.Fatalf("set %s: %v", column, err)
	}
}

func TestReportsWriteOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	id := n.res.NodeID

	// Hello: the first one writes the version, the schemas and the time; the same again leaves the time alone.
	if err := f.svc.RecordHello(ctx, id, "0.1.0", []int{1}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.Version != "0.1.0" || r.Schemas != "[1]" || r.LastSeenAt != f.clock.Now().UnixNano() {
		t.Fatalf("after hello: version %q schemas %q last_seen %d", r.Version, r.Schemas, r.LastSeenAt)
	}
	f.setInt(t, "last_seen_at", id, sentinelNS)
	if err := f.svc.RecordHello(ctx, id, "0.1.0", []int{1}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.LastSeenAt != sentinelNS {
		t.Errorf("an unchanged hello wrote the row (last_seen_at %d)", r.LastSeenAt)
	}
	// A changed version writes. An overlong version is cut and a long schema list is cut.
	long := strings.Repeat("v", 200)
	many := make([]int, 40)
	for i := range many {
		many[i] = i + 1
	}
	if err := f.svc.RecordHello(ctx, id, long, many); err != nil {
		t.Fatal(err)
	}
	r := f.row(t, id)
	if len(r.Version) != ingresswire.MaxVersionLen || r.LastSeenAt == sentinelNS {
		t.Errorf("changed hello: version %d bytes, last_seen %d", len(r.Version), r.LastSeenAt)
	}
	wantSchemas, _ := json.Marshal(many[:ingresswire.MaxSchemas])
	if r.Schemas != string(wantSchemas) {
		t.Errorf("schemas = %s, want %s", r.Schemas, wantSchemas)
	}

	// Applied.
	if err := f.svc.RecordApplied(ctx, id, "bundle-1"); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.AppliedBundleID != "bundle-1" || r.AppliedAt != f.clock.Now().UnixNano() {
		t.Fatalf("applied = %q at %d", r.AppliedBundleID, r.AppliedAt)
	}
	f.setInt(t, "applied_at", id, sentinelNS)
	if err := f.svc.RecordApplied(ctx, id, "bundle-1"); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.AppliedAt != sentinelNS {
		t.Errorf("an unchanged applied report wrote the row")
	}
	if err := f.svc.RecordApplied(ctx, id, "bundle-2"); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.AppliedBundleID != "bundle-2" || r.AppliedAt == sentinelNS {
		t.Errorf("a new applied bundle was not written: %q at %d", r.AppliedBundleID, r.AppliedAt)
	}

	// Rejected, the reason cut to the wire limit.
	if err := f.svc.RecordRejected(ctx, id, "bundle-3", strings.Repeat("é", 400)); err != nil {
		t.Fatal(err)
	}
	r = f.row(t, id)
	if r.RejectedBundleID != "bundle-3" || len(r.RejectedReason) > ingresswire.MaxReasonLen || len(r.RejectedReason) < ingresswire.MaxReasonLen-1 {
		t.Fatalf("rejected = %q, reason %d bytes", r.RejectedBundleID, len(r.RejectedReason))
	}
	if !strings.HasSuffix(r.RejectedReason, "é") {
		t.Errorf("the reason was cut inside a character")
	}
	f.setInt(t, "rejected_at", id, sentinelNS)
	if err := f.svc.RecordRejected(ctx, id, "bundle-3", strings.Repeat("é", 400)); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.RejectedAt != sentinelNS {
		t.Errorf("an unchanged rejected report wrote the row")
	}
	if err := f.svc.RecordRejected(ctx, id, "bundle-3", "another reason"); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.RejectedReason != "another reason" || r.RejectedAt == sentinelNS {
		t.Errorf("a changed reason was not written")
	}

	// Health.
	if err := f.svc.RecordHealth(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.Health != "ok" || r.HealthAt != f.clock.Now().UnixNano() {
		t.Fatalf("health = %q at %d", r.Health, r.HealthAt)
	}
	f.setInt(t, "health_at", id, sentinelNS)
	if err := f.svc.RecordHealth(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.HealthAt != sentinelNS {
		t.Errorf("an unchanged health report wrote the row")
	}
	if err := f.svc.RecordHealth(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if r := f.row(t, id); r.Health != "unhealthy" || r.HealthAt == sentinelNS {
		t.Errorf("a changed health was not written: %q", r.Health)
	}

	// Reports about a node that does not exist are ErrNodeNotFound.
	if err := f.svc.RecordHealth(ctx, "00000000-0000-0000-0000-000000000000", true); !errors.Is(err, ingresshub.ErrNodeNotFound) {
		t.Errorf("health of an unknown node error = %v", err)
	}
}

func TestTouchLastSeenOncePerMinute(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2")
	a := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	b := f.enrolNode(t, envID, tok.Token, "edge-b", "node-2")
	lastSeen := func(id string) int64 { return f.row(t, id).LastSeenAt }

	f.svc.TouchLastSeen(ctx, a.res.NodeID)
	if got := lastSeen(a.res.NodeID); got != f.clock.Now().UnixNano() {
		t.Fatalf("first touch wrote %d, want the clock", got)
	}
	f.setInt(t, "last_seen_at", a.res.NodeID, sentinelNS)
	f.clock.Advance(59 * time.Second)
	f.svc.TouchLastSeen(ctx, a.res.NodeID)
	if got := lastSeen(a.res.NodeID); got != sentinelNS {
		t.Errorf("a touch within a minute wrote %d", got)
	}
	// The limit is per node: another node is written at once.
	f.svc.TouchLastSeen(ctx, b.res.NodeID)
	if got := lastSeen(b.res.NodeID); got != f.clock.Now().UnixNano() {
		t.Errorf("node B was not written: %d", got)
	}
	f.clock.Advance(time.Second)
	f.svc.TouchLastSeen(ctx, a.res.NodeID)
	if got := lastSeen(a.res.NodeID); got != f.clock.Now().UnixNano() {
		t.Errorf("a touch after a minute wrote %d, want the clock", got)
	}
	// An unknown node writes nothing and does not fail.
	f.svc.TouchLastSeen(ctx, "00000000-0000-0000-0000-000000000000")
}

func TestReportsWriteNoAuditRows(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1")
	n := f.enrolNode(t, envID, tok.Token, "edge-a", "node-1")
	id := n.res.NodeID
	count := func() int {
		var c int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := count()

	for _, err := range []error{
		f.svc.RecordHello(ctx, id, "0.1.0", []int{1}),
		f.svc.RecordSent(ctx, id, []string{"a.example"}),
		f.svc.RecordApplied(ctx, id, "bundle-1"),
		f.svc.RecordRejected(ctx, id, "bundle-2", "no"),
		f.svc.RecordHealth(ctx, id, false),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.svc.TouchLastSeen(ctx, id)
	if after := count(); after != before {
		t.Errorf("the reports wrote %d audit rows", after-before)
	}
}

func TestListNodesOrderedAndIncompatibleDerived(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID, tok := f.swarmEnv(t, "node-1", "node-2", "node-3")
	// The clock stands still, so the three nodes are created in the same nanosecond.
	var ids []string
	for i, sn := range []string{"node-1", "node-2", "node-3"} {
		ids = append(ids, f.enrolNode(t, envID, tok.Token, "edge-"+string(rune('a'+i)), sn).res.NodeID)
	}
	// Another environment's node is not listed.
	otherEnv, otherTok := f.swarmEnv(t, "node-9")
	f.enrolNode(t, otherEnv, otherTok.Token, "elsewhere", "node-9")

	if err := f.svc.RecordHello(ctx, ids[0], "0.1.0", ingressrender.Schemas()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RecordHello(ctx, ids[1], "9.9.9", []int{7}); err != nil {
		t.Fatal(err)
	}
	// ids[2] never said hello.
	if err := f.svc.RecordSent(ctx, ids[0], []string{"b.example", "a.example"}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RecordApplied(ctx, ids[0], "bundle-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Revoke(ctx, testAdmin, envID, ids[2], ingresshub.RevokeOptions{Reason: "gone"}, "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.ListNodes(ctx, envID)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(ids)
	slices.Sort(want)
	if len(got) != 3 {
		t.Fatalf("%d nodes, want 3", len(got))
	}
	for i, n := range got {
		if n.ID != want[i] {
			t.Errorf("node %d is %s, want %s (created_at then id)", i, n.ID, want[i])
		}
	}
	byID := map[string]ingresshub.Node{}
	for _, n := range got {
		byID[n.ID] = n
	}

	ok := byID[ids[0]]
	if ok.Incompatible || ok.IncompatibleReason != "" || ok.Status != "active" || ok.Name != "edge-a" || ok.SwarmNodeID != "node-1" ||
		ok.Version != "0.1.0" || !slices.Equal(ok.Schemas, ingressrender.Schemas()) || ok.AppliedBundleID != "bundle-1" ||
		!slices.Equal(ok.HeldHostnames, []string{"a.example", "b.example"}) || !ok.AppliedAt.Equal(f.clock.Now()) ||
		!ok.CreatedAt.Equal(f.clock.Now()) || !ok.RejectedAt.IsZero() || !ok.RevokedAt.IsZero() || ok.EnvID != envID {
		t.Errorf("compatible node = %+v", ok)
	}
	bad := byID[ids[1]]
	if !bad.Incompatible || bad.IncompatibleReason != ingressrender.CodeSchemaUnsupported || !slices.Equal(bad.Schemas, []int{7}) {
		t.Errorf("node with schemas [7] = %+v, want incompatible with %s", bad, ingressrender.CodeSchemaUnsupported)
	}
	silent := byID[ids[2]]
	if silent.Incompatible || len(silent.Schemas) != 0 || silent.Schemas == nil {
		t.Errorf("node that never said hello = %+v, want no schemas and compatible", silent)
	}
	if silent.Status != "revoked" || silent.RevokedBy != "u-admin" || silent.RevokeReason != "gone" || !silent.ReenrolBlocked ||
		!silent.RevokedAt.Equal(f.clock.Now()) || silent.HeldHostnames == nil {
		t.Errorf("revoked node = %+v", silent)
	}

	// An environment with no nodes lists none.
	empty, err := f.svc.ListNodes(ctx, f.insertEnv(t, "swarm"))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty environment: %v, %v", empty, err)
	}
}
