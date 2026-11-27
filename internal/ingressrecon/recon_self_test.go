// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"encoding/pem"
	"slices"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// selfHost is the hostname of TierPlan's own route in the self tests.
const selfHost = "tierplan.example.com"

// fakeSelf is a Deps.Self that answers for one environment only, with a trust bundle and own version a test
// moves. It counts the calls per environment.
type fakeSelf struct {
	env string

	mu    sync.Mutex
	trust string
	own   int64
	calls map[string]int
}

func newFakeSelf(env string, trust string, own int64) *fakeSelf {
	return &fakeSelf{env: env, trust: trust, own: own, calls: map[string]int{}}
}

// set moves TierPlan's own certificate: a new trust bundle at a new own version.
func (f *fakeSelf) set(trust string, own int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trust, f.own = trust, own
}

// called is how often Self was asked about envID.
func (f *fakeSelf) called(envID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[envID]
}

func (f *fakeSelf) Self(_ context.Context, envID string) (ingressrender.SelfInput, int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[envID]++
	if envID != f.env {
		return ingressrender.SelfInput{}, 0, false
	}
	return ingressrender.SelfInput{
		Host: selfHost, BackendHost: "tierplan", BackendPort: 7480, ServerName: selfHost,
		TrustPEM: trustPEM(f.trust), Attached: true,
	}, f.own, true
}

// trustPEM is a PEM CERTIFICATE block whose body is the label, which is all the renderer checks of a trust bundle.
func trustPEM(label string) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte(label)})
}

// claimSelf stores TierPlan's own route on selfHost in env-1 and makes the hostname servable.
func (r *rig) claimSelf() {
	r.t.Helper()
	r.certs.setServable(selfHost)
	if err := r.model.SetSystemClaim(context.Background(), ingressstore.SystemClaimInput{
		EnvID: rigEnv, Hostname: selfHost, DomainID: r.domainID(), Service: "tierplan", Port: 7480, Actor: "admin",
	}, nil); err != nil {
		r.t.Fatalf("claim %s for TierPlan: %v", selfHost, err)
	}
}

// startSelf is start with Deps.Self set to self.
func (rg *rig) startSelf(self *fakeSelf) *started {
	t := rg.t
	t.Helper()
	renders := make(chan renderEvent, 64)
	d := rg.deps(func(envID, id string, changed bool) { renders <- renderEvent{envID, id, changed} })
	d.Self = self.Self
	r := New(d)
	s := &started{r: r, renders: renders, changes: r.Changes().Subscribe()}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		testsupport.Await(t, "Run to return", done)
		s.changes.Close()
	})
	return s
}

// routerNames returns the name of every router in the bundle's dynamic file, sorted.
func routerNames(t *testing.T, b ingressbundle.Bundle) []string {
	t.Helper()
	for _, f := range b.Files {
		if f.Path != ingressbundle.DynamicFile {
			continue
		}
		var doc struct {
			HTTP struct {
				Routers map[string]any `yaml:"routers"`
			} `yaml:"http"`
		}
		if err := yaml.Unmarshal(f.Data, &doc); err != nil {
			t.Fatalf("the dynamic file does not parse: %v", err)
		}
		names := make([]string, 0, len(doc.HTTP.Routers))
		for n := range doc.HTTP.Routers {
			names = append(names, n)
		}
		slices.Sort(names)
		return names
	}
	t.Fatalf("the bundle has no %s", ingressbundle.DynamicFile)
	return nil
}

// trustFile returns the data of the bundle's trust file for TierPlan, or nil.
func trustFile(b ingressbundle.Bundle, label string) []byte {
	want := ingressbundle.CertsDir + "/" + ingressbundle.Checksum(trustPEM(label)) + ".crt"
	for _, f := range b.Files {
		if f.Path == want {
			return f.Data
		}
	}
	return nil
}

// D-14 / A-02 tracer: Deps.Self's input and own version reach the bundle of the environment that carries TierPlan:
// the tierplan-self router, the trust file and Version.Own.
func TestReconcilerRendersTheSelfInput(t *testing.T) {
	rg := newRig(t)
	rg.claimSelf()
	self := newFakeSelf(rigEnv, "trust 1", 42)
	s := rg.startSelf(self)
	if ev := s.awaitRender(t); !ev.changed || ev.envID != rigEnv {
		t.Fatalf("start render = %+v, want a new bundle for %s", ev, rigEnv)
	}
	s.awaitChange(t)

	b, ok := s.r.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle")
	}
	if names := routerNames(t, b); !slices.Contains(names, "tierplan-self") {
		t.Fatalf("routers = %v, want tierplan-self", names)
	}
	if b.Version.Own != 42 {
		t.Errorf("Version.Own = %d, want 42", b.Version.Own)
	}
	if trustFile(b, "trust 1") == nil {
		t.Error("the bundle has no trust file for TierPlan's certificate")
	}
	for _, sk := range s.r.Skipped(rigEnv) {
		if sk.Hostname == selfHost {
			t.Errorf("TierPlan's own route is skipped as %s", sk.Reason)
		}
	}
	if self.called(rigEnv) == 0 {
		t.Error("Deps.Self was never asked about the environment")
	}
}

// D-14: an environment that does not carry TierPlan renders with no self input and own version 0, to the very
// bundle a reconciler with no Deps.Self renders.
func TestOtherEnvironmentsAreUnchanged(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.certs.setServable("app.example.com")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

	before := New(rg.deps(nil))
	if err := before.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("render without Deps.Self: %v", err)
	}
	want, _ := before.Current(rigEnv)

	// TierPlan is carried by another environment.
	self := newFakeSelf("env-2", "trust 1", 42)
	d := rg.deps(nil)
	d.Self = self.Self
	after := New(d)
	if err := after.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("render with Deps.Self: %v", err)
	}
	got, ok := after.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle")
	}
	if self.called(rigEnv) == 0 {
		t.Error("Deps.Self was never asked about the environment")
	}
	if got.ID != want.ID {
		t.Errorf("bundle ID = %s, want %s, the one rendered with no Deps.Self", got.ID, want.ID)
	}
	if got.Version.Own != 0 {
		t.Errorf("Version.Own = %d, want 0", got.Version.Own)
	}
	if names := routerNames(t, got); slices.Contains(names, "tierplan-self") {
		t.Errorf("routers = %v, want no tierplan-self", names)
	}
}

// settledSelf is a running reconciler over env-1 carrying TierPlan, past its start render, with the poll armed.
func settledSelf(t *testing.T, rg *rig, self *fakeSelf) *started {
	t.Helper()
	rg.claimSelf()
	s := rg.startSelf(self)
	if ev := s.awaitRender(t); !ev.changed {
		t.Fatalf("the start render reported an unchanged bundle %s, want a new one", ev.id)
	}
	s.awaitChange(t)
	rg.clock.AwaitTimer(t, PollInterval)
	return s
}

// A-02 / T-28-47: TierPlan's own certificate changing, with the config and certs versions unchanged, moves the own
// version, and the next poll renders the new trust bundle. A poll with nothing moved renders nothing.
func TestOwnVersionChangeAloneRerenders(t *testing.T) {
	rg := newRig(t)
	self := newFakeSelf(rigEnv, "trust 1", 42)
	s := settledSelf(t, rg, self)
	first, _ := s.r.Current(rigEnv)

	self.set("trust 2", 43)
	rg.clock.Advance(PollInterval)
	if ev := s.awaitRender(t); !ev.changed || ev.envID != rigEnv {
		t.Fatalf("render after the certificate change = %+v, want a new bundle for %s", ev, rigEnv)
	}
	s.awaitChange(t)
	b, _ := s.r.Current(rigEnv)
	if b.Version.Own != 43 {
		t.Errorf("Version.Own = %d, want 43", b.Version.Own)
	}
	if b.Version.Config != first.Version.Config || b.Version.Certs != first.Version.Certs {
		t.Errorf("Version = %+v, want the config and certs versions of %+v", b.Version, first.Version)
	}
	if trustFile(b, "trust 2") == nil || trustFile(b, "trust 1") != nil {
		t.Error("the bundle does not carry the new trust file alone")
	}

	// Nothing moved: the poll looks and renders nothing. It arms its next timer after it has looked.
	rg.clock.AwaitTimer(t, PollInterval)
	rg.clock.Advance(PollInterval)
	rg.clock.AwaitTimer(t, PollInterval)
	assertNoRender(t, s)
	assertNothingAnnounced(t, s)
}

// 28-32: RenderedOwn is the own version of the environment's last rendered bundle, 0 before any and for an
// environment the reconciler does not know.
func TestRenderedOwnReportsTheLastRender(t *testing.T) {
	rg := newRig(t)
	rg.claimSelf()
	self := newFakeSelf(rigEnv, "trust 1", 42)
	d := rg.deps(nil)
	d.Self = self.Self
	r := New(d)
	if got := r.RenderedOwn(rigEnv); got != 0 {
		t.Fatalf("RenderedOwn before any render = %d, want 0", got)
	}

	ctx := context.Background()
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if got := r.RenderedOwn(rigEnv); got != 42 {
		t.Errorf("RenderedOwn after the first render = %d, want 42", got)
	}

	self.set("trust 2", 43)
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if got := r.RenderedOwn(rigEnv); got != 43 {
		t.Errorf("RenderedOwn after the second render = %d, want 43", got)
	}
	if got := r.RenderedOwn("env-unknown"); got != 0 {
		t.Errorf("RenderedOwn of an unknown environment = %d, want 0", got)
	}
}

// 28-UAT 9a: right after a restart TierPlan has not yet observed itself on its own network, so a first render would leave
// its own route out and the reconnecting proxies would apply that, dropping the route until the next ensure pass. While
// Deps.HoldFirstRender says so, the environment's first render waits: the reconciler holds no bundle, so the proxies keep
// serving their cached one. Once the hold lifts the render goes ahead, and a hold never withdraws a bundle already rendered.
func TestFirstRenderWaitsWhileHeld(t *testing.T) {
	rg := newRig(t)
	rg.claimSelf()
	self := newFakeSelf(rigEnv, "trust 1", 42)
	var held sync.Mutex
	hold := true
	d := rg.deps(nil)
	d.Self = self.Self
	d.HoldFirstRender = func(envID string) bool {
		held.Lock()
		defer held.Unlock()
		return hold && envID == rigEnv
	}
	r := New(d)
	ctx := context.Background()

	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("held render: %v", err)
	}
	if b, ok := r.Current(rigEnv); ok {
		t.Fatalf("a held first render published bundle %s, want none so the proxies keep their cache", b.ID)
	}
	if f := r.Failures(); len(f) != 0 {
		t.Errorf("a held first render was recorded as a failure: %+v", f)
	}

	held.Lock()
	hold = false
	held.Unlock()
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("render after the hold: %v", err)
	}
	b, ok := r.Current(rigEnv)
	if !ok {
		t.Fatal("no bundle once the hold lifted")
	}
	if names := routerNames(t, b); !slices.Contains(names, "tierplan-self") {
		t.Fatalf("routers = %v, want tierplan-self", names)
	}

	// A hold that comes back later changes nothing: the environment has its bundle, which renders keep current.
	held.Lock()
	hold = true
	held.Unlock()
	self.set("trust 2", 43)
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("render with a bundle held: %v", err)
	}
	if b, _ := r.Current(rigEnv); b.Version.Own != 43 {
		t.Errorf("Version.Own = %d, want 43: a hold must not stop the renders of an environment that has a bundle", b.Version.Own)
	}
}

// 28-UAT 9a: the poll visits an environment with no bundle every PollInterval, so a held first render goes ahead within one
// poll of the hold lifting.
func TestPollRendersOnceTheHoldLifts(t *testing.T) {
	rg := newRig(t)
	rg.claimSelf()
	self := newFakeSelf(rigEnv, "trust 1", 42)
	var held sync.Mutex
	hold := true
	renders := make(chan renderEvent, 64)
	d := rg.deps(func(envID, id string, changed bool) { renders <- renderEvent{envID, id, changed} })
	d.Self = self.Self
	d.HoldFirstRender = func(string) bool {
		held.Lock()
		defer held.Unlock()
		return hold
	}
	r := New(d)
	s := &started{r: r, renders: renders, changes: r.Changes().Subscribe()}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		testsupport.Await(t, "Run to return", done)
		s.changes.Close()
	})

	rg.clock.AwaitTimer(t, PollInterval)
	assertNoRender(t, s)
	held.Lock()
	hold = false
	held.Unlock()
	rg.clock.Advance(PollInterval)
	if ev := s.awaitRender(t); !ev.changed || ev.envID != rigEnv {
		t.Fatalf("render after the hold lifted = %+v, want a new bundle for %s", ev, rigEnv)
	}
	if _, ok := r.Current(rigEnv); !ok {
		t.Fatal("no bundle after the poll")
	}
}
