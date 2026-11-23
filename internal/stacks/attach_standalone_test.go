// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
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

// standaloneCheck checks compose as the standalone stack shop with the ingress attachment.
func standaloneCheck(compose string) stacks.CheckResult {
	return stacks.Check(context.Background(), stacks.CheckInput{
		Name: "shop", Target: stacks.TargetStandalone, Copy: copyOf(compose),
		Ingress: &stacks.IngressAttach{Network: ingress.NetworkName},
	})
}

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

func TestStandaloneAttachIsIdempotent(t *testing.T) {
	t.Parallel()
	loaded := deployableWith(t, standaloneRouted, stacks.TargetStandalone, true)
	before := networkKeys(loaded, "web")
	cfgBefore := *loaded.Project.Services["web"].Networks[ingress.NetworkName]
	if inj := stacks.AttachIngressForTest(loaded.Project, stacks.TargetStandalone, []string{"web"}, ingress.NetworkName); inj != nil {
		t.Errorf("a second attach returned %+v, want nil", inj)
	}
	if got := networkKeys(loaded, "web"); !reflect.DeepEqual(got, before) {
		t.Errorf("web networks changed: %v -> %v", before, got)
	}
	if cfg := loaded.Project.Services["web"].Networks[ingress.NetworkName]; !reflect.DeepEqual(*cfg, cfgBefore) {
		t.Errorf("the attachment changed: %+v -> %+v", cfgBefore, *cfg)
	}
}

func TestStandaloneAttachAddsTheAliasToADeclaredEntry(t *testing.T) {
	t.Parallel()
	const declared = `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
networks:
  tierplan-ingress:
    external: true
`
	loaded := deployableWith(t, declared, stacks.TargetStandalone, true)
	cfg := loaded.Project.Services["web"].Networks[ingress.NetworkName]
	if want := []string{ingress.Alias("shop", "web")}; cfg == nil || !reflect.DeepEqual(cfg.Aliases, want) {
		t.Errorf("the declared entry = %+v, want the aliases %v", cfg, want)
	}
	if inj := loaded.Ingress; inj == nil || inj.NetworkAdded || len(inj.Services) != 0 || !reflect.DeepEqual(inj.AliasAdded, []string{"web"}) {
		t.Errorf("Loaded.Ingress = %+v, want only the alias on web recorded", inj)
	}
	out, err := stacks.Render(stacks.RenderInput{Loaded: loaded})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	plain, err := stacks.Render(stacks.RenderInput{Loaded: deployableWith(t, declared, stacks.TargetStandalone, false)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(out) != string(plain) {
		t.Errorf("the rendering differs from the file as written.\nattached:\n%s\nplain:\n%s", out, plain)
	}
	if !strings.Contains(string(out), ingress.NetworkName) || strings.Contains(string(out), "aliases") {
		t.Errorf("the rendering must keep the user's entry and drop the alias:\n%s", out)
	}
}

func TestStandaloneInjectedAliasIsNotLinted(t *testing.T) {
	t.Parallel()
	for name, compose := range map[string]string{
		"implicit attachment": standaloneRouted,
		"declared entry": `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
networks:
  tierplan-ingress:
    external: true
`,
	} {
		res := standaloneCheck(compose)
		for _, p := range res.Problems {
			if p.Code == stacks.CodeIngressNetworkAlias {
				t.Errorf("%s: the injected alias was reported: %+v", name, p)
			}
		}
		if res.Loaded == nil || res.Loaded.Ingress == nil {
			t.Errorf("%s: nothing was attached (problems %+v)", name, res.Problems)
		}
	}
}

func TestStandaloneUserAliasStillRefused(t *testing.T) {
	t.Parallel()
	const compose = `services:
  web:
    image: nginx:1.27
    networks:
      tierplan-ingress:
        aliases: [web]
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
networks:
  tierplan-ingress:
    external: true
`
	var keys []string
	for _, p := range standaloneCheck(compose).Problems {
		if p.Code == stacks.CodeIngressNetworkAlias {
			keys = append(keys, p.Key)
		}
	}
	if want := []string{"services.web.networks.tierplan-ingress.aliases"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("ingress_network_alias keys = %v, want %v", keys, want)
	}
}
