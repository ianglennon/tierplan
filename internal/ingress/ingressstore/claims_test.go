// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/vault"
)

const host = "app.apps.example.com"

func TestTracerOneOwnerPerHostnameAcrossEnvironments(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	seedStack(t, st, "stack-b", "env-b", "web-b")

	// D-04: the same suffix registers in both environments.
	const token = "cf-token-plaintext-1234"
	domA, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "Apps.Example.com.", Issuer: ingress.IssuerStaging, Token: []byte(token), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("register the domain in env A: %v", err)
	}
	if domA.Suffix != "apps.example.com" || !domA.HasToken {
		t.Fatalf("domain = %+v, want the normalised suffix and a token", domA)
	}
	if _, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-b", Suffix: "apps.example.com", Issuer: ingress.IssuerProduction, Actor: "admin"}, nil); err != nil {
		t.Fatalf("register the same suffix in env B: %v", err)
	}
	if _, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil); !errors.Is(err, ErrDomainExists) {
		t.Fatalf("a second registration in one environment = %v, want ErrDomainExists", err)
	}

	// D-08: the token is sealed under the domain's own id.
	var sealed string
	if err := st.Read.QueryRow(`SELECT token_sealed FROM ingress_domains WHERE id = ?`, domA.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || sealed == token || strings.Contains(sealed, token) {
		t.Fatalf("token_sealed = %.20q, want an envelope that hides the token", sealed)
	}
	if got, err := v.Open(vault.ContextIngressDNSToken+domA.ID, sealed); err != nil || string(got) != token {
		t.Fatalf("Open under its own domain id = %q, %v; want the token", got, err)
	}
	if _, err := v.Open(vault.ContextIngressDNSToken+"another-domain-id", sealed); err == nil {
		t.Fatal("the sealed token opened under another domain id")
	}

	// A fresh stack with no declarations commits nothing (D-23).
	before := configVersion(t, st, "env-a")
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice"}); err != nil {
		t.Fatalf("empty replace: %v", err)
	}
	if after := configVersion(t, st, "env-a"); after != before {
		t.Fatalf("config_version moved %d -> %d on an empty replace", before, after)
	}

	// Stack A claims the hostname with a strip route (D-02).
	decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", PathPrefix: "/api", Port: 8080, Strip: true}
	plan, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: []ingress.Declaration{decl}})
	if err != nil {
		t.Fatalf("stack A claims %s: %v", host, err)
	}
	if got := plan.Hostnames(); len(got) != 1 || got[0] != host {
		t.Fatalf("plan hostnames = %v, want [%s]", got, host)
	}
	if after := configVersion(t, st, "env-a"); after != before+1 {
		t.Fatalf("config_version = %d after a claim, want %d", after, before+1)
	}
	var strip int
	if err := st.Read.QueryRow(`SELECT strip FROM ingress_routes WHERE hostname = ? AND path_prefix = '/api'`, host).Scan(&strip); err != nil || strip != 1 {
		t.Fatalf("stored strip = %d, %v; want 1", strip, err)
	}

	// LBL-03 / D-19: stack B in another environment is refused, and nothing of it is written.
	versionB := configVersion(t, st, "env-b")
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-b", Actor: "bob", Decls: []ingress.Declaration{decl},
		CanView: func(env string) bool { return env == "env-a" }})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("stack B claim = %v, want *RefusedError", err)
	}
	// Env B registered the suffix too, so the domain rule passes and the owner rule is what refuses.
	if len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeHostTaken {
		t.Fatalf("problems = %+v, want only %s", refused.Problems, ingress.CodeHostTaken)
	}
	// D-20: the caller may see env A, so the holder is named.
	if m := refused.Problems[0].Message; !strings.Contains(m, "web-a") || !strings.Contains(m, "Alpha") {
		t.Errorf("message %q does not name the holder the caller can see", m)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE env_id = 'env-b' OR stack_id = 'stack-b'`); n != 0 {
		t.Errorf("%d claim rows of stack B exist after a refusal", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_routes WHERE env_id = 'env-b'`); n != 0 {
		t.Errorf("%d route rows of env B exist after a refusal", n)
	}
	if after := configVersion(t, st, "env-b"); after != versionB {
		t.Errorf("env B config_version moved %d -> %d on a refusal", versionB, after)
	}

	// D-20: a caller who cannot see env A is told neither the stack nor the environment.
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-b", Actor: "bob", Decls: []ingress.Declaration{decl}})
	if !errors.As(err, &refused) || len(refused.Problems) != 1 {
		t.Fatalf("stack B claim without visibility = %v, want one problem", err)
	}
	if m := refused.Problems[0].Message; strings.Contains(m, "web-a") || strings.Contains(m, "Alpha") {
		t.Errorf("message %q leaks the holder to a caller who cannot see it", m)
	}

	// D-01: the database refuses a second owner of the hostname on its own.
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at)
		 SELECT ?, 'env-b', id, 'stack', 'stack-b', 'raw', 1 FROM ingress_domains WHERE env_id = 'env-b'`, host); err == nil {
		t.Fatal("a raw INSERT of a second owner of the hostname succeeded")
	}

	// LBL-04: a hostname outside every domain of the environment is refused.
	outside := ingress.Declaration{Service: "web", Host: "app.other.example.org", Label: "tierplan.ingress.host", Port: 80}
	_, err = s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-a", StackID: "stack-a", Actor: "alice", Decls: []ingress.Declaration{outside}})
	if !errors.As(err, &refused) || len(refused.Problems) != 1 || refused.Problems[0].Code != ingress.CodeDomainNotAllowed {
		t.Fatalf("outside every domain = %v, want %s", err, ingress.CodeDomainNotAllowed)
	}
	// ... and the refusal left stack A's earlier claim in place.
	if n := count(t, st, `SELECT COUNT(*) FROM ingress_hostnames WHERE stack_id = 'stack-a'`); n != 1 {
		t.Errorf("stack A holds %d claims after a refused replacement, want its earlier 1", n)
	}

	// A stack that is not in the environment is not found.
	if _, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: "env-b", StackID: "stack-a", Actor: "x"}); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("a stack of another environment = %v, want ErrStackNotFound", err)
	}
}
