// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The rendered-bundle helpers of the real-Traefik tests (21-09, 21-11): the tests render what the
// reconciler would render, apply it to the harness root the way a node supervisor must, and look at
// what the pinned proxy does with it. Nothing here is hand-written YAML.

// applyBundle writes a rendered bundle into the harness root. Every file goes in with the mode its
// manifest entry states and is checked against the step the entry states (certificates are step 1 and
// the dynamic file step 2), so the harness applies the bundle exactly as the contract says: the
// harness's Apply writes every certificate first and the YAML last, by rename.
func applyBundle(t testing.TB, env *traefiktest.Env, b ingressbundle.Bundle) {
	t.Helper()
	if err := b.Verify(); err != nil {
		t.Fatalf("applyBundle: the bundle does not verify: %v", err)
	}
	specs := make([]traefiktest.FileSpec, 0, len(b.Files))
	for _, f := range b.Files {
		i := slices.IndexFunc(b.Manifest.Files, func(e ingressbundle.FileEntry) bool { return e.Path == f.Path })
		if i < 0 {
			t.Fatalf("applyBundle: %s has no manifest entry", f.Path)
		}
		e := b.Manifest.Files[i]
		wantStep := ingressbundle.StepDynamic
		if strings.HasPrefix(path.Clean(f.Path), "certs/") {
			wantStep = ingressbundle.StepCerts
		}
		if e.Step != wantStep {
			t.Fatalf("applyBundle: %s is step %d in the manifest, want %d for its directory", f.Path, e.Step, wantStep)
		}
		specs = append(specs, traefiktest.FileSpec{Path: f.Path, Data: f.Data, Mode: os.FileMode(e.Mode)})
	}
	// Ordered by step, as a supervisor reads the manifest; Apply orders by directory regardless.
	slices.SortStableFunc(specs, func(a, b traefiktest.FileSpec) int {
		return stepOfPath(a.Path) - stepOfPath(b.Path)
	})
	env.Apply(t, specs)
}

func stepOfPath(p string) int {
	if strings.HasPrefix(p, "certs/") {
		return ingressbundle.StepCerts
	}
	return ingressbundle.StepDynamic
}

// swarmRouteWith is a route of a swarm stack's service, owned by stack, with the scheme and the strip
// flag stated. (render_test.go already declares swarmRoute for the http, no-strip case, and a second
// declaration of that name would not build under the integration tag.)
func swarmRouteWith(host, prefix, stack, service string, port int, scheme ingress.Scheme, strip bool) ingress.SnapshotRoute {
	r := swarmRoute(host, prefix, stack, service, port)
	r.Scheme = scheme
	r.Strip = strip
	return r
}

// snapshotOf is a snapshot of an enabled environment holding routes, with one claim per distinct
// hostname, owned by the stack of the hostname's first route. Routes keep the order given; Render
// sorts what it needs.
func snapshotOf(routes ...ingress.SnapshotRoute) ingress.Snapshot {
	s := ingress.Snapshot{
		Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
		Routes:   routes,
	}
	seen := map[string]bool{}
	for _, r := range routes {
		if seen[r.Hostname] {
			continue
		}
		seen[r.Hostname] = true
		s.Claims = append(s.Claims, ingress.SnapshotClaim{
			Claim:     ingress.Claim{Hostname: r.Hostname, OwnerKind: ingress.OwnerStack},
			StackName: r.StackName,
			Target:    ingress.TargetSwarm,
		})
	}
	return s
}

// servable is a certificate per host, each a leaf the test CA issued for that host alone, ready to
// serve (no Reason).
func servable(t testing.TB, ca *traefiktest.CA, hosts ...string) []HostCert {
	t.Helper()
	out := make([]HostCert, 0, len(hosts))
	for _, h := range hosts {
		chain, key := ca.Leaf(t, h)
		out = append(out, HostCert{Hostname: h, ChainPEM: chain, KeyPEM: key})
	}
	return out
}
