// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// routedCompose has one routed service (web) and one that is not (db).
const routedCompose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
  db:
    image: postgres:16
`

// deployableWith loads compose through Deployable for target, with the ingress attachment when attach.
func deployableWith(t *testing.T, compose string, target stacks.Target, attach bool) *stacks.Loaded {
	t.Helper()
	in := stacks.CheckInput{Name: "shop", Target: target, Copy: copyOf(compose)}
	if attach {
		in.Ingress = &stacks.IngressAttach{Network: ingress.NetworkName}
	}
	loaded, err := stacks.Deployable(context.Background(), in)
	if err != nil {
		t.Fatalf("Deployable: %v", err)
	}
	return loaded
}

// networkKeys are the networks a service of the loaded project joins, sorted.
func networkKeys(l *stacks.Loaded, service string) []string {
	var keys []string
	for k := range l.Project.Services[service].Networks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestAttachRoutedServicesOnly(t *testing.T) {
	t.Parallel()
	loaded := deployableWith(t, routedCompose, stacks.TargetSwarm, true)
	if got, want := networkKeys(loaded, "web"), []string{"default", ingress.NetworkName}; !reflect.DeepEqual(got, want) {
		t.Errorf("web networks = %v, want %v", got, want)
	}
	if cfg := loaded.Project.Services["web"].Networks[ingress.NetworkName]; cfg != nil {
		t.Errorf("the attachment has a config %+v, want none (no alias)", cfg)
	}
	// compose-go gives a service with no networks the default one at load, so db keeps only that.
	if got := networkKeys(loaded, "db"); !reflect.DeepEqual(got, []string{"default"}) {
		t.Errorf("db networks = %v, want only default (it has no route)", got)
	}
	net, ok := loaded.Project.Networks[ingress.NetworkName]
	if !ok || !bool(net.External) || net.Name != ingress.NetworkName {
		t.Errorf("project network = %+v (declared %v), want %s as an external network", net, ok, ingress.NetworkName)
	}
	inj := loaded.Ingress
	if inj == nil || !inj.NetworkAdded || !reflect.DeepEqual(inj.Services, []string{"web"}) || len(inj.DefaultAdded) != 0 {
		t.Errorf("Loaded.Ingress = %+v, want the network added and web attached (compose-go already wrote its default)", inj)
	}

	// Ingress nil changes nothing anywhere.
	plain := deployableWith(t, routedCompose, stacks.TargetSwarm, false)
	if plain.Ingress != nil || !reflect.DeepEqual(networkKeys(plain, "web"), []string{"default"}) {
		t.Errorf("without Ingress: Loaded.Ingress = %+v, web networks = %v, want nothing injected", plain.Ingress, networkKeys(plain, "web"))
	}
	if _, ok := plain.Project.Networks[ingress.NetworkName]; ok {
		t.Error("without Ingress the project declares the ingress network")
	}

	// A standalone target is never attached, whatever the input says. Its routes sit under labels.
	const standalone = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "80"
`
	st := deployableWith(t, standalone, stacks.TargetStandalone, true)
	if st.Ingress != nil || !reflect.DeepEqual(networkKeys(st, "web"), []string{"default"}) {
		t.Errorf("standalone: Loaded.Ingress = %+v, web networks = %v, want nothing injected", st.Ingress, networkKeys(st, "web"))
	}
}

func TestAttachIsIdempotent(t *testing.T) {
	t.Parallel()
	const declared = `services:
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
	l := deployableWith(t, declared, stacks.TargetSwarm, true)
	if l.Ingress != nil {
		t.Errorf("Loaded.Ingress = %+v, want nil: the file already declares and uses the network", l.Ingress)
	}
	if got := networkKeys(l, "web"); !reflect.DeepEqual(got, []string{ingress.NetworkName}) {
		t.Errorf("web networks = %v, want exactly the ingress network", got)
	}
	if len(l.Project.Networks) != 1 {
		t.Errorf("project networks = %v, want one entry", l.Project.Networks)
	}
	if l.Project.Networks[ingress.NetworkName].Name != ingress.NetworkName {
		t.Errorf("the file's own declaration was replaced: %+v", l.Project.Networks[ingress.NetworkName])
	}
	// Checking the same copy twice gives the same project.
	again := deployableWith(t, declared, stacks.TargetSwarm, true)
	if !reflect.DeepEqual(l.Project.Networks, again.Project.Networks) || !reflect.DeepEqual(l.Project.Services["web"].Networks, again.Project.Services["web"].Networks) {
		t.Error("two checks of the same copy gave different projects")
	}

	// A network the file declares under another key, by name, is reused rather than duplicated.
	const byName = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
networks:
  edge:
    external: true
    name: tierplan-ingress
`
	n := deployableWith(t, byName, stacks.TargetSwarm, true)
	if _, dup := n.Project.Networks[ingress.NetworkName]; dup || len(n.Project.Networks) != 2 {
		t.Errorf("project networks = %v, want edge and default only, no second entry for the ingress network", n.Project.Networks)
	}
	if n.Ingress == nil || n.Ingress.NetworkAdded || !reflect.DeepEqual(n.Ingress.Services, []string{"web"}) {
		t.Errorf("Loaded.Ingress = %+v, want web attached and no network added", n.Ingress)
	}
	if got := networkKeys(n, "web"); !reflect.DeepEqual(got, []string{"default", "edge"}) {
		t.Errorf("web networks = %v, want default and edge", got)
	}
}

func TestAttachKeepsDefaultNetwork(t *testing.T) {
	t.Parallel()
	loaded := deployableWith(t, routedCompose, stacks.TargetSwarm, false)
	svc := loaded.Project.Services["web"]
	svc.Networks = map[string]*types.ServiceNetworkConfig{}
	loaded.Project.Services["web"] = svc

	inj := stacks.AttachIngressForTest(loaded.Project, []string{"web"}, ingress.NetworkName)
	if got := networkKeys(loaded, "web"); !reflect.DeepEqual(got, []string{"default", ingress.NetworkName}) {
		t.Errorf("web networks = %v, want the default kept next to the ingress network", got)
	}
	if inj == nil || !reflect.DeepEqual(inj.DefaultAdded, []string{"web"}) {
		t.Errorf("injected = %+v, want web recorded as default-added", inj)
	}
}

func TestComponentLabelIsRefused(t *testing.T) {
	t.Parallel()
	check := func(compose string, target stacks.Target) []stacks.Problem {
		res := stacks.Check(context.Background(), stacks.CheckInput{Name: "probe", Target: target, Copy: copyOf(compose)})
		var out []stacks.Problem
		for _, p := range res.Problems {
			if p.Code == "ingress_component_label" {
				out = append(out, p)
			}
		}
		return out
	}
	cases := []struct {
		name, compose string
		target        stacks.Target
		line, col     int
		key           string
	}{
		{"service labels", `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.component: ingress
`, stacks.TargetStandalone, 5, 7, "services.web.labels.tierplan.component"},
		{"service deploy labels", `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.component: ingress
`, stacks.TargetSwarm, 6, 9, "services.web.deploy.labels.tierplan.component"},
		{"service labels as a list", `services:
  web:
    image: nginx:1.27
    labels:
      - tierplan.component=ingress
`, stacks.TargetStandalone, 4, 5, "services.web.labels"},
		{"network", `services:
  web:
    image: nginx:1.27
    networks: [back]
networks:
  back:
    labels:
      tierplan.component: ingress
`, stacks.TargetStandalone, 8, 7, "networks.back.labels.tierplan.component"},
		{"volume", `services:
  web:
    image: nginx:1.27
    volumes: [data:/d]
volumes:
  data:
    labels:
      tierplan.component: ingress
`, stacks.TargetStandalone, 8, 7, "volumes.data.labels.tierplan.component"},
		{"secret", `services:
  web:
    image: nginx:1.27
    secrets: [s]
secrets:
  s:
    external: true
    labels:
      tierplan.component: ingress
`, stacks.TargetSwarm, 9, 7, "secrets.s.labels.tierplan.component"},
		{"config", `services:
  web:
    image: nginx:1.27
    configs: [c]
configs:
  c:
    content: cfg
    labels:
      tierplan.component: ingress
`, stacks.TargetSwarm, 9, 7, "configs.c.labels.tierplan.component"},
	}
	for _, c := range cases {
		got := check(c.compose, c.target)
		if len(got) != 1 {
			t.Errorf("%s: %d component-label problems (%+v), want one", c.name, len(got), got)
			continue
		}
		p := got[0]
		if p.Severity != stacks.SeverityError || p.Key != c.key || p.Line != c.line || p.Col != c.col {
			t.Errorf("%s: problem = %+v, want an error at %d:%d for %s", c.name, p, c.line, c.col, c.key)
		}
	}

	// The key is matched byte for byte; a file without the label has no problem.
	const near = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.components: x
      tierplan.component.x: y
      Tierplan.component: z
`
	if got := check(near, stacks.TargetStandalone); len(got) != 0 {
		t.Errorf("near-miss keys gave %+v, want none", got)
	}
	if got := check(routedCompose, stacks.TargetSwarm); len(got) != 0 {
		t.Errorf("a plain file gave %+v, want none", got)
	}
}

// A service may not give aliases on the shared ingress network (W-1): the proxy reaches each backend by
// its <stack>_<service> name, so an alias there could take another stack's backend name. The error is at
// the service's aliases for that network, under whatever key the file reaches the network by.
func TestIngressNetworkAliasesRefused(t *testing.T) {
	t.Parallel()
	refused := func(compose string, target stacks.Target) []stacks.Problem {
		in := stacks.CheckInput{Name: "shop", Target: target, Copy: copyOf(compose)}
		if target == stacks.TargetSwarm {
			in.Ingress = &stacks.IngressAttach{Network: ingress.NetworkName}
		}
		var out []stacks.Problem
		for _, p := range stacks.Check(context.Background(), in).Problems {
			if p.Code == stacks.CodeIngressNetworkAlias {
				out = append(out, p)
			}
		}
		return out
	}
	cases := []struct {
		name, compose string
		keys          []string
	}{
		{"alias on the network by its key", `services:
  web:
    image: nginx:1.27
    networks:
      tierplan-ingress:
        aliases: [other_web]
networks:
  tierplan-ingress:
    external: true
`, []string{"services.web.networks.tierplan-ingress.aliases"}},
		{"alias on the network under another key", `services:
  web:
    image: nginx:1.27
    networks:
      edge:
        aliases: [other_web, again]
  api:
    image: nginx:1.27
    networks:
      edge:
        aliases: [x]
networks:
  edge:
    external: true
    name: tierplan-ingress
`, []string{"services.api.networks.edge.aliases", "services.web.networks.edge.aliases"}},
		{"ingress network with no alias", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress:
    external: true
`, nil},
		{"aliases on another network", `services:
  web:
    image: nginx:1.27
    networks:
      back:
        aliases: [api]
networks:
  back: {}
`, nil},
	}
	for _, tc := range cases {
		for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
			got := refused(tc.compose, target)
			var keys []string
			for _, p := range got {
				if p.Severity != stacks.SeverityError || p.Line == 0 || !strings.Contains(p.Message, ingress.NetworkName) {
					t.Errorf("%s (%s): problem %+v, want an error at a line naming %s", tc.name, target, p, ingress.NetworkName)
				}
				keys = append(keys, p.Key)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, tc.keys) {
				t.Errorf("%s (%s): keys %v, want %v", tc.name, target, keys, tc.keys)
			}
		}
	}
}

// The network name is TierPlan's (WR-09): a file may only point at the shared network, as an external one.
func TestIngressNetworkNameIsReserved(t *testing.T) {
	t.Parallel()
	reserved := func(compose string, target stacks.Target) []stacks.Problem {
		in := stacks.CheckInput{Name: "shop", Target: target, Copy: copyOf(compose)}
		if target == stacks.TargetSwarm {
			in.Ingress = &stacks.IngressAttach{Network: ingress.NetworkName}
		}
		var out []stacks.Problem
		for _, p := range stacks.Check(context.Background(), in).Problems {
			if p.Code == stacks.CodeIngressNetworkReserved {
				out = append(out, p)
			}
		}
		return out
	}
	cases := []struct {
		name, compose string
		want          int
		key           string
	}{
		{"own network under the reserved key", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
networks:
  tierplan-ingress:
    driver: overlay
`, 1, "networks.tierplan-ingress"},
		{"own network under another key with the reserved name", `services:
  web:
    image: nginx:1.27
    networks: [edge]
networks:
  edge:
    name: tierplan-ingress
`, 1, "networks.edge"},
		{"external network under the reserved key with another name", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress:
    external: true
    name: somewhere-else
`, 1, "networks.tierplan-ingress"},
		{"external by key", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress:
    external: true
`, 0, ""},
		{"external by name under another key", `services:
  web:
    image: nginx:1.27
    networks: [edge]
networks:
  edge:
    external: true
    name: tierplan-ingress
`, 0, ""},
		{"no mention", routedCompose, 0, ""},
	}
	for _, c := range cases {
		got := reserved(c.compose, stacks.TargetSwarm)
		if len(got) != c.want {
			t.Errorf("%s: %d reserved-name problems (%+v), want %d", c.name, len(got), got, c.want)
			continue
		}
		if c.want == 1 && (got[0].Severity != stacks.SeverityError || got[0].Key != c.key || got[0].Line == 0) {
			t.Errorf("%s: problem = %+v, want an error located at %s", c.name, got[0], c.key)
		}
	}
	// The name is reserved for every target, not only the swarm one.
	if got := reserved(cases[0].compose, stacks.TargetStandalone); len(got) != 1 {
		t.Errorf("standalone: %d reserved-name problems, want one", len(got))
	}
}

// checkedWith is the loaded project of a file Deployable refuses (it has an error), with the ingress attachment tried.
func checkedWith(t *testing.T, compose string) *stacks.Loaded {
	t.Helper()
	res := stacks.Check(context.Background(), stacks.CheckInput{
		Name: "shop", Target: stacks.TargetSwarm, Copy: copyOf(compose), Ingress: &stacks.IngressAttach{Network: ingress.NetworkName},
	})
	if res.Loaded == nil {
		t.Fatalf("Check gave no project: %+v", res.Problems)
	}
	return res.Loaded
}

func TestAttachReusesOnlyAnExternalDeclaration(t *testing.T) {
	t.Parallel()
	const own = `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
networks:
  tierplan-ingress:
    driver: overlay
`
	l := checkedWith(t, own)
	decl := l.Project.Networks[ingress.NetworkName]
	if bool(decl.External) {
		t.Fatal("the stack's own network was turned into the shared one")
	}
	if l.Ingress != nil {
		t.Errorf("Loaded.Ingress = %+v, want nil: nothing is attached to a network the file declares as its own", l.Ingress)
	}

	// A non-external declaration under another key with the reserved name is not the shared network either.
	const byName = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "80"
networks:
  edge:
    name: tierplan-ingress
`
	n := checkedWith(t, byName)
	if n.Project.Networks["edge"].External {
		t.Error("edge, a network the stack creates, was reused as the shared one")
	}
	if n.Ingress != nil || !reflect.DeepEqual(networkKeys(n, "web"), []string{"default"}) {
		t.Errorf("Loaded.Ingress = %+v, web networks = %v, want nothing attached to a network the stack creates", n.Ingress, networkKeys(n, "web"))
	}
}
