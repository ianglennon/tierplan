// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops/stackopstest"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
)

const standaloneRouted = `
services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
    networks: [back]
  db:
    image: postgres:17
    networks: [back]
networks:
  back: {}
`

// TestRebuildStandaloneDropsTheIngressNetwork (D-16, D-20): a project whose containers were made on the host's shared
// ingress bridge, with their alias, comes back as a file that neither declares the network nor lists it on a service nor
// carries the alias. The attachment is TierPlan's, added at deploy time, never part of the user's file.
func TestRebuildStandaloneDropsTheIngressNetwork(t *testing.T) {
	t.Parallel()
	for _, listed := range []bool{false, true} {
		name := "the host's network list leaves the shared network out"
		if listed {
			name = "the host's network list holds the shared network"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := stackopstest.HostAttached(t, "shop", "stack-shop", standaloneRouted, nil)
			doc := h.Docs["web"]
			if ep := doc.NetworkSettings.Networks[ingress.NetworkName]; ep == nil || !slices.Contains(ep.Aliases, ingress.Alias("shop", "web")) {
				t.Fatalf("the fixture's web is on %v, want it on %s with its alias", doc.NetworkSettings.Networks, ingress.NetworkName)
			}
			in := hostInputOf(t, h)
			if listed {
				in.Networks = append(in.Networks, rebuild.NetworkInfo{ID: "net-" + ingress.NetworkName, Name: ingress.NetworkName, Driver: "bridge"})
			}

			res, err := rebuild.Standalone(in)
			if err != nil {
				t.Fatalf("Standalone: %v", err)
			}

			if strings.Contains(string(res.YAML), ingress.NetworkName) || strings.Contains(string(res.YAML), ingress.Alias("shop", "web")) {
				t.Errorf("the file names the ingress network or the alias:\n%s", res.YAML)
			}
			p := loadStandalone(t, "shop", res.YAML)
			if _, declared := p.Networks[ingress.NetworkName]; declared {
				t.Errorf("project networks = %v, want no %s", p.Networks, ingress.NetworkName)
			}
			for _, svc := range []string{"web", "db"} {
				nets := p.Services[svc].Networks
				if _, on := nets[ingress.NetworkName]; on || len(nets) != 1 {
					t.Errorf("%s networks = %v, want only back", svc, nets)
				}
				if _, own := nets["back"]; !own {
					t.Errorf("%s networks = %v, want its own network back", svc, nets)
				}
			}
		})
	}
}
