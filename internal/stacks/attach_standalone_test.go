// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// standaloneRouted has one routed service (web, its route under labels) and one that is not (db).
const standaloneRouted = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
  db:
    image: postgres:16
`

func TestStandaloneAttachRoutedServiceWithAlias(t *testing.T) {
	t.Parallel()
	loaded := deployableWith(t, standaloneRouted, stacks.TargetStandalone, true)
	if got, want := networkKeys(loaded, "web"), []string{"default", ingress.NetworkName}; !reflect.DeepEqual(got, want) {
		t.Errorf("web networks = %v, want %v", got, want)
	}
	cfg := loaded.Project.Services["web"].Networks[ingress.NetworkName]
	want := []string{ingress.Alias("shop", "web")}
	if cfg == nil || !reflect.DeepEqual(cfg.Aliases, want) {
		t.Errorf("the attachment = %+v, want exactly the aliases %v (never the bare name)", cfg, want)
	}
	if def := loaded.Project.Services["web"].Networks["default"]; def != nil && len(def.Aliases) != 0 {
		t.Errorf("the default network entry changed: %+v", def)
	}
	if got := networkKeys(loaded, "db"); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("db networks = %v, want only default (it has no route)", got)
	}
	net, ok := loaded.Project.Networks[ingress.NetworkName]
	if !ok || !bool(net.External) || net.Name != ingress.NetworkName {
		t.Errorf("project network = %+v (declared %v), want %s as an external network", net, ok, ingress.NetworkName)
	}
	if inj := loaded.Ingress; inj == nil || !inj.NetworkAdded || !reflect.DeepEqual(inj.Services, []string{"web"}) {
		t.Errorf("Loaded.Ingress = %+v, want the network added and web attached", inj)
	}
	// Without Ingress nothing is injected.
	plain := deployableWith(t, standaloneRouted, stacks.TargetStandalone, false)
	if plain.Ingress != nil || !reflect.DeepEqual(networkKeys(plain, "web"), []string{"default"}) {
		t.Errorf("without Ingress: Loaded.Ingress = %+v, web networks = %v", plain.Ingress, networkKeys(plain, "web"))
	}
}

func TestStandaloneAttachRenderRoundTrip(t *testing.T) {
	t.Parallel()
	render := func(attach bool) string {
		loaded := deployableWith(t, standaloneRouted, stacks.TargetStandalone, attach)
		out, err := stacks.Render(stacks.RenderInput{Loaded: loaded})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return string(out)
	}
	attached, plain := render(true), render(false)
	if attached != plain {
		t.Errorf("the rendering shows the attachment.\nwith ingress:\n%s\nwithout:\n%s", attached, plain)
	}
	if strings.Contains(attached, ingress.NetworkName) || strings.Contains(attached, ingress.Alias("shop", "web")) {
		t.Errorf("the rendering names the network or the alias:\n%s", attached)
	}
	// Render is a view: the loaded project keeps the attachment for the deploy.
	loaded := deployableWith(t, standaloneRouted, stacks.TargetStandalone, true)
	if _, err := stacks.Render(stacks.RenderInput{Loaded: loaded}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if cfg := loaded.Project.Services["web"].Networks[ingress.NetworkName]; cfg == nil || len(cfg.Aliases) != 1 {
		t.Errorf("Render took the attachment out of the loaded service: %+v", cfg)
	}
}
