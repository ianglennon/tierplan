// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestJanitorNeverTouchesVolumesOrPrune is the safety rail of JAN-06 and T-11-04: no file of this
// package, tests aside, may call a volume removal or any prune endpoint of the engine, or import
// the HTTP layer. The day someone adds one, the build fails.
func TestJanitorNeverTouchesVolumesOrPrune(t *testing.T) {
	t.Parallel()
	forbidden := []string{"VolumeRemove", "VolumesPrune", "ContainersPrune", "ImagesPrune", "NetworksPrune", "BuildCachePrune", "/prune"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		raw, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(raw), bad) {
				t.Errorf("%s contains %q: the janitor never removes volumes and never prunes", name, bad)
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, raw, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasSuffix(path, "/internal/api") || strings.Contains(path, "/internal/api/") {
				t.Errorf("%s imports %s: the janitor is handed ports and never imports the HTTP layer", name, path)
			}
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned %d source files, want the whole package: the guard is not looking at it", scanned)
	}
}
