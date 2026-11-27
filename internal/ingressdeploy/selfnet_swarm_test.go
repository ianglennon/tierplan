// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The swarm service TierPlan runs as in these tests.
const (
	tpServiceID   = "tp-svc-id"
	tpServiceName = "tierplan_tierplan"
)

// selfFacts is what the local engine says of TierPlan's own container; a test flips Networks to put TierPlan on the self
// network or take it off.
type selfFacts struct {
	mu    sync.Mutex
	facts ingressdeploy.SelfFacts
	// err makes the read fail, as a busy local socket does.
	err error
}

func (f *selfFacts) read(context.Context) (ingressdeploy.SelfFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return ingressdeploy.SelfFacts{}, f.err
	}
	out := f.facts
	out.Networks = slices.Clone(f.facts.Networks)
	return out, nil
}

func (f *selfFacts) setAttached(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts.Networks = []string{"tierplan_default"}
	if on {
		f.facts.Networks = append(f.facts.Networks, ingress.SelfNetworkName)
	}
}

// selfRigOptions tune a self rig.
type selfRigOptions struct {
	config func(*dockertest.FakeConfig)
	deps   func(*ingressdeploy.Deps)
}

// newSelfRig is an enabled swarm rig in which TierPlan runs as the swarm service tierplan_tierplan on the manager node, with
// that service in the fake swarm.
func newSelfRig(t *testing.T, o selfRigOptions) (*rig, *selfFacts) {
	t.Helper()
	facts := &selfFacts{facts: ingressdeploy.SelfFacts{
		ContainerID: "c1", Swarm: true, ServiceName: tpServiceName, ServiceID: tpServiceID, StackNamespace: "tierplan",
		Networks: []string{"tierplan_default"},
	}}
	r := enabledRig(t, rigOptions{
		config: func(c *dockertest.FakeConfig) {
			c.ServiceRows = append(c.ServiceRows, dockertest.FakeService{
				ID: tpServiceID, Name: tpServiceName, Running: 1, Desired: 1,
				Labels: map[string]string{"com.docker.stack.namespace": "tierplan"}, Networks: []string{"tierplan_default"},
			})
			if o.config != nil {
				o.config(c)
			}
		},
		deps: func(d *ingressdeploy.Deps) {
			d.Self = facts.read
			d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) {
				return envs.SwarmMembership{Known: true, Member: true, ClusterID: testClusterID, NodeID: "node-1", EngineID: "engine-1"}, nil
			}
			if o.deps != nil {
				o.deps(d)
			}
		},
	})
	return r, facts
}

// claimSelf switches TierPlan's own route on in the rig's environment.
func (r *rig) claimSelf() {
	r.t.Helper()
	ctx := testsupport.Context(r.t)
	dom, err := r.istore.CreateDomain(ctx, ingressstore.CreateDomainInput{EnvID: r.envID, Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil)
	if err != nil {
		r.t.Fatalf("CreateDomain: %v", err)
	}
	if err := r.istore.SetSystemClaim(ctx, ingressstore.SystemClaimInput{
		EnvID: r.envID, Hostname: "tp.apps.example.com", DomainID: dom.ID, Service: tpServiceName, Port: 7480, Actor: "admin",
	}, nil); err != nil {
		r.t.Fatalf("SetSystemClaim: %v", err)
	}
}

// unclaimSelf switches TierPlan's own route off.
func (r *rig) unclaimSelf() {
	r.t.Helper()
	if _, ok, err := r.istore.ClearSystemClaim(testsupport.Context(r.t), r.envID, "admin", time.Time{}, nil); err != nil || !ok {
		r.t.Fatalf("ClearSystemClaim: ok %v, err %v", ok, err)
	}
}

// selfNetwork is the network of the reserved name the fake swarm holds.
func (r *rig) selfNetwork() (dockerx.NetworkRow, bool) {
	r.t.Helper()
	rows, err := r.cli.Networks(testsupport.Context(r.t))
	if err != nil {
		r.t.Fatalf("Networks: %v", err)
	}
	for _, n := range rows {
		if n.Name == ingress.SelfNetworkName {
			return n, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// proxyNetworks are the names of the networks the live proxy spec attaches to, in its order.
func (r *rig) proxyNetworks() []string {
	r.t.Helper()
	rows, err := r.cli.Networks(testsupport.Context(r.t))
	if err != nil {
		r.t.Fatalf("Networks: %v", err)
	}
	var out []string
	for _, a := range r.liveSpec().TaskTemplate.Networks {
		name := a.Target
		for _, n := range rows {
			if n.ID == a.Target {
				name = n.Name
			}
		}
		out = append(out, name)
	}
	return out
}

// TestEnsureCreatesTheSelfNetworkAndAttachesTheProxy: with no reason for it, no self network is made and the proxy has one
// network; once TierPlan's own route is on, one pass makes tierplan-self (a swarm overlay that is not attachable, with the
// self labels and a /24) and attaches the proxy to it beside tierplan-ingress, and a second pass writes nothing (D-02).
func TestEnsureCreatesTheSelfNetworkAndAttachesTheProxy(t *testing.T) {
	r, _ := newSelfRig(t, selfRigOptions{deps: func(d *ingressdeploy.Deps) {
		d.OwnHTTP01InUse = func(context.Context) (bool, error) { return false, nil }
	}})

	r.round()
	if _, ok := r.selfNetwork(); ok {
		t.Fatal("a pass with no system claim and no HTTP-01 renewal made the self network")
	}
	if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName}) {
		t.Fatalf("proxy networks with no self network wanted = %v, want only %s", got, ingress.NetworkName)
	}

	r.claimSelf()
	r.round()
	row, ok := r.selfNetwork()
	if !ok {
		t.Fatal("one pass with the system claim held did not make the self network")
	}
	if row.Driver != "overlay" || row.Scope != dockerx.ScopeSwarm || row.Attachable {
		t.Errorf("self network = driver %q scope %q attachable %v, want an overlay of swarm scope that is not attachable", row.Driver, row.Scope, row.Attachable)
	}
	if !maps.Equal(row.Labels, ingress.SelfNetworkLabels()) {
		t.Errorf("self network labels = %v, want %v", row.Labels, ingress.SelfNetworkLabels())
	}
	if len(row.Subnets) != 1 {
		t.Fatalf("self network subnets = %v, want one", row.Subnets)
	}
	if p, err := netip.ParsePrefix(row.Subnets[0]); err != nil || p.Bits() != ingressdeploy.SelfNetworkPrefix {
		t.Errorf("self network subnet = %q, want a /%d", row.Subnets[0], ingressdeploy.SelfNetworkPrefix)
	}
	if ing, ok := r.network(); ok && slices.Contains(ing.Subnets, row.Subnets[0]) {
		t.Errorf("the self network took the ingress network's subnet %s", row.Subnets[0])
	}
	creates := r.fake.NetworkCreates()
	last := creates[len(creates)-1]
	if last.Name != ingress.SelfNetworkName || last.Attachable {
		t.Errorf("last network create = %q attachable %v, want %s not attachable", last.Name, last.Attachable, ingress.SelfNetworkName)
	}
	if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName, ingress.SelfNetworkName}) {
		t.Errorf("proxy networks = %v, want %s and %s", got, ingress.NetworkName, ingress.SelfNetworkName)
	}

	written := len(r.fake.Writes())
	r.round()
	if got := r.fake.Writes(); len(got) != written {
		t.Errorf("a second pass wrote %v, want nothing", got[written:])
	}
}

// TestEnsureNeverTouchesTierPlansOwnService: whatever the self network does, TierPlan's own swarm service is never updated: a
// stop-first update of it would kill the task that sent it (D-03).
func TestEnsureNeverTouchesTierPlansOwnService(t *testing.T) {
	r, facts := newSelfRig(t, selfRigOptions{})
	r.claimSelf()
	for range 3 {
		r.round()
	}
	facts.setAttached(true)
	r.unclaimSelf()
	for range 3 {
		r.round()
	}
	if _, ok := r.selfNetwork(); !ok {
		t.Fatal("the self network was not made, so the test proved nothing")
	}
	for _, u := range r.fake.ServiceUpdates() {
		if u.ServiceID == tpServiceID || u.Spec.Name == tpServiceName {
			t.Errorf("the ensure loop updated TierPlan's own service: %s %q", u.ServiceID, u.Spec.Name)
		}
	}
	for _, w := range r.fake.Writes() {
		if strings.Contains(w, tpServiceID) || strings.Contains(w, tpServiceName) {
			t.Errorf("the ensure loop wrote to TierPlan's own service: %s", w)
		}
	}
}

// logBuffer is a log sink the passes and the test may use at once.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// premadeSelf is a network of the reserved name that is in the swarm before the test starts.
func premadeSelf(labels map[string]string, subnet, gateway string) dockertest.FakeNetwork {
	return dockertest.FakeNetwork{
		ID: "self-net-id", Name: ingress.SelfNetworkName, Driver: "overlay", Scope: "swarm", Labels: labels,
		Subnets: []string{subnet}, Gateways: []string{gateway},
	}
}

// selfCreates counts the creates of a network of the reserved name.
func (r *rig) selfCreates() int {
	n := 0
	for _, c := range r.fake.NetworkCreates() {
		if c.Name == ingress.SelfNetworkName {
			n++
		}
	}
	return n
}

// TestSelfObservationAfterAPass: one pass hands the registered function one observation with the self network's subnet and
// gateway from the engine's inspect, the ingress network's subnet, the node's bridge gateways and TierPlan's attachment, and
// SelfObservationOf answers the same (D-06, A-08).
func TestSelfObservationAfterAPass(t *testing.T) {
	r, facts := newSelfRig(t, selfRigOptions{config: func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks,
			dockertest.FakeNetwork{ID: "bridge-id", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}, Gateways: []string{"172.17.0.1"}},
			dockertest.FakeNetwork{ID: "gwbridge-id", Name: "docker_gwbridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.19.0.0/16"}, Gateways: []string{"172.19.0.1"}},
			premadeSelf(ingress.SelfNetworkLabels(), "10.9.0.0/24", "10.9.0.1"),
		)
	}})
	facts.setAttached(true)
	if _, ok := r.svc.SelfObservationOf(r.envID); ok {
		t.Fatal("an observation before any pass")
	}
	var mu sync.Mutex
	var got []ingressdeploy.SelfObservation
	r.svc.OnSelfObserved(func(o ingressdeploy.SelfObservation) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, o)
	})
	r.claimSelf()
	r.round()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("observations after one pass = %d, want 1", len(got))
	}
	o := got[0]
	ing, ok := r.network()
	if !ok || len(ing.Subnets) != 1 {
		t.Fatalf("the ingress network = %+v, %v; want it with one subnet", ing, ok)
	}
	want := ingressdeploy.SelfObservation{
		EnvID: r.envID, Exists: true, Wanted: true, NetworkID: "self-net-id",
		Subnet: netip.MustParsePrefix("10.9.0.0/24"), Gateway: netip.MustParseAddr("10.9.0.1"),
		IngressSubnets: []netip.Prefix{netip.MustParsePrefix(ing.Subnets[0])},
		BridgeGateways: []netip.Addr{netip.MustParseAddr("172.17.0.1"), netip.MustParseAddr("172.19.0.1")},
		Attached:       true, Kind: ingressdeploy.SelfKindSwarmService, Namespace: "tierplan", BackendHost: tpServiceName, Observed: true,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("observation =\n%+v\nwant\n%+v", o, want)
	}
	if mem, ok := r.svc.SelfObservationOf(r.envID); !ok || !reflect.DeepEqual(mem, o) {
		t.Errorf("SelfObservationOf = %+v, %v; want the observation handed to the callback", mem, ok)
	}
	if p := ingress.SelfTrustedPrefixes(o.Facts()); len(p) == 0 {
		t.Error("the observation's facts build no trusted set")
	}
}

// TestEnsureAdoptsOrRefusesAnExistingTierPlanSelf: a tierplan-self that carries the ingress label and the self role is
// TierPlan's whatever its subnet and the proxy is attached to it; one without the role is foreign: nothing is attached or
// made, the observation says so and the pass logs self_network_foreign (T-28-37).
func TestEnsureAdoptsOrRefusesAnExistingTierPlanSelf(t *testing.T) {
	t.Run("adopted", func(t *testing.T) {
		r, _ := newSelfRig(t, selfRigOptions{config: func(c *dockertest.FakeConfig) {
			c.Networks = append(c.Networks, premadeSelf(ingress.SelfNetworkLabels(), "10.200.7.0/24", "10.200.7.1"))
		}})
		r.claimSelf()
		r.round()
		if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName, ingress.SelfNetworkName}) {
			t.Errorf("proxy networks = %v, want the adopted self network attached", got)
		}
		if n := r.selfCreates(); n != 0 {
			t.Errorf("self network creates = %d, want 0: the existing one is adopted", n)
		}
		o, ok := r.svc.SelfObservationOf(r.envID)
		if !ok || o.Foreign || !o.Exists || o.Subnet != netip.MustParsePrefix("10.200.7.0/24") {
			t.Errorf("observation = %+v, %v; want TierPlan's network on 10.200.7.0/24, not foreign", o, ok)
		}
	})
	t.Run("foreign", func(t *testing.T) {
		logs := &logBuffer{}
		r, _ := newSelfRig(t, selfRigOptions{
			config: func(c *dockertest.FakeConfig) {
				c.Networks = append(c.Networks, premadeSelf(ingress.ObjectLabels(), "10.200.8.0/24", "10.200.8.1"))
			},
			deps: func(d *ingressdeploy.Deps) { d.Log = slog.New(slog.NewTextHandler(logs, nil)) },
		})
		r.claimSelf()
		r.round()
		if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName}) {
			t.Errorf("proxy networks = %v, want only %s: a foreign network is never attached", got, ingress.NetworkName)
		}
		if n := r.selfCreates(); n != 0 {
			t.Errorf("self network creates = %d, want 0 beside a foreign one", n)
		}
		o, ok := r.svc.SelfObservationOf(r.envID)
		if !ok || !o.Foreign || o.Exists || o.Subnet.IsValid() {
			t.Errorf("observation = %+v, %v; want foreign, not existing, no subnet", o, ok)
		}
		if p := ingress.SelfTrustedPrefixes(o.Facts()); len(p) != 0 {
			t.Errorf("a foreign network built the trusted set %v", p)
		}
		if !strings.Contains(logs.String(), ingressdeploy.CodeSelfNetworkForeign) {
			t.Errorf("the log does not carry %s:\n%s", ingressdeploy.CodeSelfNetworkForeign, logs.String())
		}
		if row, ok := r.selfNetwork(); !ok || !maps.Equal(row.Labels, ingress.ObjectLabels()) {
			t.Errorf("the foreign network = %+v, %v; want it left as it was", row, ok)
		}
	})
}

// TestEnsureLeavesTierPlanSelfWhileTierPlanIsOnIt: once the self network is no longer wanted the proxy drops it first; the
// network stays, KeptInUse, while TierPlan's own container is on it; an engine that answers it is still in use keeps it too,
// and the pass is not a failure; then a pass removes it (A-06, Pitfall 5).
func TestEnsureLeavesTierPlanSelfWhileTierPlanIsOnIt(t *testing.T) {
	var base *dockertest.FakeConfig
	r, facts := newSelfRig(t, selfRigOptions{config: func(c *dockertest.FakeConfig) {
		c.RemoveErrors = map[string]string{ingress.SelfNetworkName: "error while removing network: network has active endpoints"}
		base = c
	}})
	observed := func() ingressdeploy.SelfObservation {
		t.Helper()
		o, ok := r.svc.SelfObservationOf(r.envID)
		if !ok {
			t.Fatal("no observation after a pass")
		}
		return o
	}
	wantOK := func(when string) {
		t.Helper()
		if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
			t.Errorf("%s: pass class = %q, want ok", when, class)
		}
	}

	r.claimSelf()
	r.round()
	if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName, ingress.SelfNetworkName}) {
		t.Fatalf("proxy networks with the claim held = %v, want both", got)
	}
	facts.setAttached(true)
	r.unclaimSelf()

	for i := range 2 {
		r.round()
		if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName}) {
			t.Errorf("pass %d after the claim went: proxy networks = %v, want the self network dropped", i+1, got)
		}
		if _, ok := r.selfNetwork(); !ok {
			t.Fatalf("pass %d removed the self network while TierPlan is on it", i+1)
		}
		if o := observed(); !o.KeptInUse || !o.Exists || !o.Attached || o.Wanted {
			t.Errorf("pass %d: observation = %+v, want kept in use, existing, TierPlan attached, not wanted", i+1, o)
		}
		wantOK("TierPlan on the network")
	}

	facts.setAttached(false)
	r.round()
	if _, ok := r.selfNetwork(); !ok {
		t.Fatal("the network was removed although the engine said it is in use")
	}
	if o := observed(); !o.KeptInUse || !o.Exists || o.Attached {
		t.Errorf("after an in-use refusal: observation = %+v, want kept in use, existing, TierPlan not attached", o)
	}
	wantOK("an in-use refusal")

	cfg := *base
	cfg.RemoveErrors = nil
	r.fake.Reconfigure(cfg)
	r.round()
	if _, ok := r.selfNetwork(); ok {
		t.Fatal("the self network is still there with nothing on it")
	}
	if o := observed(); o.KeptInUse || o.Exists || o.Subnet.IsValid() {
		t.Errorf("after the removal: observation = %+v, want not kept, not existing, no subnet", o)
	}
	wantOK("the removal")
	if got := r.proxyNetworks(); !slices.Equal(got, []string{ingress.NetworkName}) {
		t.Errorf("proxy networks after the removal = %v, want only %s", got, ingress.NetworkName)
	}
}

// TestSelfNetworkForeignMembersAreReported (CR-02): a service on tierplan-self that is neither the proxy nor TierPlan's own
// is named in the observation's ForeignMembers and logged with self_network_foreign_member; the proxy and TierPlan's own
// service never are. The stack namespace of TierPlan's own service is what SelfStackNamespace answers.
func TestSelfNetworkForeignMembersAreReported(t *testing.T) {
	withMembers := func(extra ...dockertest.FakeService) func(*dockertest.FakeConfig) {
		return func(c *dockertest.FakeConfig) {
			c.Networks = append(c.Networks, premadeSelf(ingress.SelfNetworkLabels(), "10.9.0.0/24", "10.9.0.1"))
			for i := range c.ServiceRows {
				if c.ServiceRows[i].ID == tpServiceID {
					c.ServiceRows[i].Networks = append(c.ServiceRows[i].Networks, "self-net-id")
				}
			}
			c.ServiceRows = append(c.ServiceRows, extra...)
		}
	}
	t.Run("only the proxy and TierPlan", func(t *testing.T) {
		r, facts := newSelfRig(t, selfRigOptions{config: withMembers()})
		facts.setAttached(true)
		r.claimSelf()
		r.round()
		r.round()
		o, ok := r.svc.SelfObservationOf(r.envID)
		if !ok || !o.Exists || len(o.ForeignMembers) != 0 {
			t.Fatalf("observation = %+v, %v; want TierPlan's network with no foreign member", o, ok)
		}
		if got := r.svc.SelfStackNamespace(r.envID); got != "tierplan" {
			t.Errorf("SelfStackNamespace = %q, want tierplan", got)
		}
		if got := r.svc.SelfStackNamespace("env-elsewhere"); got != "" {
			t.Errorf("SelfStackNamespace of an unobserved environment = %q, want none", got)
		}
	})
	t.Run("a stack's service joined it", func(t *testing.T) {
		logs := &logBuffer{}
		r, facts := newSelfRig(t, selfRigOptions{
			config: withMembers(
				dockertest.FakeService{ID: "shop-web-id", Name: "shop_web", Running: 1, Desired: 1, Networks: []string{"self-net-id"}},
				dockertest.FakeService{ID: "shop-db-id", Name: "shop_db", Running: 1, Desired: 1, Networks: []string{ingress.SelfNetworkName}},
				dockertest.FakeService{ID: "other-id", Name: "other_app", Running: 1, Desired: 1, Networks: []string{"tierplan_default"}},
			),
			deps: func(d *ingressdeploy.Deps) { d.Log = slog.New(slog.NewTextHandler(logs, nil)) },
		})
		facts.setAttached(true)
		r.claimSelf()
		r.round()
		o, ok := r.svc.SelfObservationOf(r.envID)
		if !ok || !slices.Equal(o.ForeignMembers, []string{"shop_db", "shop_web"}) {
			t.Fatalf("observation = %+v, %v; want the foreign members [shop_db shop_web]", o, ok)
		}
		if !strings.Contains(logs.String(), ingressdeploy.CodeSelfNetworkForeignMember) {
			t.Errorf("the log does not carry %s:\n%s", ingressdeploy.CodeSelfNetworkForeignMember, logs.String())
		}
	})
}

// TestRemovedSelfNetworkIsNotTrustedAfterAFailedRead (WR-01): a pass that removes TierPlan's own network and then cannot read
// the swarm publishes that the network is gone, so its subnet, which the swarm can hand to a user network, is not trusted.
func TestRemovedSelfNetworkIsNotTrustedAfterAFailedRead(t *testing.T) {
	r, _ := newSelfRig(t, selfRigOptions{})
	r.claimSelf()
	r.round()
	if o, ok := r.svc.SelfObservationOf(r.envID); !ok || !o.Exists {
		t.Fatalf("observation with the claim held = %+v, %v; want the network", o, ok)
	}
	r.unclaimSelf()
	r.round() // the proxy drops the network
	if _, ok := r.selfNetwork(); !ok {
		t.Fatal("the self network went before the proxy dropped it")
	}
	r.eng.failReadAfterWrite.Store(true)
	r.round() // the network goes, and the read after it fails
	if _, ok := r.selfNetwork(); ok {
		t.Fatal("the self network is still there")
	}
	o, ok := r.svc.SelfObservationOf(r.envID)
	if !ok || o.Exists || o.Subnet.IsValid() || o.NetworkID != "" {
		t.Fatalf("observation after the removal = %+v, %v; want the network gone", o, ok)
	}
	if p := ingress.SelfTrustedPrefixes(o.Facts()); len(p) != 0 {
		t.Errorf("the observation still trusts %v", p)
	}
}

// TestSelfReadFailurePublishesNothing (WR-03): a pass that cannot read TierPlan's own container does not know whether TierPlan
// is on the network, so it publishes nothing and the last observation stands: a busy local socket must not drop the route
// and the trusted set for a pass.
func TestSelfReadFailurePublishesNothing(t *testing.T) {
	r, facts := newSelfRig(t, selfRigOptions{})
	facts.setAttached(true)
	var mu sync.Mutex
	published := 0
	r.svc.OnSelfObserved(func(ingressdeploy.SelfObservation) {
		mu.Lock()
		defer mu.Unlock()
		published++
	})
	r.claimSelf()
	r.round()
	before, ok := r.svc.SelfObservationOf(r.envID)
	if !ok || !before.Attached || before.Kind != ingressdeploy.SelfKindSwarmService {
		t.Fatalf("observation = %+v, %v; want TierPlan attached as a swarm service", before, ok)
	}
	facts.mu.Lock()
	facts.err = errors.New("the local socket did not answer in time")
	facts.mu.Unlock()
	r.round()
	after, _ := r.svc.SelfObservationOf(r.envID)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("observation after a failed self read =\n%+v\nwant the last one kept\n%+v", after, before)
	}
	mu.Lock()
	defer mu.Unlock()
	if published != 1 {
		t.Errorf("observations published = %d, want 1: the pass that could not read TierPlan's container publishes none", published)
	}
}
