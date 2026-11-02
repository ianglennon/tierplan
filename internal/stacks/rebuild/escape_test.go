// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rebuild_test

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

func ptr(s string) *string { return &s }

// TestDollarEscape is the rule of RESEARCH Pitfall 2: a value with a dollar sign survives Marshal and the
// load every deploy makes. compose-go reads a bare $ as the start of a variable, so Marshal doubles it.
func TestDollarEscape(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"PW":    "pa$word",
		"REF":   "${X}",
		"TWO":   "$$",
		"PCT":   "100%",
		"MIXED": "a$b${C}$$d%e",
		"PLAIN": "hello",
	}
	env := types.MappingWithEquals{}
	for k, v := range values {
		env[k] = ptr(v)
	}
	p := &types.Project{
		Name: "shop",
		Services: types.Services{"web": {
			Name: "web", Image: "nginx:1.27", Environment: env,
			Labels:  types.Labels{"com.example.$key": "v$al"},
			Command: types.ShellCommand{"sh", "-c", "echo $$HOME"},
		}},
	}

	raw, err := rebuild.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	text := string(raw)
	if strings.HasPrefix(text, "name:") || strings.Contains(text, "\nname:") {
		t.Errorf("the top-level name is still in the file:\n%s", text)
	}
	loaded, err := stacks.Load(testsupport.Context(t), stacks.LoadInput{
		Name: "shop", Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: raw}}},
	})
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, text)
	}
	if len(loaded.Unset) != 0 {
		t.Errorf("the load found variables to fill in %v, want none:\n%s", loaded.Unset, text)
	}
	svc := loaded.Project.Services["web"]
	for k, want := range values {
		got := svc.Environment[k]
		if got == nil || *got != want {
			t.Errorf("%s loads as %v, want %q", k, got, want)
		}
	}
	if got := svc.Labels["com.example.$key"]; got != "v$al" {
		t.Errorf("the label key and value load as %q, want v$al", got)
	}
	if got := strings.Join(svc.Command, " "); got != "sh -c echo $$HOME" {
		t.Errorf("command loads as %q, want sh -c echo $$HOME", got)
	}
}

func TestMarshalShortensPorts(t *testing.T) {
	t.Parallel()
	p := &types.Project{Services: types.Services{"web": {
		Name: "web", Image: "nginx:1.27",
		Ports: []types.ServicePortConfig{
			{Target: 80, Published: "8080", Protocol: "tcp", Mode: "ingress"},
			{Target: 443, Published: "8443", Protocol: "tcp", Mode: "host"},
		},
	}}}

	raw, err := rebuild.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	text := string(raw)
	if !strings.Contains(text, `"8080:80"`) {
		t.Errorf("the ingress port is not in the short form:\n%s", text)
	}
	if !strings.Contains(text, "mode: host") {
		t.Errorf("the host-mode port lost its long form:\n%s", text)
	}
}
