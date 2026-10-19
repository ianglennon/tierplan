// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

func svc(t *testing.T, name, env string, replicas int) swarm.ServiceSpec {
	t.Helper()
	raw := `{"Name":"` + name + `","Labels":{"com.docker.stack.namespace":"shop"},"TaskTemplate":{"ContainerSpec":{"Image":"nginx:1@sha256:aa","Env":[` + env + `]}},"Mode":{"Replicated":{"Replicas":` + strconv.Itoa(replicas) + `}}}`
	return specOf(t, raw)
}

func TestMaskSpecHidesWhatTheMaskerKnowsAndKeepsTheRawValueForEquality(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{{Name: "KEY", Value: "s3cret-token", Secret: true}}, nil)
	spec := svc(t, "shop_web", `"API_KEY=s3cret-token","MODE=prod","URL=https://u:s3cret-token@h/x"`, 1)
	masked, hidden := stacks.MaskSpec(spec, m)
	raw, _ := json.Marshal(masked)
	if strings.Contains(string(raw), "s3cret-token") {
		t.Fatalf("the masked spec holds the secret: %s", raw)
	}
	for _, want := range []string{`API_KEY=\u003chidden\u003e`, `MODE=prod`, `URL=https://u:\u003chidden\u003e@h/x`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("masked spec = %s, want %s", raw, want)
		}
	}
	if got := hidden["TaskTemplate.ContainerSpec.Env[API_KEY]"]; got != "API_KEY=s3cret-token" {
		t.Errorf("hidden[API_KEY] = %q, want the raw entry", got)
	}
	if _, ok := hidden["TaskTemplate.ContainerSpec.Env[MODE]"]; ok {
		t.Errorf("a plain value is listed as hidden: %v", hidden)
	}
}

func TestDiffServicesListsAddedChangedAndRemovedAndSkipsEqual(t *testing.T) {
	t.Parallel()
	desired := map[string]swarm.ServiceSpec{
		"shop_new":  svc(t, "shop_new", ``, 1),
		"shop_same": svc(t, "shop_same", `"A=1"`, 1),
		"shop_grow": svc(t, "shop_grow", ``, 2),
	}
	live := map[string]swarm.ServiceSpec{
		"shop_same": svc(t, "shop_same", `"A=1"`, 1),
		"shop_grow": svc(t, "shop_grow", ``, 1),
		"shop_gone": svc(t, "shop_gone", ``, 1),
	}
	got := stacks.DiffServices(desired, live, stacks.Masker{})
	var names []string
	for _, c := range got {
		names = append(names, c.Change+":"+c.Short)
	}
	if want := []string{"changed:grow", "added:new", "removed:gone"}; !slices.Equal(names, want) {
		t.Fatalf("changes = %v, want %v (added and changed by name, removed last)", names, want)
	}
	grow := got[0]
	if grow.Settings != 1 {
		t.Errorf("settings = %d, want 1 (the replica count)", grow.Settings)
	}
	if !strings.Contains(string(grow.Current), `"Replicas":1`) || !strings.Contains(string(grow.Next), `"Replicas":2`) {
		t.Errorf("current %s next %s", grow.Current, grow.Next)
	}
	if got[1].Current != nil || got[1].Next == nil {
		t.Errorf("an added service has Next and no Current: %+v", got[1])
	}
	if got[2].Current != nil || got[2].Next != nil {
		t.Errorf("a removed service is a summary row only: %+v", got[2])
	}
}

func TestDiffServicesDecidesEqualityBeforeMaskingAndNeverSendsTheSecret(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{{Name: "KEY", Value: "newsecret-value", Secret: true}}, nil)
	live := map[string]swarm.ServiceSpec{"shop_web": svc(t, "shop_web", `"API_KEY=oldsecret-value","MODE=prod"`, 1)}
	desired := map[string]swarm.ServiceSpec{"shop_web": svc(t, "shop_web", `"API_KEY=newsecret-value","MODE=prod"`, 1)}
	got := stacks.DiffServices(desired, live, m)
	if len(got) != 1 || got[0].Change != stacks.ChangeChanged {
		t.Fatalf("changes = %+v, want one changed service: the secret changed even though both sides read <hidden>", got)
	}
	c := got[0]
	if !slices.Equal(c.HiddenChanges, []string{"TaskTemplate.ContainerSpec.Env[API_KEY]"}) {
		t.Errorf("hiddenChanges = %v", c.HiddenChanges)
	}
	for _, side := range []string{string(c.Current), string(c.Next)} {
		if strings.Contains(side, "secret-value") {
			t.Errorf("a secret reached a side of the diff: %s", side)
		}
		if !strings.Contains(side, `API_KEY=\u003chidden\u003e`) {
			t.Errorf("a side does not read <hidden>: %s", side)
		}
	}
}

func TestDiffServicesAnUnchangedSecretIsNoChange(t *testing.T) {
	t.Parallel()
	m := stacks.NewMasker([]stacks.Var{{Name: "KEY", Value: "samesecret-value", Secret: true}}, nil)
	spec := map[string]swarm.ServiceSpec{"shop_web": svc(t, "shop_web", `"API_KEY=samesecret-value"`, 1)}
	if got := stacks.DiffServices(spec, spec, m); len(got) != 0 {
		t.Errorf("changes = %+v, want none", got)
	}
}

func TestDiffServicesCountsKeyedListsByName(t *testing.T) {
	t.Parallel()
	live := map[string]swarm.ServiceSpec{"shop_web": svc(t, "shop_web", `"A=1","B=2"`, 1)}
	desired := map[string]swarm.ServiceSpec{"shop_web": svc(t, "shop_web", `"B=3","A=1","C=4"`, 1)}
	got := stacks.DiffServices(desired, live, stacks.Masker{})
	if len(got) != 1 || got[0].Settings != 2 {
		t.Fatalf("changes = %+v, want 2 settings (B changed, C added; A and the order are not changes)", got)
	}
}
