// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"strings"
	"testing"
)

// FuzzSelect holds the resolver to the rules that keep a credential from leaving for a host it
// was not stored for (plan 05-08, T-05-28): whatever the reference and whatever the
// candidates, Select does not panic, the credential it chooses is for the image's own
// registry, a prefix it matched on ends at a path-segment boundary, and the Docker Hub
// fallback is only ever chosen for docker.io.
func FuzzSelect(f *testing.F) {
	for _, seed := range []struct{ ref, host1, prefix1, host2, prefix2 string }{
		{"ghcr.io/acme/team/app:1", "ghcr.io", "acme", "ghcr.io", "acme/team"},
		{"ghcr.io/acme/app", "ghcr.io", "acme", "ghcr.io", "acme"},
		{"registry.example.com:5000/x/y", "registry.example.com:5000", "", "docker.io", ""},
		{"acme/app", "docker.io", "acme", "docker.io", ""},
		{"nginx", "docker.io", "", "docker.io", "acme"},
		{"ghcr.io.evil.example/acme/app", "ghcr.io", "", "ghcr.io", "acme"},
		{"evil.example/ghcr.io/acme/app", "ghcr.io", "", "evil.example", "ghcr.io"},
		{"ghcr.io/acmecorp/app", "ghcr.io", "acme", "GHCR.IO", "acme"},
		{"index.docker.io/library/nginx:1", "docker.io", "library", "docker.io", ""},
	} {
		f.Add(seed.ref, seed.host1, seed.prefix1, seed.host2, seed.prefix2)
	}
	f.Fuzz(func(t *testing.T, refStr, host1, prefix1, host2, prefix2 string) {
		ref, err := ParseRef(refStr)
		if err != nil {
			t.Skip()
		}
		cands := []Candidate{
			{ID: "a", Name: "a", Layer: LayerInstance, Host: host1, Prefix: prefix1, HubFallback: host1 == "docker.io"},
			{ID: "b", Name: "b", Layer: LayerEnvironment, EnvID: "e", EnvName: "e", Host: host2, Prefix: prefix2, HubFallback: prefix2 == ""},
			{ID: "c", Name: "c", Layer: LayerInstance, Host: host2, Prefix: prefix1},
		}
		m := Select(cands, ref)
		if m.Cred == nil {
			if m.On != "" {
				t.Fatalf("anonymous but matched on %q", m.On)
			}
			return
		}
		if !strings.EqualFold(m.Cred.Host, ref.Domain) {
			t.Fatalf("chose a credential for %q to pull from %q", m.Cred.Host, ref.Domain)
		}
		switch m.On {
		case OnPrefix:
			if m.Cred.Prefix == "" || (ref.Path != m.Cred.Prefix && !strings.HasPrefix(ref.Path, m.Cred.Prefix+"/")) {
				t.Fatalf("matched on prefix %q for the path %q", m.Cred.Prefix, ref.Path)
			}
		case OnHost:
			if m.Cred.Prefix != "" || strings.EqualFold(ref.Domain, "docker.io") {
				t.Fatalf("matched on the host with prefix %q for %q", m.Cred.Prefix, ref.Domain)
			}
		case OnHubFallback:
			if !strings.EqualFold(ref.Domain, "docker.io") || !m.Cred.HubFallback {
				t.Fatalf("Hub fallback chosen for %q (flag %v)", ref.Domain, m.Cred.HubFallback)
			}
		default:
			t.Fatalf("chose %q on %q", m.Cred.ID, m.On)
		}
		used := 0
		for _, c := range m.Considered {
			if !strings.EqualFold(c.Candidate.Host, ref.Domain) {
				t.Fatalf("considered a credential for %q for an image on %q", c.Candidate.Host, ref.Domain)
			}
			if c.Result == ResultUsed {
				used++
			}
		}
		if used != 1 {
			t.Fatalf("%d candidates marked used", used)
		}
	})
}
