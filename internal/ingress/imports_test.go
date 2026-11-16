// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePrefix starts the import path of every package of this module.
const modulePrefix = "github.com/ianglennon/tierplan/"

// TestIngressPackageImportsNoInternalPackage pins D-22: the pure package imports nothing of this
// module, so internal/stacks can import it without a cycle.
func TestIngressPackageImportsNoInternalPackage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // G304: a Go file of this package, found by Glob
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
			}
			if strings.HasPrefix(path, modulePrefix) {
				t.Errorf("%s imports %s: package ingress must import no package of this module (D-22)", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go file was checked")
	}
}
