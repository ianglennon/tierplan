// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// claimProbe is an engine that, once armed, records at each manager write whether TierPlan's own route is still claimed, so a
// test sees what the switch-off released before it wrote anything to the swarm.
type claimProbe struct {
	ingressdeploy.Engine
	held func(ctx context.Context) bool

	mu     sync.Mutex
	armed  bool
	writes []bool
}

func (p *claimProbe) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	p.mu.Lock()
	armed := p.armed
	p.mu.Unlock()
	if armed {
		held := p.held(ctx)
		p.mu.Lock()
		p.writes = append(p.writes, held)
		p.mu.Unlock()
	}
	return p.Engine.WriteOnManager(ctx, envID, fn)
}

func (p *claimProbe) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
}

func (p *claimProbe) recorded() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.writes)
}

// claimOwnRoute registers apps.example.org in the rig's environment and stores TierPlan's own route on tp.apps.example.org there,
// with no job and no audit row.
func claimOwnRoute(r *rig) {
	r.t.Helper()
	dom, err := r.istore.CreateDomain(testsupport.Context(r.t), ingressstore.CreateDomainInput{EnvID: r.envID, Suffix: srDomain, Issuer: ingress.IssuerStaging, Actor: "admin"}, nil)
	if err != nil {
		r.t.Fatalf("CreateDomain: %v", err)
	}
	r.claimSelfRoute(dom)
}

// itemIDs are the IDs of a job's items, in order.
func itemIDs(job jobs.Job) []string {
	out := make([]string, 0, len(job.Items))
	for _, it := range job.Items {
		out = append(out, it.ID)
	}
	return out
}

// TestIngressDisableReleasesTheSelfRouteFirst is the tracer of D-11: switching ingress off on a swarm whose virtual IP and
// TierPlan's own route are on runs self_route first, which releases the claim with one ingress.self_route_disabled row before
// the virtual IP item writes anything; a held self route never refuses the switch-off; a release that fails ends the job at
// self_route with ingress still on, and only the switch-off audits it; a standalone host's job starts with self_route too.
func TestIngressDisableReleasesTheSelfRouteFirst(t *testing.T) {
	t.Run("swarm with the virtual IP", func(t *testing.T) {
		probe := &claimProbe{}
		f := newVIPDisableFix(t, vipDisableOpts{deps: func(d *ingressdeploy.Deps) {
			store := d.Store
			probe.Engine = d.Engine
			probe.held = func(ctx context.Context) bool {
				_, held, err := store.SystemClaim(ctx)
				return err != nil || held
			}
			d.Engine = probe
		}})
		wantAllDone(t, f.enable(ingressdeploy.EnableRequest{}))
		f.enabled()
		claimOwnRoute(f.rig)
		ctx := testsupport.Context(t)

		pv, err := f.svc.PreviewDisable(ctx, f.envID, seesAll)
		if err != nil {
			t.Fatalf("PreviewDisable: %v", err)
		}
		if len(pv.Findings) != 0 {
			t.Fatalf("preview findings = %+v, want none: TierPlan's own route never refuses a switch-off", pv.Findings)
		}

		probe.arm()
		job := f.disableIngress()
		if got := itemIDs(job); len(got) < 2 || !slices.Equal(got[:2], []string{"self_route", "virtual_ip"}) {
			t.Fatalf("items = %v, want self_route then virtual_ip first", got)
		}
		if job.State != jobs.JobDone {
			t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
		}
		if it := itemOf(job, ingressdeploy.ItemSelfRoute); it.State != jobs.ItemDone || it.Result["removed"] != "true" {
			t.Errorf("self_route = %q %v, want done and removed", it.State, it.Result)
		}
		if it := itemOf(job, ingressdeploy.ItemVirtualIP); it.State != jobs.ItemDone || it.Result["removed"] != "true" {
			t.Errorf("virtual_ip = %q %v, want done and removed", it.State, it.Result)
		}
		writes := probe.recorded()
		if len(writes) == 0 {
			t.Fatal("the switch-off wrote nothing to the swarm")
		}
		if writes[0] {
			t.Error("the claim was still held when the virtual IP item wrote first: the self route must go before it")
		}
		if _, held, err := f.istore.SystemClaim(ctx); err != nil || held {
			t.Errorf("SystemClaim after the switch-off: held %v, err %v", held, err)
		}
		rows := audittest.Rows(t, f.st, audit.ActionIngressSelfRouteDisabled)
		if len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) {
			t.Errorf("ingress.self_route_disabled rows = %+v, want one success", rows)
		}
		if st, err := f.istore.DeployState(ctx, f.envID); err != nil || st.Enabled {
			t.Errorf("ingress state = %+v, %v, want switched off", st, err)
		}
	})

	t.Run("no route on", func(t *testing.T) {
		f := newDisableFix(t, disableOpts{})
		job := f.disable(seesAll)
		wantDisableDone(t, job)
		if it := itemOf(job, ingressdeploy.ItemSelfRoute); it.Result["removed"] != "false" {
			t.Errorf("self_route result = %v, want nothing removed", it.Result)
		}
		if n := f.auditCount(audit.ActionIngressSelfRouteDisabled); n != 0 {
			t.Errorf("%d ingress.self_route_disabled rows, want none", n)
		}
	})

	t.Run("a failed release keeps ingress on", func(t *testing.T) {
		f := newDisableFix(t, disableOpts{})
		ctx := testsupport.Context(t)
		claimOwnRoute(f.rig)
		if _, err := f.st.Write.Exec(`CREATE TRIGGER self_release_refused BEFORE DELETE ON ingress_hostnames BEGIN SELECT RAISE(ABORT, '` +
			switchCanary + `'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		job := f.disable(seesAll)
		if job.State == jobs.JobDone {
			t.Fatalf("the job ended done with the release refused: %+v", job.Items)
		}
		it := itemOf(job, ingressdeploy.ItemSelfRoute)
		if it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeSelfRouteReleaseFailed {
			t.Errorf("self_route = %q %q, want failed with %s", it.State, it.Code, ingressdeploy.CodeSelfRouteReleaseFailed)
		}
		if !strings.Contains(it.Message, "ingress was left on") || strings.Contains(it.Message, switchCanary) {
			t.Errorf("self_route message = %q, want ingress left on and none of the store's words", it.Message)
		}
		for _, id := range itemIDs(job)[1:] {
			if it := itemOf(job, id); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
				t.Errorf("%s = %q %q, want skipped and not_run", id, it.State, it.Code)
			}
		}
		if st, err := f.istore.DeployState(ctx, f.envID); err != nil || !st.Enabled {
			t.Errorf("ingress state = %+v, %v, want still on", st, err)
		}
		if _, held, err := f.istore.SystemClaim(ctx); err != nil || !held {
			t.Errorf("SystemClaim after a failed release: held %v, err %v, want still on", held, err)
		}
		rows := audittest.Rows(t, f.st, audit.ActionIngressDisabled)
		wantFailureRow(t, rows, f.envID, audit.ActionIngressDisabled, ingressdeploy.ItemSelfRoute, ingressdeploy.CodeSelfRouteReleaseFailed)
		if n := f.auditCount(audit.ActionIngressSelfRouteDisabled); n != 0 {
			t.Errorf("%d ingress.self_route_disabled rows, want none: only the switch-off audits the failure", n)
		}
		audittest.AssertNoSecrets(t, rows, switchCanary)
		if _, held := f.locks.Held(f.envID); held {
			t.Error("the operation lock is still held after the job")
		}
	})

	t.Run("standalone", func(t *testing.T) {
		r := newStandaloneDisableRig(t, nil)
		claimOwnRoute(r.rig)
		job := r.disable()
		if got := itemIDs(job); len(got) == 0 || got[0] != ingressdeploy.ItemSelfRoute {
			t.Fatalf("items = %v, want self_route first", got)
		}
		wantStandaloneDisableDone(t, job)
		if it := itemOf(job, ingressdeploy.ItemSelfRoute); it.Result["removed"] != "true" {
			t.Errorf("self_route result = %v, want removed", it.Result)
		}
		if _, held, err := r.istore.SystemClaim(testsupport.Context(t)); err != nil || held {
			t.Errorf("SystemClaim after the switch-off: held %v, err %v", held, err)
		}
		if n := r.auditCount(audit.ActionIngressSelfRouteDisabled); n != 1 {
			t.Errorf("%d ingress.self_route_disabled rows, want 1", n)
		}
	})
}
