// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"slices"
	"strings"
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

func TestStandaloneReviewWarnings(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: x.example.com
      tierplan.ingress.port: "80"
  idle:
    image: nginx:1.27
    scale: 0
    healthcheck:
      test: ["CMD", "true"]
    labels:
      tierplan.ingress.host: idle.example.com
      tierplan.ingress.port: "80"
  rr:
    image: nginx:1.27
    healthcheck:
      test: ["CMD", "true"]
    deploy:
      endpoint_mode: dnsrr
    labels:
      tierplan.ingress.host: rr.example.com
      tierplan.ingress.port: "80"
`
	warnings := func(t *testing.T, off, stopped bool) map[string]bool {
		t.Helper()
		r := newStandaloneRig(t)
		r.deps.Ingress = &fakeIngress{}
		in := r.standaloneReviewInput(t, "demo", compose, nil)
		in.Ingress, in.IngressOff, in.Stack.Stopped = ingressOn(), off, stopped
		rv, err := r.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		got := map[string]bool{}
		for _, p := range rv.Problems {
			if !strings.HasPrefix(p.Code, "ingress_") {
				continue
			}
			if p.Severity != stacks.SeverityWarning {
				t.Errorf("problem %+v, want a warning and never an error", p)
			}
			got[p.Code+" "+strings.TrimPrefix(p.Key, "services.")] = true
		}
		return got
	}
	eq := func(t *testing.T, got map[string]bool, want ...string) {
		t.Helper()
		w := map[string]bool{}
		for _, k := range want {
			w[k] = true
		}
		if len(got) != len(w) {
			t.Errorf("warnings = %v, want %v", got, want)
			return
		}
		for k := range w {
			if !got[k] {
				t.Errorf("warnings = %v, want %v", got, want)
			}
		}
	}

	// Scale 0 and a missing health check warn, and endpoint_mode: dnsrr gives nothing on one host.
	eq(t, warnings(t, false, false), "ingress_no_healthcheck web", "ingress_no_replicas idle")
	// Ingress off warns once.
	eq(t, warnings(t, true, false), "ingress_disabled idle", "ingress_no_healthcheck web", "ingress_no_replicas idle")
	// A stopped stack runs nothing, so each routed service has no replicas.
	eq(t, warnings(t, false, true), "ingress_no_healthcheck web", "ingress_no_replicas web", "ingress_no_replicas idle", "ingress_no_replicas rr")
}
