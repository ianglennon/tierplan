// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// attachCompose is a stack with one routed service and one that is not.
const attachCompose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: free.apps.example.com
        tierplan.ingress.port: "80"
  db:
    image: postgres:16
`

// withIngressNetwork puts the shared ingress network on the fake swarm, as enabling ingress leaves it.
func withIngressNetwork(c *dockertest.FakeConfig) {
	c.Networks = append(c.Networks, dockertest.FakeNetwork{
		ID: "net-tierplan-ingress", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm",
		Labels: map[string]string{ingress.LabelComponent: "network"},
	})
}

// networkTargets are the networks a created service is attached to, by the target the deploy sent.
func networkTargets(spec swarm.ServiceSpec) []string {
	var out []string
	for _, n := range spec.TaskTemplate.Networks {
		out = append(out, n.Target)
	}
	return out
}

// attachesIngress reports whether the spec joins the shared ingress network, by name or by its ID, and whether it
// does so with an alias.
func attachesIngress(spec swarm.ServiceSpec) (attached, aliased bool) {
	for _, n := range spec.TaskTemplate.Networks {
		if n.Target == ingress.NetworkName || n.Target == "net-tierplan-ingress" {
			return true, len(n.Aliases) > 0
		}
	}
	return false, false
}

// deployAttachFixture seeds the ingress world, sets the ingress switch and deploys attachCompose, and returns the
// services the deploy created, by name.
func deployAttachFixture(t *testing.T, enabled bool) map[string]swarm.ServiceSpec {
	t.Helper()
	f, e := stackEnv(t, withIngressNetwork)
	seedIngressWorld(t, f, e)
	if _, err := f.stackDeps.Ingress.SetEnabled(testsupport.Context(t), e.ID, enabled, "admin", nil); err != nil {
		t.Fatalf("set the ingress switch: %v", err)
	}
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "shop")
	f.deployGood(t, admin, st.ID, attachCompose)
	created := map[string]swarm.ServiceSpec{}
	for _, c := range f.fake.ServiceCreates() {
		created[c.Spec.Name] = c.Spec
	}
	return created
}

// TestDeployAttachesWhenIngressEnabled is D-18 and D-19: with ingress on, the routed service joins the shared network
// with no alias there, and a service that declares no route does not.
func TestDeployAttachesWhenIngressEnabled(t *testing.T) {
	t.Parallel()
	created := deployAttachFixture(t, true)
	web, ok := created["shop_web"]
	if !ok {
		t.Fatalf("services created = %v, want shop_web", created)
	}
	attached, aliased := attachesIngress(web)
	if !attached {
		t.Fatalf("shop_web networks = %v, want %s among them", networkTargets(web), ingress.NetworkName)
	}
	if aliased {
		t.Errorf("shop_web has an alias on the ingress network: %+v", web.TaskTemplate.Networks)
	}
	if db, ok := created["shop_db"]; !ok {
		t.Fatalf("services created = %v, want shop_db", created)
	} else if attached, _ := attachesIngress(db); attached {
		t.Errorf("shop_db networks = %v, a service with no route is not attached", networkTargets(db))
	}
}

// TestDeployDoesNotAttachWhenIngressOff is D-13: with ingress off nothing names the shared network.
func TestDeployDoesNotAttachWhenIngressOff(t *testing.T) {
	t.Parallel()
	created := deployAttachFixture(t, false)
	if len(created) == 0 {
		t.Fatal("the deploy created no service")
	}
	for name, spec := range created {
		if slices.ContainsFunc(spec.TaskTemplate.Networks, func(n swarm.NetworkAttachmentConfig) bool {
			return n.Target == ingress.NetworkName || n.Target == "net-tierplan-ingress"
		}) {
			t.Errorf("%s networks = %v, want none of them the ingress network with ingress off", name, networkTargets(spec))
		}
	}
}
