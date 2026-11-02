// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package compose_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ianglennon/tierplan/internal/rbac/compose"
)

// FuzzCheck feeds arbitrary bytes to Check. It must never panic, must only
// fail with ErrTooLarge or ErrInvalid, and must return findings that are
// ordered, positioned and free of exact duplicates.
func FuzzCheck(f *testing.F) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.yaml"))
	if err != nil {
		f.Fatal(err)
	}
	for _, name := range files {
		b, err := os.ReadFile(name) //nolint:gosec // a fixed testdata path
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, seed := range []string{
		"", "services: {}", "services: [", "a: &a [*a]", "services:\n  a: &a\n    <<: *a\n",
		"services:\n  a:\n    <<: [*x]\n", "x: &x {privileged: true}\nservices:\n  a:\n    <<: [*x, *x, *x]\n",
		"services:\n  a:\n    volumes: [\"${D}:/x\", \"/:/y\", {type: bind}, [1]]\n",
		"volumes:\n  v: {driver_opts: {o: [bind], device: {a: b}}}\n",
		"---\n---\nservices: 1\n--- \nservices:\n  a: {cap_add: ~}\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		fs, err := compose.Check(src)
		if err != nil {
			if !errors.Is(err, compose.ErrTooLarge) && !errors.Is(err, compose.ErrInvalid) {
				t.Fatalf("unexpected error type: %v", err)
			}
			if fs != nil {
				t.Fatalf("an error must carry no findings, got %+v", fs)
			}
			return
		}
		if compose.NeedsEnvAdmin(fs) != (len(fs) > 0) {
			t.Fatal("NeedsEnvAdmin must be true exactly when there are findings")
		}
		seen := map[compose.Finding]bool{}
		for i, f := range fs {
			if f.Rule == "" || f.Key == "" || f.Why == "" || f.Line < 1 {
				t.Fatalf("incomplete finding %+v", f)
			}
			if seen[f] {
				t.Fatalf("duplicate finding %+v", f)
			}
			seen[f] = true
			if i == 0 {
				continue
			}
			p := fs[i-1]
			if p.Line > f.Line || (p.Line == f.Line && (p.Column > f.Column || (p.Column == f.Column && p.Rule > f.Rule))) {
				t.Fatalf("findings out of order: %+v then %+v", p, f)
			}
		}
	})
}

// FuzzCheckResolved feeds arbitrary bytes and a table that answers every name to CheckResolved. It
// must never panic and must keep Check's promises: only ErrTooLarge or ErrInvalid, and complete,
// positioned findings, and it must refuse the same files Check refuses.
func FuzzCheckResolved(f *testing.F) {
	for _, seed := range []string{
		"", "services:\n  a:\n    volumes: [\"${D}:/x\", \"$D:/y\", \"${D:-/etc}:/z\", \"${D:?no}:/w\"]\n",
		"services:\n  a:\n    privileged: ${P}\n    network_mode: \"${N:-host}\"\n    cap_add: [\"$$C\", \"${C}\"]\n",
		"volumes:\n  v: {driver: \"${DR}\", driver_opts: {type: \"${T}\", o: \"${O}\", device: \"${DEV}\"}}\n",
		"services:\n  a:\n    volumes:\n      - type: \"${T}\"\n        source: \"${S}\"\n",
		"secrets:\n  k:\n    file: \"${F}\"\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		fs, err := compose.CheckResolved(src, func(string) (string, bool) { return "/var/run/docker.sock", true })
		if err != nil {
			if !errors.Is(err, compose.ErrTooLarge) && !errors.Is(err, compose.ErrInvalid) {
				t.Fatalf("unexpected error type: %v", err)
			}
			if _, plainErr := compose.Check(src); plainErr == nil {
				t.Fatalf("CheckResolved refused a file Check accepts: %v", err)
			}
			return
		}
		for _, f := range fs {
			if f.Rule == "" || f.Key == "" || f.Why == "" || f.Line < 1 {
				t.Fatalf("incomplete finding %+v", f)
			}
		}
		if _, plainErr := compose.Check(src); plainErr != nil {
			t.Fatalf("CheckResolved accepted a file Check refuses: %v", plainErr)
		}
	})
}
