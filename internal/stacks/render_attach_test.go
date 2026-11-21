// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// renderWith renders the compose as a swarm stack, with the ingress attachment when attach.
func renderWith(t *testing.T, compose string, attach bool) string {
	t.Helper()
	loaded := deployableWith(t, compose, stacks.TargetSwarm, attach)
	out, err := stacks.Render(stacks.RenderInput{Loaded: loaded})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func TestRenderIgnoresInjectedAttachment(t *testing.T) {
	t.Parallel()
	const withNetworks = `services:
  web:
    image: nginx:1.27
    networks: [front]
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
networks:
  front: {}
`
	for name, compose := range map[string]string{"no networks block": routedCompose, "explicit networks block": withNetworks} {
		attached, plain := renderWith(t, compose, true), renderWith(t, compose, false)
		if attached != plain {
			t.Errorf("%s: the rendering shows the attachment.\nwith ingress:\n%s\nwithout:\n%s", name, attached, plain)
		}
		if strings.Contains(attached, ingress.NetworkName) {
			t.Errorf("%s: the rendering names %s:\n%s", name, ingress.NetworkName, attached)
		}
	}

	// Render is a view: the loaded project keeps the attachment for the deploy.
	loaded := deployableWith(t, routedCompose, stacks.TargetSwarm, true)
	if _, err := stacks.Render(stacks.RenderInput{Loaded: loaded}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, ok := loaded.Project.Networks[ingress.NetworkName]; !ok {
		t.Error("Render took the ingress network out of the loaded project")
	}
	if _, ok := loaded.Project.Services["web"].Networks[ingress.NetworkName]; !ok {
		t.Error("Render took the attachment out of the loaded service")
	}
}

func TestRenderKeepsUserWrittenIngressNetwork(t *testing.T) {
	t.Parallel()
	const written = `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
networks:
  tierplan-ingress:
    external: true
`
	attached, plain := renderWith(t, written, true), renderWith(t, written, false)
	if attached != plain {
		t.Errorf("the rendering differs with Ingress set.\nwith:\n%s\nwithout:\n%s", attached, plain)
	}
	if n := strings.Count(attached, ingress.NetworkName); n < 3 {
		t.Errorf("the rendering mentions %s %d times, want the user's declaration and use kept:\n%s", ingress.NetworkName, n, attached)
	}
}
