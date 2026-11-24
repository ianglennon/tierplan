// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// The drain's handoff of the virtual IP and the warnings that tell of it (D-25, VIP-04). The handoff itself is the
// ingressdeploy service's and is tested there; here a fake stands in for it, so the route's ordering, answer and audit
// are what is under test.

// fakeVIP is the virtual IP hook of the node routes a test sets up. holder is the node that holds the address.
type fakeVIP struct {
	t      *testing.T
	holder string
	// alone is what VIPHolds says of the holder.
	alone bool
	// result and err are what HandoffVIP answers for the holder.
	result  ingressdeploy.VIPHandoff
	err     error
	holdsOf error
	// seenUpdates is how many node updates the daemon had when HandoffVIP was called, -1 before any call.
	updates func() int

	mu          sync.Mutex
	calls       []string
	seenUpdates int
	deadline    time.Duration
}

func (v *fakeVIP) HandoffVIP(ctx context.Context, _, node string) (*ingressdeploy.VIPHandoff, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, node)
	if v.updates != nil {
		v.seenUpdates = v.updates()
	}
	if dl, ok := ctx.Deadline(); ok {
		v.deadline = time.Until(dl)
	}
	if node != v.holder {
		return nil, nil
	}
	if v.err != nil {
		return nil, v.err
	}
	r := v.result
	return &r, nil
}

func (v *fakeVIP) VIPHolds(_ context.Context, _, node string) (bool, bool, error) {
	if v.holdsOf != nil {
		return false, false, v.holdsOf
	}
	if node != v.holder {
		return false, false, nil
	}
	return true, v.alone, nil
}

func (v *fakeVIP) called() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

// naEnvVIP is naEnv with the virtual IP hook set before the router is built.
func naEnvVIP(t *testing.T, vip *fakeVIP) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(naServices()...)
	cfg.SwarmNodeID, cfg.SwarmClusterID = "node-1", "cluster-1"
	cfg.NodeRows = naNodes()
	cfg.Tasks = []swarm.Task{
		tkRunning("web-1", "svc-web", "node-4", 1, "c1", time.Minute),
		tkRunning("web-2", "svc-web", "node-5", 2, "c4", time.Minute),
	}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	vip.t = t
	vip.updates = func() int { return len(f.fake.NodeUpdates()) }
	f.nodeVIP = vip
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

// vipAudits lists the audit rows of the virtual IP handoffs, oldest first.
func (f *envFix) vipAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionIngressVIPHandoff)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDrainHandsOffTheVIP is the tracer of the drain hook (D-25): draining the node that holds the address moves it first, the
// drain is written after, the answer says where the address went, and the audit log has the handoff beside the drain.
func TestDrainHandsOffTheVIP(t *testing.T) {
	t.Parallel()
	vip := &fakeVIP{holder: "node-4", result: ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffMoved, Holder: "wrk-2", HolderNodeID: "node-5"}}
	f, e := naEnvVIP(t, vip)

	rec := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s, want 200 with the handoff", rec.code, rec.body)
	}
	var got struct {
		VIPHandoff *struct {
			Outcome string `json:"outcome"`
			Holder  string `json:"holder"`
		} `json:"vipHandoff"`
	}
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatalf("body %s: %v", rec.body, err)
	}
	if got.VIPHandoff == nil || got.VIPHandoff.Outcome != "moved" || got.VIPHandoff.Holder != "wrk-2" {
		t.Fatalf("vipHandoff = %+v, want moved to wrk-2", got.VIPHandoff)
	}

	if calls := vip.called(); len(calls) != 1 || calls[0] != "node-4" {
		t.Fatalf("handoff calls = %v, want one for node-4", calls)
	}
	if vip.seenUpdates != 0 {
		t.Errorf("the drain had been written %d times when the handoff ran, want it after the handoff", vip.seenUpdates)
	}
	if vip.deadline <= 0 || vip.deadline > ingressdeploy.VIPHandoffBound+5*time.Second {
		t.Errorf("handoff deadline = %s away, want about the bound", vip.deadline)
	}
	ups := f.fake.NodeUpdates()
	if len(ups) != 1 || ups[0].NodeID != "node-4" || ups[0].Spec.Availability != "drain" {
		t.Fatalf("updates = %+v, want the drain written once", ups)
	}

	rows := f.vipAudits(t)
	if len(rows) != 1 {
		t.Fatalf("handoff audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionIngressVIPHandoff || r.TargetType != "node" || r.TargetID != "node-4" || r.EnvID != e.ID || r.Result != string(audit.ResultSuccess) {
		t.Errorf("handoff audit row = %+v", r)
	}
	for k, want := range map[string]string{"node": "wrk-1", "holder": "wrk-2", "outcome": "moved"} {
		if got := r.Details[k]; got != want {
			t.Errorf("handoff detail %s = %q, want %q", k, got, want)
		}
	}
	if drain := f.nodeAudits(t); len(drain) != 1 || drain[0].Action != audit.ActionNodeAvailabilityChanged || drain[0].Details["code"] != "done" {
		t.Errorf("drain audit rows = %+v, want the drain's own row beside the handoff", drain)
	}
}
