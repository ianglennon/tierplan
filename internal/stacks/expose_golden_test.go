// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// The Expose panel's label editor is TypeScript (web/src/features/stack-editor/expose), and the
// labels are read here. testdata/expose-golden.json is the one file both read: the vitest suite
// applies each case's ops to its input and must get its output byte for byte, and this test runs the
// real check on that output and must read the routes the case names. A key the editor writes that the
// server would read differently fails here (A-08, T-27-08).

// goldenDecl is one route as ingress.Declaration holds it.
type goldenDecl struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Path    string `json:"path"`
	Scheme  string `json:"scheme"`
	Strip   bool   `json:"strip"`
}

// goldenCase is one case of the file. Declarations and Problems are the server's side: a case with
// neither is only read by the vitest suite.
type goldenCase struct {
	Name         string          `json:"name"`
	Target       string          `json:"target"`
	Input        string          `json:"input"`
	Output       string          `json:"output"`
	Declarations *[]goldenDecl   `json:"declarations"`
	Problems     *[]string       `json:"problems"`
	Ops          json.RawMessage `json:"ops"`
}

func TestExposeGolden(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/expose-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("expose-golden.json: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("expose-golden.json has no cases")
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("two cases are named %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			target := stacks.Target(c.Target)
			if target != stacks.TargetSwarm && target != stacks.TargetStandalone {
				t.Fatalf("target %q is not a target", c.Target)
			}
			if c.Declarations == nil && c.Problems == nil {
				return // read by the vitest suite only
			}
			res := stacks.Check(context.Background(), stacks.CheckInput{
				Name: "probe", Target: target, Copy: copyOf(c.Output),
			})

			var codes []string
			for _, p := range res.Problems {
				if strings.HasPrefix(p.Code, "ingress_") {
					codes = append(codes, p.Code)
				}
			}
			sort.Strings(codes)
			want := []string{}
			if c.Problems != nil {
				want = slices.Clone(*c.Problems)
			}
			sort.Strings(want)
			if !slices.Equal(codes, want) {
				t.Errorf("ingress problem codes = %v, want %v (all problems: %+v)", codes, want, res.Problems)
			}

			if c.Declarations == nil {
				return
			}
			if res.Loaded == nil || res.Loaded.Project == nil {
				t.Fatalf("the output did not load: %+v", res.Problems)
			}
			got := []goldenDecl{}
			for _, d := range stacks.IngressDeclarations(res.Loaded.Project, target) {
				got = append(got, goldenDecl{
					Service: d.Service, Name: d.Name, Host: d.Host, Port: d.Port,
					Path: d.PathPrefix, Scheme: string(d.Scheme), Strip: d.Strip,
				})
			}
			if !slices.Equal(got, *c.Declarations) {
				t.Errorf("declarations = %+v, want %+v", got, *c.Declarations)
			}
		})
	}
}
