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

// Ownership is decided in one place, Ownership in ownership.go (plan 08-18, D-16, T-08-69). An adopted stack's objects
// carry no coaming.stack.id until its first deploy, so a site that compares the label with a stack's ID, or asks the
// engine for the objects that carry it, silently skips an adopted stack and, worse, the next such site is written the
// same way. The guard below fails on the two forms:
//
//   - a comparison (== or !=) with an index by stacks.LabelStackID, such as row.Labels[stacks.LabelStackID] == stack.ID;
//   - stacks.LabelStackID as the key of a label-filtered read (ServicesWithLabel and the others named ...WithLabel) or
//     inside a TaskFilter, such as c.NetworksWithLabel(ctx, stacks.LabelStackID, stack.ID).
//
// A read is keyed by the namespace or project label (Ownership.labelKey) and kept by what the predicate owns. What
// stays allowed is what decides nothing: writing the label (labels[stacks.LabelStackID] = id), and reading it to report
// an ID (discovery's orphan flag, stackOwner), because neither compares it with anything. A string literal of the label
// is not allowed either, as it would dodge the constant. The guard cannot follow a value through a variable, so a site
// that copies the label into one first is for review to catch; ownership.go is the one file excused.

// ownershipFile is the one file that may compare the label: the rule itself.
const ownershipFile = "ownership.go"

// labelStackID says whether an expression is stacks.LabelStackID.
func labelStackID(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "LabelStackID" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "stacks"
}

// indexesByLabelStackID says whether an expression reads a map by stacks.LabelStackID, parentheses aside.
func indexesByLabelStackID(e ast.Expr) bool {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	idx, ok := e.(*ast.IndexExpr)
	return ok && labelStackID(idx.Index)
}

// mentionsLabelStackID says whether stacks.LabelStackID is anywhere inside a node.
func mentionsLabelStackID(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(c ast.Node) bool {
		if e, ok := c.(ast.Expr); ok && labelStackID(e) {
			found = true
		}
		return !found
	})
	return found
}

// ownershipViolations are the places of a parsed file that decide ownership by the label instead of asking
// Ownership, each as a position and what is wrong.
func ownershipViolations(fset *token.FileSet, file *ast.File) []string {
	var out []string
	report := func(pos token.Pos, what string) { out = append(out, fset.Position(pos).String()+": "+what) }
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if (x.Op == token.EQL || x.Op == token.NEQ) && (indexesByLabelStackID(x.X) || indexesByLabelStackID(x.Y)) {
				report(x.Pos(), "compares coaming.stack.id with a value: ask Ownership (OwnsWorkload, OwnsNetwork, OwnsVolume, OwnsObject)")
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(sel.Sel.Name, "WithLabel") {
				return true
			}
			for _, arg := range x.Args {
				if labelStackID(arg) {
					report(x.Pos(), sel.Sel.Name+" is keyed by coaming.stack.id: read by the namespace or project label and keep what Ownership owns")
				}
			}
		case *ast.CompositeLit:
			if t := compositeTypeName(x.Type); t == "TaskFilter" && mentionsLabelStackID(x) {
				report(x.Pos(), "a TaskFilter is keyed by coaming.stack.id: read by the namespace or project label and keep the tasks of the services Ownership owns")
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && x.Value == `"coaming.stack.id"` {
				report(x.Pos(), "the coaming.stack.id label is written out: use stacks.LabelStackID, and only in ownership.go for a comparison")
			}
		}
		return true
	})
	return out
}

// compositeTypeName is the name of the type of a composite literal, with its package left off.
func compositeTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// TestOwnershipIsDecidedInOnePlace fails when a non-test file of internal/stackops or internal/api, ownership.go aside,
// decides who owns an object by coaming.stack.id.
func TestOwnershipIsDecidedInOnePlace(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	var parsed int
	for _, dir := range []string{".", "../api"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") || (dir == "." && filepath.Base(path) == ownershipFile) {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			parsed++
			for _, v := range ownershipViolations(fset, file) {
				t.Error(v)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("no Go file was found to read: the guard checked nothing")
	}
}

// TestTheOwnershipGuardFlagsWhatItGuards runs the guard over snippets, so it cannot go quiet unnoticed: each form it
// forbids is found, and each form it allows is not.
func TestTheOwnershipGuardFlagsWhatItGuards(t *testing.T) {
	t.Parallel()
	const head = "package p\n\nfunc f() {\n"
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"an equality":                {`_ = row.Labels[stacks.LabelStackID] == stack.ID`, 1},
		"an inequality":              {`_ = stack.ID != r.Labels[stacks.LabelStackID]`, 1},
		"an equality in parentheses": {`_ = (row.Labels[stacks.LabelStackID]) == id`, 1},
		"a service read by the ID":   {`_, _ = c.ServicesWithLabel(ctx, stacks.LabelStackID, stack.ID)`, 1},
		"a network read by the ID":   {`_, _ = c.NetworksWithLabel(ctx, stacks.LabelStackID, stack.ID)`, 1},
		"a volume read by the ID":    {`_, _ = c.VolumesWithLabel(ctx, stacks.LabelStackID, stack.ID)`, 1},
		"a secret read by the ID":    {`_, _ = c.SecretsWithLabel(ctx, stacks.LabelStackID, stack.ID)`, 1},
		"a task filter":              {`_, _ = c.Tasks(ctx, dockerx.TaskFilter{Labels: []string{stacks.LabelStackID + "=" + id}})`, 1},
		"the label written out":      {`_ = labels["coaming.stack.id"]`, 1},
		"a write":                    {`labels[stacks.LabelStackID] = id`, 0},
		"a report of the ID":         {`id := labels[stacks.LabelStackID]; _ = id`, 0},
		"a read by the namespace":    {`_, _ = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, name)`, 0},
		"a task filter by namespace": {`_, _ = c.Tasks(ctx, dockerx.TaskFilter{Labels: []string{stacks.LabelSwarmNamespace + "=" + name}})`, 0},
		"another comparison":         {`_ = labels[stacks.LabelSwarmNamespace] == name`, 0},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "snippet.go", head+tc.body+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := ownershipViolations(fset, file); len(got) != tc.want {
			t.Errorf("%s: %d violations %v, want %d", name, len(got), got, tc.want)
		}
	}
}
