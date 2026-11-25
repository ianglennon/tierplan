// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// vipBellIDs lists the value of every WarnVIP* string constant of warnings_vip.go.
func vipBellIDs(t *testing.T) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "warnings_vip.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "WarnVIP") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				ids[name.Name] = value
			}
		}
	}
	return ids
}

// An ID the server counts on the bell but the web has no case for would count and draw nothing (UI-SPEC S4). The bell's
// switch is a TypeScript file, so this reads it: every WarnVIP* constant must have its case in warnings.tsx.
func TestEveryVIPBellIDHasAWebCase(t *testing.T) {
	ids := vipBellIDs(t)
	if len(ids) < 7 {
		t.Fatalf("found %d WarnVIP constants, expected the seven of the virtual IP: the parse is wrong", len(ids))
	}
	ts, err := os.ReadFile("../../web/src/components/warnings.tsx")
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range ids {
		if !strings.Contains(string(ts), `case "`+id+`":`) {
			t.Errorf("%s (%q) has no case in web/src/components/warnings.tsx: the bell would count it and draw nothing", name, id)
		}
	}
}
