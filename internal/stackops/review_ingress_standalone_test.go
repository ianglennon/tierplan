// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// What the review of a standalone stack says of ingress (D-17): the routes and the certificates as a swarm's review
// does, and the services whose container is recreated because it joins or leaves the shared bridge.

// ingressOn is the attachment the review route puts on a standalone review when ingress is on for the environment.
func ingressOn() *stacks.IngressAttach { return &stacks.IngressAttach{Network: ingress.NetworkName} }

// webContainer is the one container of web the host holds for the stack demo, on the networks given.
func webContainer(networks ...string) dockertest.FakeContainer {
	c := deployedContainer("demo", "web", "hash", "nginx:1.27", "nginx:1.27")
	c.Networks = map[string]dockertest.FakeEndpoint{}
	for _, n := range networks {
		c.Networks[n] = dockertest.FakeEndpoint{NetworkID: "net-" + n}
	}
	return c
}

// reviewStandaloneIngress reviews the routed compose file of the stack demo on a host that holds the containers.
func reviewStandaloneIngress(t *testing.T, attach *stacks.IngressAttach, mutate func(*stackops.ReviewInput), containers ...dockertest.FakeContainer) stackops.Review {
	t.Helper()
	r := newStandaloneRig(t, containers...)
	r.deps.Ingress = &fakeIngress{states: map[string]stackops.CertState{"x.example.com": {State: stackops.CertWillIssue}}}
	in := r.standaloneReviewInput(t, "demo", standaloneRoutedX, nil)
	in.Ingress = attach
	if mutate != nil {
		mutate(&in)
	}
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

func networkOf(rv stackops.Review) []stackops.IngressAttachRow {
	if rv.Ingress == nil {
		return nil
	}
	return rv.Ingress.Network
}

func TestStandaloneReviewJoinsWithRecreate(t *testing.T) {
	t.Parallel()
	t.Run("a running service that is not on the shared network joins and is recreated", func(t *testing.T) {
		t.Parallel()
		rv := reviewStandaloneIngress(t, ingressOn(), nil, webContainer("demo_default"))
		if rv.Ingress == nil || len(rv.Ingress.Routes) != 1 || rv.Ingress.Routes[0].Note != "" {
			t.Fatalf("ingress = %+v, want the route with no note", rv.Ingress)
		}
		if len(rv.Ingress.Certificates) != 1 {
			t.Errorf("certificates = %+v, want the hostname", rv.Ingress.Certificates)
		}
		want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachJoins, ReplacesTasks: true}}
		if got := networkOf(rv); !slices.Equal(got, want) {
			t.Errorf("network = %+v, want %+v", got, want)
		}
	})
	t.Run("a service with no container yet joins with nothing to recreate", func(t *testing.T) {
		t.Parallel()
		rv := reviewStandaloneIngress(t, ingressOn(), nil)
		want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachJoins, ReplacesTasks: false}}
		if got := networkOf(rv); !slices.Equal(got, want) {
			t.Errorf("network = %+v, want %+v", got, want)
		}
	})
}

func TestStandaloneReviewNoRowWhenAttached(t *testing.T) {
	t.Parallel()
	rv := reviewStandaloneIngress(t, ingressOn(), nil, webContainer("demo_default", ingress.NetworkName))
	if rv.Ingress == nil || len(rv.Ingress.Routes) == 0 {
		t.Fatalf("ingress = %+v, want the routes still shown", rv.Ingress)
	}
	if got := networkOf(rv); len(got) != 0 {
		t.Errorf("network = %+v, want no row for a stack already attached", got)
	}
}

func TestStandaloneReviewLeaves(t *testing.T) {
	t.Parallel()
	// Ingress is off now (no attachment is asked for), so the file does not list the network and the container on it leaves.
	rv := reviewStandaloneIngress(t, nil, func(in *stackops.ReviewInput) { in.IngressOff = true }, webContainer("demo_default", ingress.NetworkName))
	want := []stackops.IngressAttachRow{{Service: "web", Change: stackops.IngressAttachLeaves, ReplacesTasks: true}}
	if got := networkOf(rv); !slices.Equal(got, want) {
		t.Errorf("network = %+v, want %+v", got, want)
	}
}
