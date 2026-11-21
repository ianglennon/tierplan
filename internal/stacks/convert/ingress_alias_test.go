// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package convert_test

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
)

const ingressAliasCompose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
  db:
    image: postgres:16
`

// TestNoAliasOnIngressNetwork proves convert never gives a service its own name as an alias on the
// shared ingress network (D-37), whatever the project holds, while every other user-defined network
// keeps the alias.
func TestNoAliasOnIngressNetwork(t *testing.T) {
	t.Parallel()
	loaded, err := stacks.Deployable(context.Background(), stacks.CheckInput{
		Name: "shop", Target: stacks.TargetSwarm,
		Copy:    stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(ingressAliasCompose)}}},
		Ingress: &stacks.IngressAttach{Network: ingress.NetworkName},
	})
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	plan, err := convert.Convert(loaded.Project, convert.Options{StackName: "shop", StackID: "id-shop"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	byService := map[string][]swarm.NetworkAttachmentConfig{}
	for _, s := range plan.Services {
		byService[s.Short] = s.Spec.TaskTemplate.Networks
	}
	var sawIngress, sawDefault bool
	for _, att := range byService["web"] {
		switch att.Target {
		case ingress.NetworkName:
			sawIngress = true
			if len(att.Aliases) != 0 {
				t.Errorf("web on %s has aliases %v, want none", ingress.NetworkName, att.Aliases)
			}
		case "shop_default":
			sawDefault = true
			if len(att.Aliases) != 1 || att.Aliases[0] != "web" {
				t.Errorf("web on shop_default has aliases %v, want [web]", att.Aliases)
			}
		default:
			t.Errorf("web is on an unexpected network %q", att.Target)
		}
	}
	if !sawIngress || !sawDefault {
		t.Errorf("web networks = %+v, want both %s and shop_default", byService["web"], ingress.NetworkName)
	}
	for _, att := range byService["db"] {
		if att.Target == ingress.NetworkName {
			t.Errorf("db is attached to %s, want only the routed service", ingress.NetworkName)
		}
	}
	// The shared network is only referenced, never created by the stack.
	for _, n := range plan.Networks {
		if n.Name == ingress.NetworkName && !n.External {
			t.Errorf("network %s is planned for creation, want an external reference", n.Name)
		}
	}
}
