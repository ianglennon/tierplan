// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// selfWaitSignal makes the self route jobs of r tell the test, through the returned channel, when they start waiting for the
// next observation of the self network; the wait then ends only by an observation or the job's end.
func selfWaitSignal(r *rig) <-chan struct{} {
	waiting := make(chan struct{}, 4)
	r.svc.SetSelfWaitBound(func() <-chan time.Time {
		waiting <- struct{}{}
		return nil
	})
	return waiting
}

// selfWaitNone makes the self route jobs of r stop waiting for the self network at once, as if SelfNetworkWait had passed.
func selfWaitNone(r *rig) {
	r.svc.SetSelfWaitBound(func() <-chan time.Time {
		c := make(chan time.Time)
		close(c)
		return c
	})
}

// receive waits for a signal on c, or fails the test when its context ends first.
func receive(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-testsupport.Context(t).Done():
		t.Fatalf("no signal: %s", what)
	}
}

// enableSelf starts a self route switch-on and returns its job's ID.
func (r *rig) enableSelf(host string) string {
	r.t.Helper()
	id, err := r.svc.EnableSelfRoute(testsupport.Context(r.t), testActor, testOwner, r.envID, ingressdeploy.SelfRouteRequest{Hostname: host})
	if err != nil {
		r.t.Fatalf("EnableSelfRoute: %v", err)
	}
	return id
}

// wantJobItems fails the test unless the job ended in state with its items, in order, in the states given.
func wantJobItems(t *testing.T, job jobs.Job, state string, items ...[2]string) {
	t.Helper()
	if job.State != state {
		t.Errorf("job state = %q, want %q: %+v", job.State, state, job.Items)
	}
	if len(job.Items) != len(items) {
		t.Fatalf("job items = %+v, want %v", job.Items, items)
	}
	for i, want := range items {
		if got := [2]string{job.Items[i].ID, job.Items[i].State}; got != want {
			t.Errorf("item %d = %v (code %q, %q), want %v", i, got, job.Items[i].Code, job.Items[i].Message, want)
		}
	}
}

// TestEnableSelfRouteStoresTheClaimAndAudits is the D-17 tracer and the proof of ingress.self_route_enabled: a switch-on in
// the environment TierPlan runs in as tierplan_tierplan stores the claim with the service and the listen port, writes one
// success row {host, environment} in the claim's transaction, wakes the ensure loop once, and ends done once the next pass
// reports the self network.
func TestEnableSelfRouteStoresTheClaimAndAudits(t *testing.T) {
	r, _, _, dom := newSelfRouteRig(t, selfRigOptions{})
	ctx := testsupport.Context(t)
	r.svc.WakePending()
	waiting := selfWaitSignal(r)

	id := r.enableSelf("TP.Apps.Example.org")
	receive(t, waiting, "the switch-on never waited for the self network")
	if !r.svc.WakePending() {
		t.Error("the ensure loop was not woken by the switch-on")
	}
	claim, held, err := r.istore.SystemClaim(ctx)
	if err != nil || !held {
		t.Fatalf("SystemClaim while the job waits: held %v, err %v", held, err)
	}
	r.round()
	job := r.wait(id)
	wantJobItems(t, job, jobs.JobDone,
		[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemDone},
		[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemDone},
		[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemDone},
	)
	if r.svc.WakePending() {
		t.Error("the switch-on woke the ensure loop more than once")
	}
	if claim.EnvID != r.envID || claim.Hostname != srHost || claim.DomainID != dom.ID || claim.Service != tpServiceName || claim.Port != 7480 {
		t.Errorf("claim = %+v, want %s in %s under %s to %s:7480", claim, srHost, r.envID, dom.ID, tpServiceName)
	}
	if claim.ClaimedBy != testActor.ID {
		t.Errorf("claimed by %q, want %q", claim.ClaimedBy, testActor.ID)
	}
	if _, ok := r.selfNetwork(); !ok {
		t.Error("the pass the switch-on woke made no self network")
	}
	if it := itemOf(job, ingressdeploy.ItemSelfNetwork); it.Result["network"] != ingress.SelfNetworkName {
		t.Errorf("network item result = %v, want the network named", it.Result)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the operation lock is still held after the job")
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressSelfRouteEnabled)
	wantSuccessRow(t, rows, audit.ActionIngressSelfRouteEnabled, "host", "environment")
	if got := rows[0].Details; len(got) != 2 || got["host"] != srHost || got["environment"] != "prod" {
		t.Errorf("details = %v, want exactly host %s and environment prod", got, srHost)
	}
	if rows[0].ActorName != testActor.Name || rows[0].EnvID != r.envID || rows[0].TargetID != r.envID {
		t.Errorf("row = %+v, want by %s on %s", rows[0], testActor.Name, r.envID)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
}

// TestAuditProofSelfRouteEnabledFailure is the proof of A-01 class F for the switch-on (T-28-55): the dialog's preview went
// ahead, then a stack took the hostname, so the check inside the job refuses. The job fails at self_check with the hostname
// item's code, the claim is not stored, the loop is not woken, and the log gets one failure row with only the step and code.
func TestAuditProofSelfRouteEnabledFailure(t *testing.T) {
	r, _, _, _ := newSelfRouteRig(t, selfRigOptions{})
	ctx := testsupport.Context(t)
	pv, err := r.svc.PreviewSelfRoute(ctx, r.envID, ingressdeploy.SelfRouteRequest{Hostname: srHost})
	if err != nil || pv.Refused() {
		t.Fatalf("the dialog's preview: refused %v, err %v", pv.Refused(), err)
	}
	if _, err := r.st.Write.Exec(`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('st1', ?, 'web', 'swarm', 'tester', 1)`, r.envID); err != nil {
		t.Fatalf("insert stack: %v", err)
	}
	decl := ingress.Declaration{Service: "web", Host: srHost, Label: "tierplan.ingress.host", Port: 80}
	if _, err := r.istore.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: r.envID, StackID: "st1", Actor: "admin", Decls: []ingress.Declaration{decl}}); err != nil {
		t.Fatalf("ReplaceStackRoutes: %v", err)
	}
	r.svc.WakePending()

	job := r.wait(r.enableSelf(srHost))
	wantJobItems(t, job, jobs.JobFailed,
		[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemFailed},
		[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemSkipped},
		[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemSkipped},
	)
	if it := itemOf(job, ingressdeploy.ItemSelfCheck); it.Code != ingress.CodeHostTaken || it.Message == "" {
		t.Errorf("check item = %+v, want code %s with the hostname item's sentence", it, ingress.CodeHostTaken)
	}
	if _, held, err := r.istore.SystemClaim(ctx); err != nil || held {
		t.Errorf("SystemClaim after a refused switch-on: held %v, err %v", held, err)
	}
	if r.svc.WakePending() {
		t.Error("a refused switch-on woke the ensure loop")
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressSelfRouteEnabled)
	wantFailureRow(t, rows, r.envID, audit.ActionIngressSelfRouteEnabled, ingressdeploy.ItemSelfCheck, ingress.CodeHostTaken)
	if len(rows) != 1 {
		t.Errorf("%d ingress.self_route_enabled rows, want only the failure: %+v", len(rows), rows)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
}
