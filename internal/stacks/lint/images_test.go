// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package lint

import "testing"

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMovingTag(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]bool{
		"nginx":                        true,
		"nginx:latest":                 true,
		"registry:5000/app":            true,
		"ghcr.io/owner/repo":           true,
		"docker.io/library/nginx":      true,
		"nginx:1.27":                   false,
		"nginx:1.27-alpine":            false,
		"registry:5000/app:2":          false,
		"localhost:5000/app:v1":        false,
		"nginx@" + digest:              false,
		"nginx:latest@" + digest:       false,
		"ghcr.io/owner/repo@" + digest: false,
		// A reference that does not parse is not for this rule to judge.
		"":            false,
		"NOT A REF":   false,
		"Upper/Case:": false,
	} {
		if got := movingTag(ref); got != want {
			t.Errorf("movingTag(%q) = %v, want %v", ref, got, want)
		}
	}
}
