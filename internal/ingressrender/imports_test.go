// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedImports are the only non-standard-library packages the renderer may import: the YAML encoder,
// the pure ingress model and the bundle contract.
// internal/config is allowed for its connect path constants (D-10): the self route excludes the exact
// paths the agent and proxy addresses are built from. It imports no store, clock or network client.
var allowedImports = map[string]bool{
	"go.yaml.in/yaml/v3":                                 true,
	"github.com/ianglennon/tierplan/internal/config":     true,
	"github.com/ianglennon/tierplan/internal/ingress":    true,
	"github.com/ianglennon/tierplan/proto/ingressbundle": true,
}

// TestRendererImportsOnlyTheModelAndTheBundle keeps the renderer pure: no store, vault, clock, ACME or
// network package can be imported without this test failing.
func TestRendererImportsOnlyTheModelAndTheBundle(t *testing.T) {
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
			first, _, _ := strings.Cut(path, "/")
			if !strings.Contains(first, ".") {
				continue // the standard library
			}
			if !allowedImports[path] {
				t.Errorf("%s imports %s: the renderer may import only the standard library, go.yaml.in/yaml/v3, internal/config, internal/ingress and proto/ingressbundle", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go file was checked")
	}
}
