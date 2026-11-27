// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
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

// errEngineDown is every call of a downEngine that is down.
var errEngineDown = errors.New("the environment's engine cannot be reached")

// downEngine is an engine that fails every call while down is set.
type downEngine struct {
	ingressdeploy.Engine
	down *atomic.Bool
}

func (e downEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	if e.down.Load() {
		return errEngineDown
	}
	return e.Engine.WriteOnManager(ctx, envID, fn)
}

func (e downEngine) WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	if e.down.Load() {
		return errEngineDown
	}
	return e.Engine.WriteOnNode(ctx, envID, nodeID, fn)
}

func (e downEngine) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	if e.down.Load() {
		return errEngineDown
	}
	return e.Engine.ClusterRead(ctx, envID, fn)
}

func (e downEngine) OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error {
	if e.down.Load() {
		return errEngineDown
	}
	return e.Engine.OnNode(ctx, envID, nodeID, fn)
}

// brokenSelfRig is a self route rig whose engine, TierPlan's own container read and certificate manager all fail once the
// returned switch is set, so every check of a preview fails or cannot be told.
func brokenSelfRig(t *testing.T) (*rig, ingress.Domain, *atomic.Bool) {
	t.Helper()
	broken := &atomic.Bool{}
	r, _, _, dom := newSelfRouteRig(t, selfRigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Engine = downEngine{Engine: d.Engine, down: broken}
		self, cert := d.Self, d.HostCertificate
		d.Self = func(ctx context.Context) (ingressdeploy.SelfFacts, error) {
			if broken.Load() {
				return ingressdeploy.SelfFacts{}, errEngineDown
			}
			return self(ctx)
		}
		d.HostCertificate = func(ctx context.Context, envID, host string) (ingressdeploy.HostCertState, error) {
			if broken.Load() {
				return ingressdeploy.HostCertState{}, errors.New("the certificate manager cannot be read")
			}
			return cert(ctx, envID, host)
		}
	}})
	return r, dom, broken
}

// claimSelfRoute stores TierPlan's own route in the rig's environment directly, with no job and no audit row.
func (r *rig) claimSelfRoute(dom ingress.Domain) {
	r.t.Helper()
	if err := r.istore.SetSystemClaim(testsupport.Context(r.t), ingressstore.SystemClaimInput{
		EnvID: r.envID, Hostname: srHost, DomainID: dom.ID, Service: tpServiceName, Port: 7480, Actor: "admin",
	}, nil); err != nil {
		r.t.Fatalf("SetSystemClaim: %v", err)
	}
}

// disableSelf starts a self route switch-off and returns its job's ID.
func (r *rig) disableSelf() string {
	r.t.Helper()
	id, err := r.svc.DisableSelfRoute(testsupport.Context(r.t), testActor, testOwner, r.envID)
	if err != nil {
		r.t.Fatalf("DisableSelfRoute: %v", err)
	}
	return id
}

// TestDisableSelfRouteAlwaysReleases is D-11's no-lockout proof (T-28-53): with ingress switched off, where TierPlan runs
// unknown, the certificate manager erroring and the environment's engine unreachable, so every check of a preview fails, the
// switch-off still clears the claim, writes one ingress.self_route_disabled row and ends done. With no route on it is
// ErrSelfRouteNotEnabled before any job starts.
func TestDisableSelfRouteAlwaysReleases(t *testing.T) {
	r, dom, broken := brokenSelfRig(t)
	ctx := testsupport.Context(t)
	r.claimSelfRoute(dom)
	if _, err := r.istore.SetEnabled(ctx, r.envID, false, "admin", nil); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	broken.Store(true)
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
	pv, err := r.svc.PreviewSelfRoute(ctx, r.envID, ingressdeploy.SelfRouteRequest{Hostname: srHost})
	if err != nil {
		t.Fatalf("PreviewSelfRoute: %v", err)
	}
	for _, code := range []string{ingressdeploy.ItemSelfHostname, ingressdeploy.ItemSelfNetwork} {
		for _, it := range pv.Items {
			if it.Code == code && it.State != ingressdeploy.SelfItemRefused {
				t.Fatalf("the rig is not broken enough: %s is %s", code, it.State)
			}
		}
	}
	r.svc.WakePending()
	selfWaitNone(r)

	job := r.wait(r.disableSelf())
	wantJobItems(t, job, jobs.JobDone,
		[2]string{ingressdeploy.ItemSelfRelease, jobs.ItemDone},
		[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemDone},
	)
	if it := itemOf(job, ingressdeploy.ItemSelfRelease); it.Result["removed"] != "true" || it.Result["host"] != srHost {
		t.Errorf("release item result = %v, want removed true for %s", it.Result, srHost)
	}
	if _, held, err := r.istore.SystemClaim(ctx); err != nil || held {
		t.Errorf("SystemClaim after the switch-off: held %v, err %v", held, err)
	}
	if !r.svc.WakePending() {
		t.Error("the switch-off did not wake the ensure loop")
	}
	if n := r.auditCount(audit.ActionIngressSelfRouteDisabled); n != 1 {
		t.Errorf("%d ingress.self_route_disabled rows, want 1", n)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the operation lock is still held after the job")
	}

	before := len(r.hub.List(func(jobs.Job) bool { return true }))
	if _, err := r.svc.DisableSelfRoute(ctx, testActor, testOwner, r.envID); !errors.Is(err, ingressdeploy.ErrSelfRouteNotEnabled) {
		t.Errorf("a second switch-off = %v, want ErrSelfRouteNotEnabled", err)
	}
	if after := len(r.hub.List(func(jobs.Job) bool { return true })); after != before {
		t.Errorf("a refused switch-off started a job: %d jobs, then %d", before, after)
	}
	if n := r.auditCount(audit.ActionIngressSelfRouteDisabled); n != 1 {
		t.Errorf("%d ingress.self_route_disabled rows after the refusal, want still 1", n)
	}
}

// TestAuditProofSelfRouteDisabled: a switch-off of a route the switch-on stored has one success row {host, environment} and
// no failure row, and its network item reports what the pass it woke found.
func TestAuditProofSelfRouteDisabled(t *testing.T) {
	r, _, _, _ := newSelfRouteRig(t, selfRigOptions{})
	selfWaitNone(r)
	wantJobItems(t, r.wait(r.enableSelf(srHost)), jobs.JobDone,
		[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemDone},
		[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemDone},
		[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemDone},
	)
	r.round()
	waiting := selfWaitSignal(r)

	id := r.disableSelf()
	receive(t, waiting, "the switch-off never waited for the self network")
	r.round()
	job := r.wait(id)
	wantJobItems(t, job, jobs.JobDone,
		[2]string{ingressdeploy.ItemSelfRelease, jobs.ItemDone},
		[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemDone},
	)
	switch got := itemOf(job, ingressdeploy.ItemSelfNetwork).Result["network"]; got {
	case "removed", "kept", "kept_in_use":
	default:
		t.Errorf("network item result %q, want removed, kept or kept_in_use", got)
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressSelfRouteDisabled)
	wantSuccessRow(t, rows, audit.ActionIngressSelfRouteDisabled, "host", "environment")
	if got := rows[0].Details; len(got) != 2 || got["host"] != srHost || got["environment"] != "prod" {
		t.Errorf("details = %v, want exactly host %s and environment prod", got, srHost)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
}

// TestAuditProofSelfRouteDisabledFailure is the proof of A-01 class F for the switch-off: a store that refuses the release
// with an answer holding a join token ends the job failed at self_release with self_route_release_failed, the network item not
// run and the route still on; the log gets one failure row with only the step and code.
func TestAuditProofSelfRouteDisabledFailure(t *testing.T) {
	r, _, _, dom := newSelfRouteRig(t, selfRigOptions{})
	ctx := testsupport.Context(t)
	r.claimSelfRoute(dom)
	if _, err := r.st.Write.Exec(`CREATE TRIGGER self_release_refused BEFORE DELETE ON ingress_hostnames BEGIN SELECT RAISE(ABORT, '` +
		switchCanary + `'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	selfWaitNone(r)

	job := r.wait(r.disableSelf())
	wantJobItems(t, job, jobs.JobFailed,
		[2]string{ingressdeploy.ItemSelfRelease, jobs.ItemFailed},
		[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemSkipped},
	)
	it := itemOf(job, ingressdeploy.ItemSelfRelease)
	if it.Code != ingressdeploy.CodeSelfRouteReleaseFailed || strings.Contains(it.Message, switchCanary) {
		t.Errorf("release item = %+v, want %s without the store's words", it, ingressdeploy.CodeSelfRouteReleaseFailed)
	}
	if _, held, err := r.istore.SystemClaim(ctx); err != nil || !held {
		t.Errorf("SystemClaim after a failed release: held %v, err %v, want still on", held, err)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the operation lock is still held after the job")
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressSelfRouteDisabled)
	wantFailureRow(t, rows, r.envID, audit.ActionIngressSelfRouteDisabled, ingressdeploy.ItemSelfRelease, ingressdeploy.CodeSelfRouteReleaseFailed)
	if len(rows) != 1 {
		t.Errorf("%d ingress.self_route_disabled rows, want only the failure: %+v", len(rows), rows)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
}

// certGate holds every certificate read of a self route check: each tells entered which environment it is for, then waits
// for proceed to be closed, so a test can hold switch-on jobs inside their checks.
type certGate struct {
	entered chan string
	proceed chan struct{}
}

// gatedSelfRig is a self route rig whose certificate reads pass through the gate the returned pointer holds, when it holds one.
func gatedSelfRig(t *testing.T) (*rig, ingress.Domain, *atomic.Pointer[certGate]) {
	t.Helper()
	gate := &atomic.Pointer[certGate]{}
	r, _, _, dom := newSelfRouteRig(t, selfRigOptions{deps: func(d *ingressdeploy.Deps) {
		cert := d.HostCertificate
		d.HostCertificate = func(ctx context.Context, envID, host string) (ingressdeploy.HostCertState, error) {
			if g := gate.Load(); g != nil {
				select {
				case g.entered <- envID:
				case <-ctx.Done():
					return ingressdeploy.HostCertState{}, ctx.Err()
				}
				select {
				case <-g.proceed:
				case <-ctx.Done():
					return ingressdeploy.HostCertState{}, ctx.Err()
				}
			}
			return cert(ctx, envID, host)
		}
	}})
	return r, dom, gate
}

// secondSelfEnv adds a second swarm environment, "staging", with ingress on and the domain other.example.org, to the rig.
func (r *rig) secondSelfEnv() string {
	r.t.Helper()
	ctx := testsupport.Context(r.t)
	id := uuid.NewString()
	if _, err := r.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES (?, 'agent', 'staging', '', 1, 1, 1, 'swarm')`, id); err != nil {
		r.t.Fatalf("insert environment: %v", err)
	}
	if _, err := r.istore.SetEnabled(ctx, id, true, "admin", nil); err != nil {
		r.t.Fatalf("SetEnabled: %v", err)
	}
	if _, err := r.istore.CreateDomain(ctx, ingressstore.CreateDomainInput{EnvID: id, Suffix: "other.example.org", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); err != nil {
		r.t.Fatalf("CreateDomain: %v", err)
	}
	return id
}

// TestTwoSelfRouteEnablesLeaveOne is SELF-01's concurrency edge: two switch-ons in one environment are serialised by its
// operation lock (the second is refused while the first runs, and one started after it is refused by its own check), and two in
// different environments that both pass their checks leave exactly one claim, the other job failing at self_claim with
// self_route_held and its failure row {step, code}. The test holds both jobs inside their checks with channels, no sleeps.
func TestTwoSelfRouteEnablesLeaveOne(t *testing.T) {
	t.Run("one environment", func(t *testing.T) {
		r, _, gate := gatedSelfRig(t)
		ctx := testsupport.Context(t)
		g := &certGate{entered: make(chan string, 1), proceed: make(chan struct{})}
		gate.Store(g)
		selfWaitNone(r)

		first := r.enableSelf(srHost)
		select {
		case <-g.entered:
		case <-ctx.Done():
			t.Fatal("the first switch-on never reached its check")
		}
		_, err := r.svc.EnableSelfRoute(ctx, testActor, testOwner, r.envID, ingressdeploy.SelfRouteRequest{Hostname: "tp2.apps.example.org"})
		var locked *ingressdeploy.LockedError
		if !errors.As(err, &locked) {
			t.Errorf("a second switch-on while the first runs = %v, want a *LockedError", err)
		}
		gate.Store(nil)
		close(g.proceed)
		wantJobItems(t, r.wait(first), jobs.JobDone,
			[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemDone},
			[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemDone},
			[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemDone},
		)

		later := r.wait(r.enableSelf("tp2.apps.example.org"))
		wantJobItems(t, later, jobs.JobFailed,
			[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemFailed},
			[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemSkipped},
			[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemSkipped},
		)
		if it := itemOf(later, ingressdeploy.ItemSelfCheck); it.Code != ingressdeploy.CodeSelfRouteAlreadyOn {
			t.Errorf("the later check = %+v, want %s", it, ingressdeploy.CodeSelfRouteAlreadyOn)
		}
		if claim, held, err := r.istore.SystemClaim(ctx); err != nil || !held || claim.Hostname != srHost {
			t.Errorf("SystemClaim = %+v, held %v, err %v, want the first switch-on's %s", claim, held, err, srHost)
		}
	})

	t.Run("two environments", func(t *testing.T) {
		r, _, gate := gatedSelfRig(t)
		ctx := testsupport.Context(t)
		env2 := r.secondSelfEnv()
		g := &certGate{entered: make(chan string, 2), proceed: make(chan struct{})}
		gate.Store(g)
		selfWaitNone(r)

		id1 := r.enableSelf(srHost)
		id2, err := r.svc.EnableSelfRoute(ctx, testActor, testOwner, env2, ingressdeploy.SelfRouteRequest{Hostname: "tp.other.example.org"})
		if err != nil {
			t.Fatalf("EnableSelfRoute in the second environment: %v", err)
		}
		// Both checks are running, so each has found the install without a route; then both go on to claim.
		for range 2 {
			select {
			case <-g.entered:
			case <-ctx.Done():
				t.Fatal("the two switch-ons never both reached their checks")
			}
		}
		close(g.proceed)
		jobsByEnv := map[string]jobs.Job{r.envID: r.wait(id1), env2: r.wait(id2)}

		claim, held, err := r.istore.SystemClaim(ctx)
		if err != nil || !held {
			t.Fatalf("SystemClaim: held %v, err %v, want one route", held, err)
		}
		winner := jobsByEnv[claim.EnvID]
		loserEnv := r.envID
		if claim.EnvID == r.envID {
			loserEnv = env2
		}
		loser := jobsByEnv[loserEnv]
		wantJobItems(t, winner, jobs.JobDone,
			[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemDone},
			[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemDone},
			[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemDone},
		)
		// The check went well before the claim was refused, so the hub reads the loser's job as partial: it did not switch on.
		wantJobItems(t, loser, jobs.JobPartial,
			[2]string{ingressdeploy.ItemSelfCheck, jobs.ItemDone},
			[2]string{ingressdeploy.ItemSelfClaim, jobs.ItemFailed},
			[2]string{ingressdeploy.ItemSelfNetwork, jobs.ItemSkipped},
		)
		it := itemOf(loser, ingressdeploy.ItemSelfClaim)
		if it.Code != ingressdeploy.CodeSelfRouteHeld {
			t.Errorf("the loser's claim item = %+v, want %s", it, ingressdeploy.CodeSelfRouteHeld)
		}
		if want := "TierPlan's own route is already on in another environment. Turn it off there first."; it.Message != want {
			t.Errorf("the loser's sentence = %q, want %q", it.Message, want)
		}

		rows := audittest.Rows(t, r.st, audit.ActionIngressSelfRouteEnabled)
		var failures []audit.Row
		for _, row := range rows {
			if row.Result == string(audit.ResultFailure) {
				failures = append(failures, row)
			}
		}
		if len(rows) != 2 || len(failures) != 1 {
			t.Fatalf("ingress.self_route_enabled rows = %+v, want one success and one failure", rows)
		}
		wantFailureRow(t, failures, loserEnv, audit.ActionIngressSelfRouteEnabled, ingressdeploy.ItemSelfClaim, ingressdeploy.CodeSelfRouteHeld)
		audittest.AssertNoSecrets(t, rows, switchCanary)
	})
}

// TestDisableSelfRouteWaitsForTheLock (D-11, WR-04): a stack operation holding TierPlan's environment does not refuse the
// switch-off. The job waits for the lock and releases once it is free; when it stays held past the bound, the job releases
// without it, since the release is one transaction of its own, and the other holder keeps its lock.
func TestDisableSelfRouteWaitsForTheLock(t *testing.T) {
	deploy := stackops.Holder{StackID: "st-1", StackName: "shop", Kind: "deploy", By: "someone", Started: time.Now()}
	t.Run("freed while it waits", func(t *testing.T) {
		r, _ := newSelfRig(t, selfRigOptions{deps: func(d *ingressdeploy.Deps) { d.SelfReleaseLockWait = time.Minute }})
		r.claimSelf()
		selfWaitNone(r)
		free, _, ok := r.locks.TryAcquire(r.envID, deploy)
		if !ok {
			t.Fatal("the rig's lock is held")
		}
		id := r.disableSelf()
		time.AfterFunc(50*time.Millisecond, free)
		job := r.wait(id)
		if it := itemOf(job, ingressdeploy.ItemSelfRelease); it.State != jobs.ItemDone || it.Result["removed"] != "true" {
			t.Fatalf("release item = %+v, want done and removed", it)
		}
		if _, held, err := r.istore.SystemClaim(testsupport.Context(t)); err != nil || held {
			t.Errorf("SystemClaim after the switch-off: held %v, err %v", held, err)
		}
		if _, held := r.locks.Held(r.envID); held {
			t.Error("the operation lock is still held after the job")
		}
	})
	t.Run("held past the bound", func(t *testing.T) {
		r, _ := newSelfRig(t, selfRigOptions{deps: func(d *ingressdeploy.Deps) { d.SelfReleaseLockWait = 50 * time.Millisecond }})
		r.claimSelf()
		selfWaitNone(r)
		free, _, ok := r.locks.TryAcquire(r.envID, deploy)
		if !ok {
			t.Fatal("the rig's lock is held")
		}
		defer free()
		job := r.wait(r.disableSelf())
		if it := itemOf(job, ingressdeploy.ItemSelfRelease); it.State != jobs.ItemDone || it.Result["removed"] != "true" {
			t.Fatalf("release item = %+v, want done and removed", it)
		}
		if _, held, err := r.istore.SystemClaim(testsupport.Context(t)); err != nil || held {
			t.Errorf("SystemClaim after the switch-off: held %v, err %v", held, err)
		}
		if h, held := r.locks.Held(r.envID); !held || h.StackID != "st-1" {
			t.Errorf("the deploy's lock = %+v, %v; want it still the deploy's", h, held)
		}
	})
}
