// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package lint

import (
	"sort"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/stacks/fieldclass"
)

// fakeIndex is an Index over a flat map of dotted path to scalar text ("" for a mapping or a
// sequence). Positions are the path's line in sorted order, so a test can say where a finding is.
type fakeIndex map[string]string

func (f fakeIndex) Lookup(path string) (Pos, bool) {
	if _, ok := f[path]; !ok {
		return Pos{}, false
	}
	paths := make([]string, 0, len(f))
	for p := range f {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	n := sort.SearchStrings(paths, path) + 1
	return Pos{Line: n, Col: 1, EndLine: n, EndCol: 2}, true
}

func (f fakeIndex) Keys(path string) []string {
	var out []string
	for p := range f {
		if rest, ok := strings.CutPrefix(p, path+"."); ok && !strings.Contains(rest, ".") {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

func (f fakeIndex) Scalar(path string) (string, bool) {
	v, ok := f[path]
	return v, ok && v != ""
}

func TestEverySwarmKeyTheConverterClassifiesHasAnAction(t *testing.T) {
	t.Parallel()
	for key, class := range fieldclass.Table {
		action, ok := swarmActions[key]
		if class == fieldclass.Converted {
			if ok {
				t.Errorf("%s is converted, so it has no action to word", key)
			}
			continue
		}
		if !ok || strings.TrimSpace(action) == "" || !strings.HasSuffix(action, ".") {
			t.Errorf("%s is %v on a swarm and has no action sentence (got %q)", key, class, action)
		}
	}
	for key := range swarmActions {
		if _, ok := fieldclass.Table[key]; !ok {
			t.Errorf("swarmActions words %s, which the converter no longer classifies", key)
		}
	}
}

func TestEnvFileIsLeftToThePrescanUntilItIsClassedConverted(t *testing.T) {
	t.Parallel()
	for key := range handledElsewhere {
		if fieldclass.Table[key] != fieldclass.Blocking {
			t.Errorf("%s is no longer Blocking in the converter's table; remove it from handledElsewhere", key)
		}
	}
	in := Input{
		Target:  TargetSwarm,
		Project: &types.Project{Services: types.Services{"web": {Name: "web", Image: "registry.example/web:1"}}},
		Index:   fakeIndex{"services": "", "services.web": "", "services.web.image": "x", "services.web.env_file": ""},
	}
	if got := Lint(in); len(got) != 0 {
		t.Errorf("findings = %+v, want none: the prescan owns env_file", got)
	}
}

func TestMessagesNameTheKeyThenSayWhatToDo(t *testing.T) {
	t.Parallel()
	if got, want := ignoredMessage("expose", TargetSwarm, "Remove the key."), "`expose` is ignored on Swarm. Remove the key."; got != want {
		t.Errorf("swarm = %q, want %q", got, want)
	}
	if got, want := ignoredMessage("deploy.mode", TargetStandalone, "Remove the key."), "`deploy.mode` is ignored on a standalone host. Remove the key."; got != want {
		t.Errorf("standalone = %q, want %q", got, want)
	}
	if got, want := blockingMessage("extends"), "`extends` can't be deployed to Swarm. Write the service out in full."; got != want {
		t.Errorf("blocking = %q, want %q", got, want)
	}
}

func TestEveryStandalonePathIsWordedWithAKeyAndASentence(t *testing.T) {
	t.Parallel()
	for _, path := range StandaloneUnsupported {
		a, ok := standaloneAdvice[path]
		if !ok || a.key == "" || !strings.HasSuffix(a.action, ".") {
			t.Errorf("%s has no wording (got %+v)", path, a)
		}
	}
	for path := range standaloneAdvice {
		found := false
		for _, p := range StandaloneUnsupported {
			found = found || p == path
		}
		if !found {
			t.Errorf("wording for %s, which is not in StandaloneUnsupported", path)
		}
	}
}

func TestExpandNamesEachKeyAndItemAPatternMatches(t *testing.T) {
	t.Parallel()
	ix := fakeIndex{
		"services": "", "services.a": "", "services.a.ports": "", "services.a.ports.0": "", "services.a.ports.0.mode": "host",
		"services.a.ports.1": "", "services.a.ports.1.mode": "ingress", "services.b": "", "services.b.ports": "",
	}
	got := expand(ix, strings.Split("services.*.ports.[].mode=host", "."), "")
	if len(got) != 1 || got[0] != "services.a.ports.0.mode" {
		t.Errorf("expand = %v, want only the host-mode port", got)
	}
	if got := expand(ix, strings.Split("services.*.deploy.mode", "."), ""); len(got) != 0 {
		t.Errorf("expand = %v, want none for a key the file lacks", got)
	}
}
