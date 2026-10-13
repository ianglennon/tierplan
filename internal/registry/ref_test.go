// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/registry"
)

const sha = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseRefNormalises(t *testing.T) {
	for _, tc := range []struct {
		in, full, domain, path, tag, digest string
	}{
		{"nginx", "docker.io/library/nginx:latest", "docker.io", "library/nginx", "latest", ""},
		{"nginx:1.27", "docker.io/library/nginx:1.27", "docker.io", "library/nginx", "1.27", ""},
		{"acme/app:1", "docker.io/acme/app:1", "docker.io", "acme/app", "1", ""},
		{"  acme/app:1\n", "docker.io/acme/app:1", "docker.io", "acme/app", "1", ""},
		{"localhost:5000/foo/bar:tag", "localhost:5000/foo/bar:tag", "localhost:5000", "foo/bar", "tag", ""},
		{"ghcr.io/acme/app", "ghcr.io/acme/app:latest", "ghcr.io", "acme/app", "latest", ""},
		{"nginx:1.27@" + sha, "docker.io/library/nginx:1.27@" + sha, "docker.io", "library/nginx", "1.27", sha},
		{"nginx@" + sha, "docker.io/library/nginx@" + sha, "docker.io", "library/nginx", "", sha},
	} {
		got, err := registry.ParseRef(tc.in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", tc.in, err)
			continue
		}
		if got.String() != tc.full || got.Domain != tc.domain || got.Path != tc.path || got.Tag != tc.tag || got.Digest != tc.digest {
			t.Errorf("ParseRef(%q) = %q domain=%q path=%q tag=%q digest=%q", tc.in, got.String(), got.Domain, got.Path, got.Tag, got.Digest)
		}
		if got.Repo() != tc.domain+"/"+tc.path {
			t.Errorf("Repo(%q) = %q", tc.in, got.Repo())
		}
	}
}

func TestParseRefRefuses(t *testing.T) {
	for _, in := range []string{"", "   ", "GHCR.io/Acme/app", "Bad Ref!", "nginx:", ":1", "a//b", strings.Repeat("a", 513)} {
		if _, err := registry.ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) = nil error, want an error", in)
		}
	}
}
