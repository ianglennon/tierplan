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
