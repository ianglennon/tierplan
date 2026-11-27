// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"maps"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// selfCall is one call TierPlan made through its local socket to put its own container on the self network or take it off.
type selfCall struct {
	network, alias, container string
}

// standaloneSelf is TierPlan's own container on a standalone host as the local engine shows it, and the local socket TierPlan
// connects itself through: a connect puts the container on tierplan-self and a disconnect takes it off.
type standaloneSelf struct {
	mu          sync.Mutex
	facts       ingressdeploy.SelfFacts
	connects    []selfCall
	disconnects []selfCall
}

func newStandaloneSelf(containerID string) *standaloneSelf {
	return &standaloneSelf{facts: ingressdeploy.SelfFacts{ContainerID: containerID, Networks: []string{"bridge"}}}
}

func (f *standaloneSelf) read(context.Context) (ingressdeploy.SelfFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.facts
	out.Networks = slices.Clone(f.facts.Networks)
	return out, nil
}

func (f *standaloneSelf) connect(_ context.Context, networkID, alias string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, selfCall{network: networkID, alias: alias, container: f.facts.ContainerID})
	if !slices.Contains(f.facts.Networks, ingress.SelfNetworkName) {
		f.facts.Networks = append(f.facts.Networks, ingress.SelfNetworkName)
	}
	return nil
}

func (f *standaloneSelf) disconnect(_ context.Context, networkID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, selfCall{network: networkID, container: f.facts.ContainerID})
	f.facts.Networks = slices.DeleteFunc(f.facts.Networks, func(n string) bool { return n == ingress.SelfNetworkName })
	return nil
}

// recreate replaces TierPlan's container with a new one that is on no network of TierPlan's, as an upgrade does.
func (f *standaloneSelf) recreate(containerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = ingressdeploy.SelfFacts{ContainerID: containerID, Networks: []string{"bridge"}}
}

func (f *standaloneSelf) calls() (connects, disconnects []selfCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.connects), slices.Clone(f.disconnects)
}

// standaloneSelfOptions tune a standalone self rig.
type standaloneSelfOptions struct {
	// noSocket leaves SelfConnect and SelfDisconnect nil: TierPlan does not reach its local engine.
	noSocket bool
	// deps adjusts the deployer's dependencies further.
	deps func(*ingressdeploy.Deps)
}

// newStandaloneSelfRig is an enabled standalone rig on TierPlan's own host (the local environment), on which TierPlan runs
// as the container c1.
func newStandaloneSelfRig(t *testing.T, o standaloneSelfOptions) (*standaloneRig, *standaloneSelf) {
	t.Helper()
	self := newStandaloneSelf("c1")
	r := newStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Self = self.read
		if !o.noSocket {
			d.SelfConnect, d.SelfDisconnect = self.connect, self.disconnect
		}
		if o.deps != nil {
			o.deps(d)
		}
	}})
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Local = true })
	wantStandaloneDone(t, r.enable(ingressdeploy.EnableRequest{}))
	if got := r.sys.deployCount(); got != 1 {
		t.Fatalf("deploys after the enable = %d, want 1", got)
	}
	return r, self
}

// lastProjectNetworks are the Docker names of the networks the proxy service of the last deploy joins, sorted.
func (r *standaloneRig) lastProjectNetworks() []string {
	r.t.Helper()
	r.sys.mu.Lock()
	defer r.sys.mu.Unlock()
	if len(r.sys.deploys) == 0 {
		r.t.Fatal("no deploy was made")
	}
	p := r.sys.deploys[len(r.sys.deploys)-1].Project
	var out []string
	for key := range p.Services[ingress.ProxyComposeService].Networks {
		out = append(out, p.Networks[key].Name)
	}
	sort.Strings(out)
	return out
}

// observation is the last observation of the rig's self network, which must exist.
func (r *standaloneRig) observation() ingressdeploy.SelfObservation {
	r.t.Helper()
	o, ok := r.svc.SelfObservationOf(r.envID)
	if !ok {
		r.t.Fatal("no observation of the self network was published")
	}
	return o
}

// TestStandaloneEnsureConnectsTierPlan is the tracer (D-04): with TierPlan's own route held on its own standalone host, one
// pass makes the bridge tierplan-self with the self labels, connects TierPlan's container to it once under the alias
// tierplan, and brings the proxy up on both networks; the observation says TierPlan is attached and reached as tierplan. A
// second pass is quiet.
func TestStandaloneEnsureConnectsTierPlan(t *testing.T) {
	r, self := newStandaloneSelfRig(t, standaloneSelfOptions{})
	r.claimSelf()

	r.round()
	n, ok := r.selfNetwork()
	if !ok {
		t.Fatal("no tierplan-self network after the pass")
	}
	if n.Driver != "bridge" || !maps.Equal(n.Labels, ingress.SelfNetworkLabels()) {
		t.Errorf("self network = driver %q labels %v, want a bridge with %v", n.Driver, n.Labels, ingress.SelfNetworkLabels())
	}
	connects, _ := self.calls()
	if len(connects) != 1 || connects[0] != (selfCall{network: n.ID, alias: ingressdeploy.SelfBackendAlias, container: "c1"}) {
		t.Fatalf("connects = %+v, want one of c1 to %s as %s", connects, n.ID, ingressdeploy.SelfBackendAlias)
	}
	if got := r.sys.deployCount(); got != 2 {
		t.Fatalf("deploys = %d, want 2: the proxy is brought up on the self network", got)
	}
	if got, want := r.lastProjectNetworks(), []string{ingress.NetworkName, ingress.SelfNetworkName}; !slices.Equal(got, want) {
		t.Errorf("proxy networks = %v, want %v", got, want)
	}
	o := r.observation()
	if !o.Exists || !o.Wanted || !o.Attached || o.Kind != ingressdeploy.SelfKindStandalone || o.BackendHost != ingressdeploy.SelfBackendAlias ||
		o.NetworkID != n.ID || !o.Subnet.IsValid() {
		t.Errorf("observation = %+v, want TierPlan's standalone container attached to %s as %s", o, n.ID, ingressdeploy.SelfBackendAlias)
	}

	r.round()
	if connects, _ := self.calls(); len(connects) != 1 {
		t.Errorf("a pass over the converged host connected again: %+v", connects)
	}
	if got := r.sys.deployCount(); got != 2 {
		t.Errorf("a pass over the converged host redeployed (%d deploys)", got)
	}
}

// TestStandaloneEnsureReconnectsAfterARecreate (D-04): a recreated TierPlan container (an upgrade: a new ID, on no network of
// TierPlan's) is connected again within one pass, once, and the proxy is left alone.
func TestStandaloneEnsureReconnectsAfterARecreate(t *testing.T) {
	r, self := newStandaloneSelfRig(t, standaloneSelfOptions{})
	r.claimSelf()
	r.round()
	n, ok := r.selfNetwork()
	if !ok {
		t.Fatal("no tierplan-self network after the first pass")
	}
	deploys := r.sys.deployCount()

	self.recreate("c2")
	r.round()
	connects, _ := self.calls()
	if len(connects) != 2 || connects[1] != (selfCall{network: n.ID, alias: ingressdeploy.SelfBackendAlias, container: "c2"}) {
		t.Fatalf("connects = %+v, want a second one, of c2 to %s as %s", connects, n.ID, ingressdeploy.SelfBackendAlias)
	}
	if got := r.sys.deployCount(); got != deploys {
		t.Errorf("deploys = %d, want %d: the proxy is on the network already", got, deploys)
	}
	if o := r.observation(); !o.Attached {
		t.Errorf("observation = %+v, want TierPlan attached again", o)
	}

	r.round()
	if connects, _ := self.calls(); len(connects) != 2 {
		t.Errorf("a pass after the reconnect connected again: %+v", connects)
	}
}
