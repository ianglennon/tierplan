// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// TestDrainOfANonHolderIsUnchanged pins that nothing else triggers the handoff (PROH-26-04): a drain of a node that does not
// hold the address, and a pause, activate, promote or demote of the one that does, answer 204 with no body as before and
// record no handoff.
func TestDrainOfANonHolderIsUnchanged(t *testing.T) {
	t.Parallel()
	vip := &fakeVIP{holder: "node-4", result: ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffMoved, Holder: "wrk-2"}}
	f, e := naEnvVIP(t, vip)

	// A drain of a node that holds nothing: the hook is asked and answers that it does not.
	rec := f.setAvailability(t, e, "node-5", `{"availability":"drain","version":4}`)
	if rec.code != http.StatusNoContent || len(rec.body) != 0 {
		t.Fatalf("drain of a non-holder = %d %q, want 204 with no body", rec.code, rec.body)
	}
	if calls := vip.called(); len(calls) != 1 || calls[0] != "node-5" {
		t.Fatalf("handoff calls = %v, want the one for the drained node", calls)
	}

	// Every other change to the holder is judged without the hook.
	for _, c := range []struct {
		name, path, body string
	}{
		{"pause", "availability", `{"availability":"pause","version":3}`},
		{"promote", "role", `{"role":"manager","version":4}`},
	} {
		var r *envRec
		if c.path == "role" {
			r = f.setRole(t, e, "node-4", c.body)
		} else {
			r = f.setAvailability(t, e, "node-4", c.body)
		}
		if r.code != http.StatusNoContent || len(r.body) != 0 {
			t.Fatalf("%s of the holder = %d %q, want 204 with no body", c.name, r.code, r.body)
		}
	}
	if calls := vip.called(); len(calls) != 1 {
		t.Fatalf("handoff calls = %v after a pause and a promote, want no more", calls)
	}
	// The pause bumped the node's version; read it fresh for the activate and the demote.
	ni := niOf(t, f.nodeImpact(t, e, "node-4", "pause"))
	if r := f.setAvailability(t, e, "node-4", fmt.Sprintf(`{"availability":"active","version":%d}`, ni.Version)); r.code != http.StatusNoContent || len(r.body) != 0 {
		t.Fatalf("activate of the holder = %d %q, want 204 with no body", r.code, r.body)
	}
	ni = niOf(t, f.nodeImpact(t, e, "node-4", "demote"))
	if r := f.setRole(t, e, "node-4", fmt.Sprintf(`{"role":"worker","version":%d}`, ni.Version)); r.code != http.StatusNoContent || len(r.body) != 0 {
		t.Fatalf("demote of the holder = %d %q, want 204 with no body", r.code, r.body)
	}
	if calls := vip.called(); len(calls) != 1 {
		t.Fatalf("handoff calls = %v, want only the first drain's", calls)
	}
	if rows := f.vipAudits(t); len(rows) != 0 {
		t.Errorf("handoff audit rows = %+v, want none", rows)
	}
}

// TestDrainRefusedRunsNoHandoff pins that a drain the confirm check refuses never reaches the hook, so no keepalived is
// stopped for a drain that will not be made.
func TestDrainRefusedRunsNoHandoff(t *testing.T) {
	t.Parallel()
	// node-1 is TierPlan's own node, so its drain needs its name typed.
	vip := &fakeVIP{holder: "node-1", result: ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffMoved, Holder: "mgr-2"}}
	f, e := naEnvVIP(t, vip)
	rec := f.setAvailability(t, e, "node-1", `{"availability":"drain","version":7}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
		t.Fatalf("unconfirmed drain = %d %s (%s), want 422 confirmation_required", rec.code, rec.body, rec.ptyp)
	}
	if calls := vip.called(); len(calls) != 0 {
		t.Fatalf("handoff calls = %v, want none for a refused drain", calls)
	}
	if rows := f.vipAudits(t); len(rows) != 0 || len(f.fake.NodeUpdates()) != 0 {
		t.Fatalf("handoff rows %+v and updates %+v, want neither", rows, f.fake.NodeUpdates())
	}
}

// TestDrainHandoffOutcomesAudited pins that every outcome is audited with the node, the holder and the outcome, that the
// drain's own row follows it, and that none of them turns the drain into an error (PROH-26-04).
func TestDrainHandoffOutcomesAudited(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		result     ingressdeploy.VIPHandoff
		err        error
		wantStatus int
		wantResult audit.Result
	}{
		{"moved", ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffMoved, Holder: "wrk-2"}, nil, http.StatusOK, audit.ResultSuccess},
		{"no_candidate", ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffNoCandidate}, nil, http.StatusOK, audit.ResultSuccess},
		{"agent_unreachable", ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffAgentUnreachable}, nil, http.StatusOK, audit.ResultFailure},
		{"timeout", ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffTimeout}, nil, http.StatusOK, audit.ResultFailure},
		// A hook that cannot even tell whether the node holds the address leaves the drain to be made as before.
		{"unknown", ingressdeploy.VIPHandoff{}, errors.New("the proxy table is unreadable"), http.StatusNoContent, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			vip := &fakeVIP{holder: "node-4", result: c.result, err: c.err}
			f, e := naEnvVIP(t, vip)
			rec := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`)
			if rec.code != c.wantStatus {
				t.Fatalf("status = %d: %s, want %d", rec.code, rec.body, c.wantStatus)
			}
			if ups := f.fake.NodeUpdates(); len(ups) != 1 || ups[0].Spec.Availability != "drain" {
				t.Fatalf("updates = %+v, want the drain made whatever the outcome", ups)
			}
			if c.err != nil {
				if len(rec.body) != 0 || len(f.vipAudits(t)) != 0 {
					t.Errorf("body %q and handoff rows %+v, want neither when the hook could not tell", rec.body, f.vipAudits(t))
				}
				return
			}
			var got struct {
				VIPHandoff struct {
					Outcome string `json:"outcome"`
					Holder  string `json:"holder"`
				} `json:"vipHandoff"`
			}
			if err := json.Unmarshal(rec.body, &got); err != nil {
				t.Fatalf("body %s: %v", rec.body, err)
			}
			if got.VIPHandoff.Outcome != c.name || got.VIPHandoff.Holder != c.result.Holder {
				t.Errorf("vipHandoff = %+v, want %s with holder %q", got.VIPHandoff, c.name, c.result.Holder)
			}
			rows := f.vipAudits(t)
			if len(rows) != 1 {
				t.Fatalf("handoff rows = %+v, want one", rows)
			}
			r := rows[0]
			if r.Result != string(c.wantResult) || r.Details["node"] != "wrk-1" || r.Details["holder"] != c.result.Holder || r.Details["outcome"] != c.name {
				t.Errorf("handoff row = %+v, want %s with node wrk-1 and holder %q", r, c.wantResult, c.result.Holder)
			}
			if c.wantResult == audit.ResultFailure && r.Reason != c.name {
				t.Errorf("handoff reason = %q, want %q", r.Reason, c.name)
			}
			// The handoff's row comes first, and the drain's own follows it.
			var order []string
			all, err := f.st.Read.Query(`SELECT action FROM audit_log WHERE action IN (?, ?) ORDER BY seq`, audit.ActionIngressVIPHandoff, audit.ActionNodeAvailabilityChanged)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = all.Close() }()
			for all.Next() {
				var a string
				if err := all.Scan(&a); err != nil {
					t.Fatal(err)
				}
				order = append(order, a)
			}
			if len(order) != 2 || order[0] != audit.ActionIngressVIPHandoff || order[1] != audit.ActionNodeAvailabilityChanged {
				t.Errorf("audit order = %v, want the handoff then the drain", order)
			}
		})
	}
}

// TestNodeImpactHoldsVIP pins the warnings of a drain of the node that holds the address (UI-SPEC S3, D-25): holds_vip with a
// ready peer, holds_vip_alone without one, neither of them asking for the name, and never on a pause, promote or demote.
func TestNodeImpactHoldsVIP(t *testing.T) {
	t.Parallel()
	impact := func(t *testing.T, vip *fakeVIP, node, action string) niBody {
		t.Helper()
		f, e := naEnvVIP(t, vip)
		return niOf(t, f.nodeImpact(t, e, node, action))
	}

	t.Run("a holder with a ready peer", func(t *testing.T) {
		t.Parallel()
		b := impact(t, &fakeVIP{holder: "node-4"}, "node-4", "drain")
		if len(b.Warnings) != 1 || b.Warnings[0] != "holds_vip" || b.NeedsConfirm {
			t.Fatalf("warnings %v needsConfirm %v, want holds_vip and no confirm", b.Warnings, b.NeedsConfirm)
		}
	})
	t.Run("a holder alone", func(t *testing.T) {
		t.Parallel()
		b := impact(t, &fakeVIP{holder: "node-4", alone: true}, "node-4", "drain")
		if len(b.Warnings) != 1 || b.Warnings[0] != "holds_vip_alone" || b.NeedsConfirm {
			t.Fatalf("warnings %v needsConfirm %v, want holds_vip_alone and no confirm", b.Warnings, b.NeedsConfirm)
		}
	})
	t.Run("a node that holds nothing", func(t *testing.T) {
		t.Parallel()
		b := impact(t, &fakeVIP{holder: "node-4"}, "node-5", "drain")
		if len(b.Warnings) != 0 || b.NeedsConfirm {
			t.Fatalf("warnings %v needsConfirm %v, want none", b.Warnings, b.NeedsConfirm)
		}
	})
	t.Run("another warning still asks for the name", func(t *testing.T) {
		t.Parallel()
		// node-1 is TierPlan's own node.
		b := impact(t, &fakeVIP{holder: "node-1"}, "node-1", "drain")
		if !slices.Equal(b.Warnings, []string{"runs_tierplan", "holds_vip"}) || !b.NeedsConfirm {
			t.Fatalf("warnings %v needsConfirm %v, want runs_tierplan and holds_vip, confirm needed", b.Warnings, b.NeedsConfirm)
		}
	})
	t.Run("a read that fails leaves the warnings off", func(t *testing.T) {
		t.Parallel()
		b := impact(t, &fakeVIP{holder: "node-4", holdsOf: errors.New("unreadable")}, "node-4", "drain")
		if len(b.Warnings) != 0 || b.NeedsConfirm {
			t.Fatalf("warnings %v needsConfirm %v, want none", b.Warnings, b.NeedsConfirm)
		}
	})
	t.Run("no virtual IP", func(t *testing.T) {
		t.Parallel()
		f, e := naEnv(t, nil)
		b := niOf(t, f.nodeImpact(t, e, "node-4", "drain"))
		if len(b.Warnings) != 0 {
			t.Fatalf("warnings %v, want none", b.Warnings)
		}
	})
	for _, action := range []string{"pause", "promote", "demote"} {
		t.Run(action+" of the holder", func(t *testing.T) {
			t.Parallel()
			node := "node-4"
			if action == "demote" {
				node = "node-2"
			}
			b := impact(t, &fakeVIP{holder: node}, node, action)
			for _, w := range b.Warnings {
				if w == "holds_vip" || w == "holds_vip_alone" {
					t.Fatalf("%s carries %v, want no virtual IP warning", action, b.Warnings)
				}
			}
		})
	}
}

// TestDrainWithAStaleVersionRunsNoHandoff pins WR-07: the version a drain carries comes from an earlier read of the node, and
// a stale one is the commonest reason the write is refused. It is refused with 409 node_changed before the hook runs, so no
// keepalived is stopped and no address moved for a drain that was not made.
func TestDrainWithAStaleVersionRunsNoHandoff(t *testing.T) {
	t.Parallel()
	vip := &fakeVIP{holder: "node-4", result: ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffMoved, Holder: "wrk-2"}}
	f, e := naEnvVIP(t, vip)

	// node-4 is at version 3; the drain was made on the strength of version 2.
	rec := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":2}`)
	if rec.code != http.StatusConflict || rec.ptyp != "node_changed" {
		t.Fatalf("stale drain = %d %s (%s), want 409 node_changed", rec.code, rec.body, rec.ptyp)
	}
	if calls := vip.called(); len(calls) != 0 {
		t.Fatalf("handoff calls = %v, want none for a drain that is refused", calls)
	}
	if rows := f.vipAudits(t); len(rows) != 0 {
		t.Errorf("handoff audit rows = %+v, want none", rows)
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 0 {
		t.Errorf("node updates = %+v, want none", ups)
	}
	if drain := f.nodeAudits(t); len(drain) != 1 || drain[0].Details["code"] != "node_changed" {
		t.Errorf("drain audit rows = %+v, want the refused attempt recorded as node_changed", drain)
	}

	// The same drain with the current version still hands off first and is written after.
	if r := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`); r.code != http.StatusOK {
		t.Fatalf("current drain = %d %s, want 200 with the handoff", r.code, r.body)
	}
	if calls := vip.called(); len(calls) != 1 {
		t.Fatalf("handoff calls = %v after the current drain, want one", calls)
	}
}
