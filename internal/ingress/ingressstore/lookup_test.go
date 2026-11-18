// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// TestTracerARequestedHostnameLeadsToItsDomainsToken runs the whole path the certificate manager
// takes (D-08, D-21): a hostname judged against an environment's world leads, by the domain's id
// alone, to that domain and to its opened token.
func TestTracerARequestedHostnameLeadsToItsDomainsToken(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	const token = "cf-token-plaintext-1234" //nolint:gosec // G101: a fixed test fixture, not a credential
	created, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Token: []byte(token), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("register the domain: %v", err)
	}

	w, err := s.World(ctx, "env-a", nil)
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	dom, p, bad := ingress.CheckRequest("web.apps.example.com", w)
	if bad {
		t.Fatalf("CheckRequest refused a hostname under a registered domain: %+v", p)
	}
	if dom.ID != created.ID {
		t.Fatalf("CheckRequest domain = %q, want %q", dom.ID, created.ID)
	}

	byID, err := s.DomainByID(ctx, dom.ID)
	if err != nil {
		t.Fatalf("DomainByID: %v", err)
	}
	if byID.EnvID != "env-a" || byID.Suffix != "apps.example.com" || byID.Issuer != ingress.IssuerStaging {
		t.Fatalf("DomainByID = %+v, want env-a, apps.example.com, staging", byID)
	}
	got, err := s.DomainTokenByID(ctx, dom.ID)
	if err != nil {
		t.Fatalf("DomainTokenByID: %v", err)
	}
	if string(got) != token {
		t.Fatalf("DomainTokenByID did not return the saved token")
	}

	if _, err := s.DomainByID(ctx, "no-such-domain"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("DomainByID of an unknown id = %v, want ErrDomainNotFound", err)
	}
	if _, err := s.DomainTokenByID(ctx, "no-such-domain"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("DomainTokenByID of an unknown id = %v, want ErrDomainNotFound", err)
	}
	bare := registerDomain(t, s, "env-a", "bare.example.com")
	if _, err := s.DomainTokenByID(ctx, bare.ID); !errors.Is(err, ErrNoToken) {
		t.Fatalf("DomainTokenByID of a domain with no token = %v, want ErrNoToken", err)
	}
}

// TestInstallClaimsListsEveryEnvironment checks the reconciler's read: claims of two environments,
// each with its domain id and the names of its environment and stack.
func TestInstallClaimsListsEveryEnvironment(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	seedStack(t, st, "stack-a", "env-a", "web-a")
	seedStack(t, st, "stack-b", "env-b", "web-b")
	domA := registerDomain(t, s, "env-a", "a.example.com")
	domB := registerDomain(t, s, "env-b", "b.example.com")
	claim(t, s, "env-a", "stack-a", "web.a.example.com")
	claim(t, s, "env-b", "stack-b", "web.b.example.com")

	got, err := s.InstallClaims(ctx)
	if err != nil {
		t.Fatalf("InstallClaims: %v", err)
	}
	byHost := map[string]ingress.ClaimInfo{}
	for _, c := range got {
		byHost[c.Hostname] = c
	}
	if len(got) != 2 {
		t.Fatalf("InstallClaims returned %d claims, want 2: %+v", len(got), got)
	}
	for host, want := range map[string]struct{ env, envName, stack, stackName, domain string }{
		"web.a.example.com": {"env-a", "Alpha", "stack-a", "web-a", domA.ID},
		"web.b.example.com": {"env-b", "Beta", "stack-b", "web-b", domB.ID},
	} {
		c, ok := byHost[host]
		if !ok {
			t.Fatalf("InstallClaims has no claim for %s", host)
		}
		if c.EnvID != want.env || c.EnvName != want.envName || c.StackID != want.stack || c.StackName != want.stackName || c.DomainID != want.domain {
			t.Fatalf("claim %s = %+v, want %+v", host, c, want)
		}
	}
}
