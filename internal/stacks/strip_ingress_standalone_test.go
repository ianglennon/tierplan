// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// declaredEntry lists the shared network itself, with a setting of the person's own, on a routed service.
const declaredEntry = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
    networks:
      default:
      front:
        priority: 3
networks:
  front:
    external: true
    name: tierplan-ingress
`

func keysOf(svc types.ServiceConfig) []string {
	var out []string
	for k := range svc.Networks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestStripIngressStandalone is D-16: the attachment the injection added goes, by the network's key or its name, with
// the default network when the injection wrote it; every other network stays; a user-written entry keeps its settings
// and loses only the injected alias; a service without the attachment comes back as it was and the input is never
// changed.
func TestStripIngressStandalone(t *testing.T) {
	t.Parallel()

	t.Run("the injected attachment goes and the default network stays", func(t *testing.T) {
		t.Parallel()
		loaded := deployableWith(t, standaloneRouted, stacks.TargetStandalone, true)
		web := loaded.Project.Services["web"]
		got := stacks.StandaloneIngressStripper(loaded.Project, loaded.Ingress)(web)
		if want := []string{"default"}; !reflect.DeepEqual(keysOf(got), want) {
			t.Errorf("networks = %v, want %v", keysOf(got), want)
		}
		if want := []string{"default", ingress.NetworkName}; !reflect.DeepEqual(keysOf(web), want) {
			t.Errorf("the input has networks %v, want it unchanged", keysOf(web))
		}
	})

	t.Run("a service without the attachment is unchanged", func(t *testing.T) {
		t.Parallel()
		loaded := deployableWith(t, standaloneRouted, stacks.TargetStandalone, true)
		db := loaded.Project.Services["db"]
		if got := stacks.StandaloneIngressStripper(loaded.Project, loaded.Ingress)(db); !reflect.DeepEqual(got, db) {
			t.Errorf("db = %+v, want it as it was", got)
		}
		unattached := deployableWith(t, standaloneRouted, stacks.TargetStandalone, false)
		web := unattached.Project.Services["web"]
		if got := stacks.StripIngressStandalone(web, "shop", nil); !reflect.DeepEqual(got, web) {
			t.Errorf("web without ingress = %+v, want it as it was", got)
		}
	})

	t.Run("a container made attached reads as the file does with ingress off", func(t *testing.T) {
		t.Parallel()
		// With ingress off nothing was injected, and the desired side is the file; the running side still lists the entry.
		off := deployableWith(t, standaloneRouted, stacks.TargetStandalone, false)
		running := off.Project.Services["web"]
		running.Networks = map[string]*types.ServiceNetworkConfig{
			"default": nil, ingress.NetworkName: {Aliases: []string{ingress.Alias("shop", "web")}},
		}
		got := stacks.StandaloneIngressStripper(off.Project, nil)(running)
		if want := []string{"default"}; !reflect.DeepEqual(keysOf(got), want) {
			t.Errorf("networks = %v, want %v", keysOf(got), want)
		}
	})

	t.Run("a user-written entry keeps its settings and loses the injected alias", func(t *testing.T) {
		t.Parallel()
		loaded := deployableWith(t, declaredEntry, stacks.TargetStandalone, true)
		if inj := loaded.Ingress; inj == nil || !slices.Equal(inj.AliasAdded, []string{"web"}) {
			t.Fatalf("Loaded.Ingress = %+v, want the alias added to web's own entry", inj)
		}
		web := loaded.Project.Services["web"]
		strip := stacks.StandaloneIngressStripper(loaded.Project, loaded.Ingress)
		got := strip(web)
		if want := []string{"default", "front"}; !reflect.DeepEqual(keysOf(got), want) {
			t.Fatalf("networks = %v, want %v: the entry is the person's", keysOf(got), want)
		}
		if e := got.Networks["front"]; e == nil || e.Priority != 3 || len(e.Aliases) != 0 {
			t.Errorf("front = %+v, want the priority kept and no alias", e)
		}
		if e := web.Networks["front"]; e == nil || len(e.Aliases) != 1 {
			t.Errorf("the input's entry = %+v, want it unchanged", e)
		}
		// The same file with ingress off has the entry as the person wrote it, and a container made attached has
		// the alias on it: both come out alike.
		off := deployableWith(t, declaredEntry, stacks.TargetStandalone, false)
		want := stacks.StandaloneIngressStripper(off.Project, nil)(off.Project.Services["web"])
		ranAttached := web
		if !reflect.DeepEqual(stacks.StandaloneIngressStripper(off.Project, nil)(ranAttached), want) {
			t.Errorf("attached container = %+v, want it to read as %+v", stacks.StandaloneIngressStripper(off.Project, nil)(ranAttached), want)
		}
	})

	t.Run("the default network goes when the injection wrote it", func(t *testing.T) {
		t.Parallel()
		svc := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{
			"default": nil, "back": nil, ingress.NetworkName: {Aliases: []string{ingress.Alias("shop", "web")}},
		}}
		inj := &stacks.InjectedIngress{Network: ingress.NetworkName, Services: []string{"web"}, DefaultAdded: []string{"web"}}
		if got, want := keysOf(stacks.StripIngressStandalone(svc, "shop", inj)), []string{"back"}; !reflect.DeepEqual(got, want) {
			t.Errorf("networks = %v, want %v", got, want)
		}
		only := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{"default": nil, ingress.NetworkName: nil}}
		if got := stacks.StripIngressStandalone(only, "shop", inj); got.Networks != nil {
			t.Errorf("networks = %v, want none left", keysOf(got))
		}
	})

	t.Run("by the key the file gives the network", func(t *testing.T) {
		t.Parallel()
		svc := types.ServiceConfig{Name: "web", Networks: map[string]*types.ServiceNetworkConfig{"default": nil, "front": nil}}
		if got := keysOf(stacks.StripIngressStandalone(svc, "shop", nil)); !reflect.DeepEqual(got, []string{"default", "front"}) {
			t.Errorf("without the key networks = %v, want both: a key alone says nothing", got)
		}
		if got := keysOf(stacks.StripIngressStandalone(svc, "shop", nil, "front")); !reflect.DeepEqual(got, []string{"default"}) {
			t.Errorf("with the key networks = %v, want default", got)
		}
	})
}
