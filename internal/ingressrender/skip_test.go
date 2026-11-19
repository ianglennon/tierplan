// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
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
// per case. Only the claimed, certified, swarm-backed one is rendered; each other is returned with its
// stable reason, in hostname order.
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

	if got := len(res.Bundle.Manifest.Routes); got != 1 || res.Bundle.Manifest.Routes[0].Hostname != "a.example.com" {
		t.Errorf("manifest routes = %+v, want exactly the a.example.com route", res.Bundle.Manifest.Routes)
	}
	_, doc := dynamicOf(t, res.Bundle)
	routers, ok := dig(t, doc, "http", "routers").(map[string]any)
	if !ok || len(routers) != 1 {
		t.Errorf("dynamic file has %d routers, want 1: %v", len(routers), routers)
	}
	if got := len(certFileNames(res.Bundle)); got != 2 {
		t.Errorf("bundle has %d certificate files, want one pair: %v", got, certFileNames(res.Bundle))
	}

	want := []Skipped{
		{Hostname: "b.example.com", PathPrefix: "/", Stack: "shop", Service: "web", Reason: SkipNoCertificate},
		{Hostname: "c.example.com", PathPrefix: "/", Stack: "shop", Service: "web", Reason: SkipCertificateNotReady},
		{Hostname: "d.example.com", PathPrefix: "/", Stack: "legacy", Service: "app", Reason: SkipNoBackend},
	}
	if !slices.Equal(res.Skipped, want) {
		t.Errorf("Skipped = %+v\nwant      %+v", res.Skipped, want)
	}
}
