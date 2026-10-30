// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeployPathsTakeTheirProjectFromTheCheck fails when code of internal/stackops or internal/api turns a
// compose file into a project with the bare loader, stacks.Load, instead of the check pipeline
// (stacks.Check, stacks.Deployable, which the roll back and the swarm start reach through PrepareRollback).
// A second way to load a compose file is how a stack that reviewed clean could not be deployed, rolled back,
// started or stopped (review CR-03, CR-04 and CR-05): the bare loader does not resolve env_file from the
// stack's own files and keeps build beside image.
//
// Repo.Declared in resources.go is the one allowed call. It reads only the names a revision declares, for the
// Resources tab and the removal preview, and a load failure there is a Declared with an Err: it never turns a
// revision into something to deploy, stop or start.
func TestDeployPathsTakeTheirProjectFromTheCheck(t *testing.T) {
	t.Parallel()
	const allowed = "resources.go"
	fset := token.NewFileSet()
	var parsed int
	for _, dir := range []string{".", "../api", "../stacks/rebuild"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			parsed++
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Load" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "stacks" {
					return true
				}
				if filepath.Base(path) == allowed && dir == "." {
					return true
				}
				t.Errorf("%s calls stacks.Load: a deploy, roll back, stop or start takes its project from stacks.Deployable or stacks.Check", fset.Position(call.Pos()))
				return true
			})
		}
	}
	if parsed == 0 {
		t.Fatal("no Go file was found to read: the guard checked nothing")
	}
}
