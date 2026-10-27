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

// TestChangedPathsKeyedLists: ChangedPaths lists the leaves countChanges counts, by the same rules: an environment
// entry is matched by its variable and not by its place, so a reorder lists nothing, and the paths are sorted.
func TestChangedPathsKeyedLists(t *testing.T) {
	t.Parallel()
	cur := svc(t, "shop_web", `"A=1","B=2"`, 1)
	next := svc(t, "shop_web", `"B=3","A=1","C=4"`, 1)

	got := stacks.ChangedPaths(cur, next)

	want := []stacks.PathChange{
		{Path: "TaskTemplate.ContainerSpec.Env[B]", From: `"B=2"`, To: `"B=3"`},
		{Path: "TaskTemplate.ContainerSpec.Env[C]", From: "", To: `"C=4"`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ChangedPaths = %+v, want %+v", got, want)
	}
	if n := len(stacks.ChangedPaths(cur, cur)); n != 0 {
		t.Errorf("ChangedPaths of a spec with itself = %d paths, want none", n)
	}
	reordered := svc(t, "shop_web", `"B=2","A=1"`, 1)
	if n := len(stacks.ChangedPaths(cur, reordered)); n != 0 {
		t.Errorf("ChangedPaths of a reordered environment = %d paths, want none", n)
	}
}

// TestMarkOwnershipOnly: a service is ownership-only when every difference is a bayplan.* label of the service or of
// its containers; one other difference keeps it a gap (T-08-55).
func TestMarkOwnershipOnly(t *testing.T) {
	t.Parallel()
	running := svc(t, "shop_web", `"A=1"`, 1)
	labelled := func(spec swarm.ServiceSpec) swarm.ServiceSpec {
		out := specOf(t, marshalSpec(t, spec))
		out.Labels["bayplan.stack.id"] = "id-1"
		return out
	}

	only := labelled(running)
	withEnv := labelled(svc(t, "shop_web", `"A=2"`, 1))
	foreign := specOf(t, marshalSpec(t, running))
	foreign.Labels["team"] = "platform"
	foreign.Labels["bayplan.stack.id"] = "id-1"
	containerLabel := specOf(t, marshalSpec(t, running))
	containerLabel.TaskTemplate.ContainerSpec.Labels = map[string]string{"bayplan.stack.id": "id-1"}

	for name, tc := range map[string]struct {
		desired swarm.ServiceSpec
		want    bool
	}{
		"only the bayplan label":                    {only, true},
		"a bayplan label on the containers":         {containerLabel, true},
		"a bayplan label and an environment change": {withEnv, false},
		"a bayplan label and another label":         {foreign, false},
	} {
		desired := map[string]swarm.ServiceSpec{"shop_web": tc.desired}
		live := map[string]swarm.ServiceSpec{"shop_web": running}
		rows := stacks.DiffServices(desired, live, stacks.Masker{})
		if len(rows) != 1 {
			t.Fatalf("%s: rows = %+v, want one changed service", name, rows)
		}

		stacks.MarkOwnershipOnly(rows, desired, live)

		if rows[0].OwnershipOnly != tc.want {
			t.Errorf("%s: OwnershipOnly = %v, want %v", name, rows[0].OwnershipOnly, tc.want)
		}
	}

	// An added or a removed service is never ownership-only.
	added := stacks.DiffServices(map[string]swarm.ServiceSpec{"shop_new": only}, nil, stacks.Masker{})
	stacks.MarkOwnershipOnly(added, map[string]swarm.ServiceSpec{"shop_new": only}, nil)
	if added[0].OwnershipOnly {
		t.Errorf("an added service was marked ownership-only")
	}
}

// TestOwnershipOnlyPathsKnowsTheLabelOfANamedVolumeMount: the converter puts Bayplan's ID label on the mount of a
// named volume, so that path is one of the ownership labels. A difference in the same mount beside it, or a label of
// the mount that is not Bayplan's, keeps the service a gap (G-08-4, T-08-55).
func TestOwnershipOnlyPathsKnowsTheLabelOfANamedVolumeMount(t *testing.T) {
	t.Parallel()
	const mountLabel = "TaskTemplate.ContainerSpec.Mounts[/data].VolumeOptions.Labels.bayplan.stack.id"
	for name, tc := range map[string]struct {
		paths []string
		want  bool
	}{
		"only the mount label":                    {[]string{mountLabel}, true},
		"the mount label and the service label":   {[]string{mountLabel, "Labels.bayplan.stack.id"}, true},
		"the mount label and its source":          {[]string{mountLabel, "TaskTemplate.ContainerSpec.Mounts[/data].Source"}, false},
		"another label of the mount":              {[]string{"TaskTemplate.ContainerSpec.Mounts[/data].VolumeOptions.Labels.com.docker.stack.namespace"}, false},
		"a label of the mount named like Bayplan": {[]string{"TaskTemplate.ContainerSpec.Mounts[/data].VolumeOptions.Labels.bayplanx"}, false},
		"a bayplan key elsewhere in a mount":      {[]string{"TaskTemplate.ContainerSpec.Mounts[/data].Source.bayplan.x"}, false},
	} {
		var changes []stacks.PathChange
		for _, p := range tc.paths {
			changes = append(changes, stacks.PathChange{Path: p})
		}
		if got := stacks.OwnershipOnlyPaths(changes); got != tc.want {
			t.Errorf("%s: OwnershipOnlyPaths = %v, want %v", name, got, tc.want)
		}
	}
}

// TestMarkOwnershipOnlyAcceptsTheLabelOfANamedVolumeMount: a service that differs from the running one in Bayplan's
// ID on the service, its containers and a volume mount is ownership-only; a real difference in that mount is not.
func TestMarkOwnershipOnlyAcceptsTheLabelOfANamedVolumeMount(t *testing.T) {
	t.Parallel()
	running := specOf(t, `{"Name":"shop_api","Labels":{"x":"1"},"TaskTemplate":{"ContainerSpec":{"Image":"api","Mounts":[{"Type":"volume","Source":"shop_data","Target":"/data","VolumeOptions":{"Labels":{"com.docker.stack.namespace":"shop"}}}]}}}`)
	desired := specOf(t, `{"Name":"shop_api","Labels":{"x":"1","bayplan.stack.id":"id-1"},"TaskTemplate":{"ContainerSpec":{"Image":"api","Labels":{"bayplan.stack.id":"id-1"},"Mounts":[{"Type":"volume","Source":"shop_data","Target":"/data","VolumeOptions":{"Labels":{"com.docker.stack.namespace":"shop","bayplan.stack.id":"id-1"}}}]}}}`)
	moved := specOf(t, marshalSpec(t, desired))
	moved.TaskTemplate.ContainerSpec.Mounts[0].Source = "shop_other"

	for name, tc := range map[string]struct {
		desired swarm.ServiceSpec
		want    bool
	}{
		"Bayplan's ID on service, containers and mount": {desired, true},
		"the same and another source":                   {moved, false},
	} {
		d := map[string]swarm.ServiceSpec{"shop_api": tc.desired}
		l := map[string]swarm.ServiceSpec{"shop_api": running}
		rows := stacks.DiffServices(d, l, stacks.Masker{})
		stacks.MarkOwnershipOnly(rows, d, l)
		if len(rows) != 1 || rows[0].OwnershipOnly != tc.want {
			t.Errorf("%s: rows = %+v, want one row with OwnershipOnly %v", name, rows, tc.want)
		}
	}
}

// TestMarkOwnershipOnlyIsNotFooledByACraftedMountTarget: the live mount's target is written so that every path inside
// it reads like Bayplan's label path. The paths alone would call a change of its source ownership-only; the two
// specs, with every ownership label stripped, still differ, so the service stays a gap (T-08-55, T-08-96).
func TestMarkOwnershipOnlyIsNotFooledByACraftedMountTarget(t *testing.T) {
	t.Parallel()
	const target = "/x].VolumeOptions.Labels.bayplan.y"
	mountOf := func(source string) string {
		return `{"Name":"shop_api","TaskTemplate":{"ContainerSpec":{"Image":"api","Mounts":[{"Type":"volume","Source":"` + source + `","Target":"` + target + `"}]}}}`
	}
	running, desired := specOf(t, mountOf("shop_data")), specOf(t, mountOf("shop_other"))
	d := map[string]swarm.ServiceSpec{"shop_api": desired}
	l := map[string]swarm.ServiceSpec{"shop_api": running}

	rows := stacks.DiffServices(d, l, stacks.Masker{})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one changed service", rows)
	}
	stacks.MarkOwnershipOnly(rows, d, l)

	if rows[0].OwnershipOnly {
		t.Errorf("a changed mount source was marked ownership-only because its target reads like a label path")
	}
}

// TestMarkOwnershipOnlyWhenTheRunningServiceHasNoLabelsAtAll: a service with no label anywhere, and its twin with only
// Bayplan's ID on the service, the containers and a mount, differ in nothing else: an emptied label map is an absent
// one.
func TestMarkOwnershipOnlyWhenTheRunningServiceHasNoLabelsAtAll(t *testing.T) {
	t.Parallel()
	running := specOf(t, `{"Name":"shop_api","TaskTemplate":{"ContainerSpec":{"Image":"api","Mounts":[{"Type":"volume","Source":"shop_data","Target":"/data","VolumeOptions":{}}]}}}`)
	desired := specOf(t, `{"Name":"shop_api","Labels":{"bayplan.stack.id":"id-1"},"TaskTemplate":{"ContainerSpec":{"Image":"api","Labels":{"bayplan.stack.id":"id-1"},"Mounts":[{"Type":"volume","Source":"shop_data","Target":"/data","VolumeOptions":{"Labels":{"bayplan.stack.id":"id-1"}}}]}}}`)
	d := map[string]swarm.ServiceSpec{"shop_api": desired}
	l := map[string]swarm.ServiceSpec{"shop_api": running}

	rows := stacks.DiffServices(d, l, stacks.Masker{})
	stacks.MarkOwnershipOnly(rows, d, l)

	if len(rows) != 1 || !rows[0].OwnershipOnly {
		t.Errorf("rows = %+v, want one ownership-only service", rows)
	}
}
