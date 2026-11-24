// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// TestStandaloneRouteRendersTheAlias is the tracer of Phase 25 (D-15): a standalone stack's route is
// served through the alias the attach gives its service, and a swarm route keeps its service DNS name.
// Nothing is skipped, and two stacks with a service of the same name get different backends.
func TestStandaloneRouteRendersTheAlias(t *testing.T) {
	in := Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{Exists: true, Enabled: true, ConfigVersion: 1},
			Claims:   []ingress.SnapshotClaim{standaloneClaim("old.example.com"), claimFor("new.example.com")},
			Routes: []ingress.SnapshotRoute{
				standaloneRoute("old.example.com", "/", "shop", "web", 8080),
				swarmRoute("new.example.com", "/", "shop", "web", 8080),
			},
		},
		Certs: []HostCert{
			{Hostname: "old.example.com", ChainPEM: chainA, KeyPEM: keyA},
			{Hostname: "new.example.com", ChainPEM: chainA, KeyPEM: keyA},
		},
		CertsVersion: 2,
	}
	res := mustRender(t, in)
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want nothing", res.Skipped)
	}

	standalone := "http://" + ingress.Alias("shop", "web") + ":8080"
	swarm := "http://shop_web:8080"
	if ingress.Alias("shop", "web") == ingress.Alias("blog", "web") {
		t.Fatalf("two stacks with a web service share the alias %q", ingress.Alias("shop", "web"))
	}

	_, doc := dynamicOf(t, res.Bundle)
	for _, url := range []string{standalone, swarm} {
		svc := digMap(t, doc, "http", "services", ServiceName(ingress.SchemeHTTP, url))
		servers, ok := dig(t, svc, "loadBalancer", "servers").([]any)
		if !ok || len(servers) != 1 {
			t.Fatalf("service of %s: servers = %v, want one", url, servers)
		}
		if got := dig(t, servers[0], "url"); got != url {
			t.Errorf("service url = %v, want %s", got, url)
		}
	}
	if got := len(digMap(t, doc, "http", "services")); got != 2 {
		t.Errorf("%d services, want 2", got)
	}
}
