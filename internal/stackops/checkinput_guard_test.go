// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// unattachedMarker is what a stacks.CheckInput literal that deliberately does not attach the ingress network says,
// followed by the reason, on its own line or the line above.
const unattachedMarker = "ingress: unattached,"

// minCheckInputLiterals is how many literals the guard must have read: fewer means it was pointed at the wrong tree.
const minCheckInputLiterals = 10

// isStacksCheckInput reports whether a composite literal is of type stacks.CheckInput: only the selector counts, so
// registry.CheckInput and a bare CheckInput are left alone.
func isStacksCheckInput(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "CheckInput" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "stacks"
}

// decidesIngress reports whether a stacks.CheckInput literal names its Ingress or carries the reasoned marker on the
// line it starts on or the line above (a marker above the statement counts when the literal opens on the line below).
func decidesIngress(fset *token.FileSet, file *ast.File, lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Ingress" {
			return true
		}
	}
	line := fset.Position(lit.Pos()).Line
	for _, group := range file.Comments {
		for _, c := range group.List {
			end := fset.Position(c.End()).Line
			if (end == line || end == line-1) && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(c.Text, "//")), unattachedMarker) {
				return true
			}
		}
	}
	return false
}

// scanCheckInputs reads every non-test Go file under root, skipping directories named testdata and any path skip
// names, and returns each stacks.CheckInput literal that decides nothing about the ingress network as file:line, and
// how many literals it read.
func scanCheckInputs(root string, skip func(path string) bool) (offenders []string, checked int, err error) {
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" || (skip != nil && skip(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isStacksCheckInput(lit) {
				return true
			}
			checked++
			if !decidesIngress(fset, file, lit) {
				offenders = append(offenders, fset.Position(lit.Pos()).String())
			}
			return true
		})
		return nil
	})
	return offenders, checked, err
}

// TestCheckInputLiteralsDecideIngress fails when a stacks.CheckInput literal outside internal/stacks neither sets
// Ingress nor says why it does not attach (D-19): a caller of Check or Deployable that skips the decision would deploy
// routed services without the network, or show a phantom attachment.
func TestCheckInputLiteralsDecideIngress(t *testing.T) {
	t.Parallel()
	offenders, checked, err := scanCheckInputs("..", func(path string) bool { return filepath.ToSlash(path) == "../stacks" })
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("%s: stacks.CheckInput decides nothing about the ingress network: set Ingress, or write `// %s <reason>` on the line above", o, unattachedMarker)
	}
	if checked < minCheckInputLiterals {
		t.Fatalf("the guard read %d stacks.CheckInput literals, fewer than %d: it checked the wrong tree", checked, minCheckInputLiterals)
	}
}
