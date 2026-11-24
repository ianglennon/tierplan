// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// standaloneRoute is a route of a standalone-host stack's service.
func standaloneRoute(host, prefix, stack, svc string, port int) ingress.SnapshotRoute {
	r := swarmRoute(host, prefix, stack, svc, port)
	r.Target = ingress.TargetStandalone
	return r
}

// standaloneClaim is the claim that goes with standaloneRoute.
func standaloneClaim(host string) ingress.SnapshotClaim {
	c := claimFor(host)
	c.Target = ingress.TargetStandalone
	return c
}

// certFileNames lists the certs/ files of a bundle.
func certFileNames(b ingressbundle.Bundle) []string {
	var out []string
	for _, f := range b.Files {
		if f.Path != ingressbundle.DynamicFile {
			out = append(out, f.Path)
		}
	}
	return out
}

// TestSkippedReasons is the tracer of the selection (D-09, D-12): four routes on four hostnames, one
// per case. The two claimed, certified ones (a swarm stack's and a standalone stack's, D-15) are
// rendered; each other is returned with its stable reason, in hostname order.
func TestSkippedReasons(t *testing.T) {
	in := Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 2},
			Claims: []ingress.SnapshotClaim{
				claimFor("a.example.com"), claimFor("b.example.com"), claimFor("c.example.com"),
				standaloneClaim("d.example.com"),
			},
			Routes: []ingress.SnapshotRoute{
				standaloneRoute("d.example.com", "/", "legacy", "app", 80),
				swarmRoute("b.example.com", "/", "shop", "web", 8080),
				swarmRoute("a.example.com", "/", "shop", "web", 8080),
				swarmRoute("c.example.com", "/", "shop", "web", 8080),
			},
		},
		Certs: []HostCert{
			{Hostname: "a.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "c.example.com", Reason: SkipCertificateNotReady},
			{Hostname: "d.example.com", ChainPEM: chainA, KeyPEM: keyA},
		},
		CertsVersion: 3,
	}
	res, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var rendered []string
	for _, r := range res.Bundle.Manifest.Routes {
		rendered = append(rendered, r.Hostname)
	}
	slices.Sort(rendered)
	if !slices.Equal(rendered, []string{"a.example.com", "d.example.com"}) {
		t.Errorf("manifest routes = %+v, want the a.example.com and d.example.com routes", res.Bundle.Manifest.Routes)
	}
	_, doc := dynamicOf(t, res.Bundle)
	routers, ok := dig(t, doc, "http", "routers").(map[string]any)
	if !ok || len(routers) != 2 {
		t.Errorf("dynamic file has %d routers, want 2: %v", len(routers), routers)
	}
	// Both hosts carry the same fixture certificate, and the bundle stores certificate bytes once.
	if got := len(certFileNames(res.Bundle)); got != 2 {
		t.Errorf("bundle has %d certificate files, want one pair: %v", got, certFileNames(res.Bundle))
	}

	want := []Skipped{
		{Hostname: "b.example.com", PathPrefix: "/", Stack: "shop", Service: "web", Reason: SkipNoCertificate},
		{Hostname: "c.example.com", PathPrefix: "/", Stack: "shop", Service: "web", Reason: SkipCertificateNotReady},
	}
	if !slices.Equal(res.Skipped, want) {
		t.Errorf("Skipped = %+v\nwant      %+v", res.Skipped, want)
	}
}

// TestNoPartialHost is D-10: a hostname whose certificate is not servable is skipped whole, every
// route of it with the same reason, and nothing of it reaches the bundle, so a client fails at the
// handshake and never sees one path of the host served while another is not.
func TestNoPartialHost(t *testing.T) {
	const host = "blocked.example.com"
	routes := []ingress.SnapshotRoute{
		swarmRoute(host, "/", "shop", "web", 8080),
		swarmRoute(host, "/api", "shop", "api", 9000),
		swarmRoute(host, "/api/v2", "shop", "api", 9000),
	}
	cases := []struct {
		name string
		cert HostCert
		want SkipReason
	}{
		{"reason carried", HostCert{Hostname: host, ChainPEM: chainA, KeyPEM: keyA, Reason: SkipCertificateNotReady}, SkipCertificateNotReady},
		{"no key bytes", HostCert{Hostname: host, ChainPEM: chainA}, SkipCertificateNotReady},
		{"no chain bytes", HostCert{Hostname: host, KeyPEM: keyA}, SkipCertificateNotReady},
		{"no certificate row", HostCert{}, SkipNoCertificate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var certs []HostCert
			if tc.cert.Hostname != "" {
				certs = append(certs, tc.cert)
			}
			in := Input{
				Snapshot: ingress.Snapshot{
					Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
					Claims:   []ingress.SnapshotClaim{claimFor(host)},
					Routes:   routes,
				},
				Certs: certs,
			}
			res, err := Render(in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if len(res.Skipped) != len(routes) {
				t.Fatalf("Skipped has %d entries, want %d: %+v", len(res.Skipped), len(routes), res.Skipped)
			}
			for _, s := range res.Skipped {
				if s.Hostname != host || s.Reason != tc.want {
					t.Errorf("skipped %+v, want hostname %s with reason %s", s, host, tc.want)
				}
			}
			if names := certFileNames(res.Bundle); len(names) != 0 {
				t.Errorf("certificate files for a host that is not served: %v", names)
			}
			if len(res.Bundle.Manifest.Routes) != 0 {
				t.Errorf("manifest holds routes for a host that is not served: %+v", res.Bundle.Manifest.Routes)
			}
			raw, doc := dynamicOf(t, res.Bundle)
			if bytes.Contains(raw, []byte(host)) {
				t.Errorf("hostname appears in the dynamic file:\n%s", raw)
			}
			if _, has := doc["http"]; has {
				t.Errorf("dynamic file has an http section with nothing to serve:\n%s", raw)
			}
		})
	}

	t.Run("a ready host beside it is served alone", func(t *testing.T) {
		in := Input{
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
				Claims:   []ingress.SnapshotClaim{claimFor(host), claimFor("ok.example.com")},
				Routes:   append(slices.Clone(routes), swarmRoute("ok.example.com", "/", "shop", "web", 8080)),
			},
			Certs: []HostCert{
				{Hostname: host, Reason: SkipCertificateNotReady},
				{Hostname: "ok.example.com", ChainPEM: chainA, KeyPEM: keyA},
			},
		}
		res, err := Render(in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		raw, _ := dynamicOf(t, res.Bundle)
		if bytes.Contains(raw, []byte(host)) || !bytes.Contains(raw, []byte("ok.example.com")) {
			t.Errorf("dynamic file should name only ok.example.com:\n%s", raw)
		}
		if len(res.Skipped) != 3 {
			t.Errorf("Skipped = %+v, want the three blocked routes", res.Skipped)
		}
	})
}

// TestSystemHasNoBackend is D-28: a route of a system-owned claim waits for Phase 28, and a route
// with no stack name cannot be addressed, both as no_backend (there is no fourth reason), even with a
// ready certificate. A standalone stack's route is not among them (D-15): it is served.
func TestSystemHasNoBackend(t *testing.T) {
	system := swarmRoute("sys.example.com", "/", "", "ui", 8080)
	system.OwnerKind = ingress.OwnerSystem
	system.StackName = ""
	system.Target = ""
	systemClaim := ingress.SnapshotClaim{Claim: ingress.Claim{Hostname: "sys.example.com", OwnerKind: ingress.OwnerSystem}}
	// A system route that still carries a swarm stack and target (a model that should not produce it):
	// the owner kind alone keeps it out.
	odd := swarmRoute("odd.example.com", "/", "shop", "web", 8080)
	odd.OwnerKind = ingress.OwnerSystem
	oddClaim := ingress.SnapshotClaim{
		Claim: ingress.Claim{Hostname: "odd.example.com", OwnerKind: ingress.OwnerSystem}, StackName: "shop", Target: ingress.TargetSwarm,
	}
	// A stack route with no stack name, on either target, has no name to build a backend from.
	nameless := standaloneRoute("nameless.example.com", "/", "", "app", 80)
	namelessSwarm := swarmRoute("nameless-swarm.example.com", "/", "", "app", 80)

	in := Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
			Claims: []ingress.SnapshotClaim{
				standaloneClaim("old.example.com"), systemClaim, oddClaim,
				standaloneClaim("nameless.example.com"), standaloneClaim("nameless-swarm.example.com"),
			},
			Routes: []ingress.SnapshotRoute{
				standaloneRoute("old.example.com", "/", "legacy", "app", 80), system, odd, nameless, namelessSwarm,
			},
		},
		Certs: []HostCert{
			{Hostname: "old.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "sys.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "odd.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "nameless.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "nameless-swarm.example.com", ChainPEM: chainA, KeyPEM: keyA},
		},
	}
	res, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []Skipped{
		{Hostname: "nameless-swarm.example.com", PathPrefix: "/", Stack: "", Service: "app", Reason: SkipNoBackend},
		{Hostname: "nameless.example.com", PathPrefix: "/", Stack: "", Service: "app", Reason: SkipNoBackend},
		{Hostname: "odd.example.com", PathPrefix: "/", Stack: "shop", Service: "web", Reason: SkipNoBackend},
		{Hostname: "sys.example.com", PathPrefix: "/", Stack: "", Service: "ui", Reason: SkipNoBackend},
	}
	if !slices.Equal(res.Skipped, want) {
		t.Errorf("Skipped = %+v\nwant      %+v", res.Skipped, want)
	}
	if got := res.Bundle.Manifest.Routes; len(got) != 1 || got[0].Hostname != "old.example.com" {
		t.Errorf("manifest routes = %+v, want only the standalone stack's old.example.com route", got)
	}
	if got := len(certFileNames(res.Bundle)); got != 2 {
		t.Errorf("bundle has %d certificate files, want the one pair of old.example.com: %v", got, certFileNames(res.Bundle))
	}
}

// TestStagingServesNothing is PROH-21-01 against the goldens: a staging certificate (Serving reports it
// as certificate_not_ready) leaves the dynamic file byte-identical to the empty bundle's, with the
// route skipped for that reason.
func TestStagingServesNothing(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(goldenDir, name)) //nolint:gosec // G304: a golden file under testdata
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(read("staging/tierplan.yml"), read("empty/tierplan.yml")) {
		t.Errorf("staging dynamic file differs from the empty one:\n%s", read("staging/tierplan.yml"))
	}
	res, err := Render(goldenScenarios()["staging"])
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipCertificateNotReady {
		t.Errorf("Skipped = %+v, want one certificate_not_ready entry", res.Skipped)
	}
}
