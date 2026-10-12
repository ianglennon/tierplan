// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/registry"
)

// D-05, Task 1 answer "short-mask": a secret shorter than 12 characters keeps no last
// characters, so a short htpasswd password is never shown in large part or whole.
func TestLast4ShowsTheLastFourOfALongSecretAndNothingOfAShortOne(t *testing.T) {
	for _, c := range []struct {
		name, secret, want string
	}{
		{"empty", "", ""},
		{"three characters", "abc", ""},
		{"four characters", "abcd", ""},
		{"eleven characters", "abcdefghijk", ""},
		{"twelve characters", "abcdefghijkl", "ijkl"},
		{"a real token", "ghp_0123456789abcdefghijklmnopqrstuvwxyz", "wxyz"},
		// Characters, not bytes: eleven of these are 22 bytes and still a short secret.
		{"eleven multi-byte characters", strings.Repeat("é", 11), ""},
		{"twelve multi-byte characters", strings.Repeat("a", 8) + strings.Repeat("é", 4), "éééé"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := registry.Last4(c.secret); got != c.want {
				t.Errorf("Last4(%q) = %q, want %q", c.secret, got, c.want)
			}
		})
	}
}

var now = time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC)

func valid() registry.CreateInput {
	return registry.CreateInput{
		Name: "ghcr-acme", Type: registry.TypeGHCR, Prefix: "acme",
		Username: "bot", Secret: "ghp_0123456789abcdefghijklmnopqrstuvwxyz",
	}
}

func TestValidateFillsTheHostOfAPresetAndCleansWhatWasTyped(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*registry.CreateInput)
		host   string
		prefix string
	}{
		{"ghcr forces ghcr.io", func(in *registry.CreateInput) { in.Host = "evil.example" }, "ghcr.io", "acme"},
		{"dockerhub forces docker.io", func(in *registry.CreateInput) { in.Type = registry.TypeDockerHub; in.Host = "x.example" }, "docker.io", "acme"},
		{"gitlab defaults to registry.gitlab.com", func(in *registry.CreateInput) { in.Type = registry.TypeGitLab; in.Prefix = "" }, "registry.gitlab.com", ""},
		{"gitlab keeps a self-hosted host", func(in *registry.CreateInput) { in.Type = registry.TypeGitLab; in.Host = "Registry.Example.ORG" }, "registry.example.org", "acme"},
		{"generic is lower-cased", func(in *registry.CreateInput) { in.Type = registry.TypeGeneric; in.Host = " Harbor.Example.com:5000 " }, "harbor.example.com:5000", "acme"},
		{"generic Hub alias", func(in *registry.CreateInput) { in.Type = registry.TypeGeneric; in.Host = "index.docker.io" }, "docker.io", "acme"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := valid()
			c.mutate(&in)
			out, errs := registry.Validate(in, now)
			if len(errs) != 0 {
				t.Fatalf("errors = %+v", errs)
			}
			if out.Host != c.host || out.Prefix != c.prefix {
				t.Errorf("host/prefix = %q/%q, want %q/%q", out.Host, out.Prefix, c.host, c.prefix)
			}
		})
	}
}

func TestValidateRefusesWhatCannotBeStored(t *testing.T) {
	hub := func(in *registry.CreateInput) { in.Type = registry.TypeDockerHub; in.Prefix = "" }
	for _, c := range []struct {
		name   string
		mutate func(*registry.CreateInput)
		field  string
		code   string
	}{
		{"no name", func(in *registry.CreateInput) { in.Name = "  " }, "name", "name_required"},
		{"markup in the name", func(in *registry.CreateInput) { in.Name = "<img src=x onerror=alert(1)>" }, "name", "name_invalid"},
		{"a name of 65 characters", func(in *registry.CreateInput) { in.Name = strings.Repeat("a", 65) }, "name", "name_invalid"},
		{"an unknown type", func(in *registry.CreateInput) { in.Type = "ecr" }, "type", "type_invalid"},
		{"generic with no host", func(in *registry.CreateInput) { in.Type = registry.TypeGeneric; in.Host = "" }, "host", "host_required"},
		{"a host with a scheme", func(in *registry.CreateInput) { in.Type = registry.TypeGeneric; in.Host = "https://x.example" }, "host", "host_invalid"},
		{"a host with a path", func(in *registry.CreateInput) { in.Type = registry.TypeGeneric; in.Host = "x.example/acme" }, "host", "host_invalid"},
		{"a host with a bad port", func(in *registry.CreateInput) { in.Type = registry.TypeGeneric; in.Host = "x.example:99999" }, "host", "host_invalid"},
		{"an upper-case prefix", func(in *registry.CreateInput) { in.Prefix = "Acme" }, "prefix", "prefix_invalid"},
		{"a prefix with a leading slash", func(in *registry.CreateInput) { in.Prefix = "/acme" }, "prefix", "prefix_invalid"},
		{"a prefix with an empty segment", func(in *registry.CreateInput) { in.Prefix = "acme//app" }, "prefix", "prefix_invalid"},
		{"no username", func(in *registry.CreateInput) { in.Username = "" }, "username", "username_required"},
		{"no secret", func(in *registry.CreateInput) { in.Secret = " \n" }, "secret", "secret_required"},
		{"docker hub with neither a namespace nor the fallback", hub, "prefix", "hub_prefix_required"},
		{"the fallback on another registry", func(in *registry.CreateInput) { in.HubFallback = true }, "hubFallback", "hub_fallback_not_hub"},
		{"an expiry in the past", func(in *registry.CreateInput) { in.Expires = "2026-10-11" }, "expires", "expiry_past"},
		{"an expiry that is not a date", func(in *registry.CreateInput) { in.Expires = "next week" }, "expires", "expiry_invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := valid()
			c.mutate(&in)
			_, errs := registry.Validate(in, now)
			if len(errs) != 1 || errs[0].Field != c.field || errs[0].Code != c.code {
				t.Fatalf("errors = %+v, want %s %s", errs, c.field, c.code)
			}
		})
	}
}

func TestValidateAcceptsDockerHubByNamespaceByFallbackOrBoth(t *testing.T) {
	for _, c := range []struct {
		prefix   string
		fallback bool
	}{{"acme", false}, {"", true}, {"acme", true}} {
		in := valid()
		in.Type, in.Prefix, in.HubFallback = registry.TypeDockerHub, c.prefix, c.fallback
		if _, errs := registry.Validate(in, now); len(errs) != 0 {
			t.Errorf("prefix %q fallback %v: %+v", c.prefix, c.fallback, errs)
		}
	}
}

func TestValidateStoresAnExpiryAsTheEndOfThatDayUTC(t *testing.T) {
	for _, day := range []string{"2026-10-12", "2027-01-31"} {
		got, ok := registry.ExpiryEnd(day)
		if !ok {
			t.Fatalf("ExpiryEnd(%q) refused", day)
		}
		want, _ := time.Parse(time.DateOnly, day)
		want = want.Add(24*time.Hour - time.Second)
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("ExpiryEnd(%q) = %v, want %v", day, got, want)
		}
		in := valid()
		in.Expires = day
		if _, errs := registry.Validate(in, now); len(errs) != 0 {
			t.Errorf("expiry %s refused: %+v", day, errs)
		}
	}
	if _, ok := registry.ExpiryEnd("2026-02-30"); ok {
		t.Error("ExpiryEnd accepted a day that does not exist")
	}
}

func TestValidateReportsEveryFieldAtFaultAtOnce(t *testing.T) {
	_, errs := registry.Validate(registry.CreateInput{Type: registry.TypeGeneric}, now)
	got := map[string]string{}
	for _, e := range errs {
		got[e.Field] = e.Code
	}
	want := map[string]string{"name": "name_required", "host": "host_required", "username": "username_required", "secret": "secret_required"}
	if len(got) != len(want) {
		t.Fatalf("errors = %+v, want %v", errs, want)
	}
	for f, c := range want {
		if got[f] != c {
			t.Errorf("%s = %q, want %q", f, got[f], c)
		}
	}
}
