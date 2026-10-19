// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// FuzzPrescan: whatever bytes a person sends, the prescan neither panics nor runs past its work
// budget, and every problem it reports sits on a real line of the compose file. Seeded with the
// hostile corpus and the shapes the checks look at.
func FuzzPrescan(f *testing.F) {
	paths, err := filepath.Glob("testdata/hostile/*.yaml")
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(content)
	}
	for _, seed := range []string{
		"", "---\n---\n", "a: &a [*a]\n", "<<: *x\n", "services:\n  w:\n    <<: [*a, *b]\n",
		"services: {w: {env_file: [{path: a.env}, b.env, null, 3, [x]]}}\n",
		"secrets: {a: {file: x}}\nconfigs: [1]\n", "include: !!binary aGk=\n", "\t\x00\xff",
	} {
		f.Add([]byte(seed))
	}
	files := stacks.NewFileSet([]stacks.File{{Path: "compose.yaml"}, {Path: "a.env"}, {Path: "x"}})
	f.Fuzz(func(t *testing.T, data []byte) {
		problems, refs := stacks.Prescan(data, files)
		for _, p := range problems {
			if p.File == "" || p.Line < 1 || p.Col < 1 || p.Message == "" || p.Code == "" {
				t.Fatalf("problem %+v is not positioned", p)
			}
		}
		for _, r := range refs {
			if _, ok := files[r]; !ok {
				t.Fatalf("ref %q is not a file of the stack", r)
			}
		}
	})
}
