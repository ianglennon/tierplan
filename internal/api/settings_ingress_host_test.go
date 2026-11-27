// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// Settings may never point agents and proxies at a hostname the ingress serves (plan 28-23,
// D-07, D-10, SELF-05).

// ingressHostsFixture is a settings fixture whose ingress holds hosts.
func ingressHostsFixture(t *testing.T, hosts map[string]ingress.ClaimInfo) (*authFixture, string, string) {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	f.settingsIngressHosts = func(context.Context) (map[string]ingress.ClaimInfo, error) { return hosts, nil }
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	return f, cookie, csrf
}

func stackClaim(host string) ingress.ClaimInfo {
	return ingress.ClaimInfo{
		Claim:   ingress.Claim{EnvID: "env-1", Hostname: host, OwnerKind: ingress.OwnerStack, StackID: "stack-1"},
		EnvName: "lab", StackName: "app",
	}
}

func systemClaim(host string) ingress.ClaimInfo {
	return ingress.ClaimInfo{
		Claim:   ingress.Claim{EnvID: "env-1", Hostname: host, OwnerKind: ingress.OwnerSystem},
		EnvName: "lab",
	}
}

// assertFieldProblems checks a 422 validation_failed that lists exactly want, location to code.
func assertFieldProblems(t *testing.T, rec *httptest.ResponseRecorder, want map[string]string) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s, want 422", rec.Code, rec.Body)
	}
	p := decodeProblem(t, rec)
	if p.Type != "validation_failed" || len(p.Errors) != len(want) {
		t.Fatalf("problem = %+v, want validation_failed with %v", p, want)
	}
	for _, e := range p.Errors {
		if want[e.Location] != e.Message {
			t.Fatalf("problem = %+v, want %v", p, want)
		}
	}
}

func TestAgentURLMayNotBeAnIngressHost(t *testing.T) {
	t.Parallel()
	f, cookie, csrf := ingressHostsFixture(t, map[string]ingress.ClaimInfo{"app.apps.example.org": stackClaim("app.apps.example.org")})
	f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://example.com"})
	audited := len(f.audits(t, "settings.updated"))

	for _, bad := range []string{"https://app.apps.example.org:7480", "https://APP.apps.example.org./"} {
		rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": bad})
		assertFieldProblems(t, rec, map[string]string{"body.agentUrl": "agent_url_is_ingress_host"})
	}
	if f.inst.AgentURL() != "" {
		t.Fatalf("a refused agent URL was cached: %q", f.inst.AgentURL())
	}
	var stored int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'agent_url' AND value != ''`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("stored agent URLs = %d, %v, want none", stored, err)
	}
	if n := len(f.audits(t, "settings.updated")); n != audited {
		t.Fatalf("%d settings.updated entries, want %d: a refusal writes nothing", n, audited)
	}

	// A refusal in the same save keeps the other fields too.
	rec := f.patchSettings(t, cookie, csrf, map[string]string{"instanceName": "Renamed", "agentUrl": "https://app.apps.example.org"})
	assertFieldProblems(t, rec, map[string]string{"body.agentUrl": "agent_url_is_ingress_host"})
	if f.inst.Name() == "Renamed" {
		t.Fatal("a refused save renamed the instance")
	}

	if rec := f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": "https://tierplan.internal:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("a free agent URL: %d %s", rec.Code, rec.Body)
	}
	if f.inst.AgentURL() != "https://tierplan.internal:7480" {
		t.Fatalf("agent URL = %q", f.inst.AgentURL())
	}
}

func TestPublicURLMayBeTheRouteHostOnlyWithAFreeAgentURL(t *testing.T) {
	t.Parallel()
	hosts := map[string]ingress.ClaimInfo{
		"tp.apps.example.org":  systemClaim("tp.apps.example.org"),
		"app.apps.example.org": stackClaim("app.apps.example.org"),
	}
	publicRefused := map[string]string{"body.publicUrl": "public_url_is_ingress_host"}

	f, cookie, csrf := ingressHostsFixture(t, hosts)
	audited := len(f.audits(t, "settings.updated"))

	// No agent URL stored or sent: agents would dial the route.
	assertFieldProblems(t, f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://tp.apps.example.org"}), publicRefused)
	// A stack's host is refused the same way.
	assertFieldProblems(t, f.patchSettings(t, cookie, csrf, map[string]string{"publicUrl": "https://app.apps.example.org/tierplan"}), publicRefused)
	// An agent URL that is itself claimed: both fields are reported.
	assertFieldProblems(t, f.patchSettings(t, cookie, csrf, map[string]string{
		"publicUrl": "https://tp.apps.example.org", "agentUrl": "https://app.apps.example.org:7480",
	}), map[string]string{"body.publicUrl": "public_url_is_ingress_host", "body.agentUrl": "agent_url_is_ingress_host"})
	if f.inst.PublicURL() != "" || f.inst.AgentURL() != "" {
		t.Fatalf("a refused save stored public %q agent %q", f.inst.PublicURL(), f.inst.AgentURL())
	}
	if n := len(f.audits(t, "settings.updated")); n != audited {
		t.Fatalf("%d settings.updated entries, want %d: a refusal writes nothing", n, audited)
	}

	// A free agent URL in the same save: both are saved.
	rec := f.patchSettings(t, cookie, csrf, map[string]string{
		"publicUrl": "https://tp.apps.example.org", "agentUrl": "https://tierplan.internal:7480",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("route host with a free agent URL: %d %s", rec.Code, rec.Body)
	}
	if f.inst.PublicURL() != "https://tp.apps.example.org" || f.inst.AgentURL() != "https://tierplan.internal:7480" {
		t.Fatalf("public %q agent %q, want both saved", f.inst.PublicURL(), f.inst.AgentURL())
	}
	// Clearing the free agent URL would leave agents dialling the route.
	assertFieldProblems(t, f.patchSettings(t, cookie, csrf, map[string]string{"agentUrl": ""}), publicRefused)
	if f.inst.AgentURL() != "https://tierplan.internal:7480" {
		t.Fatalf("a refused clear changed the agent URL to %q", f.inst.AgentURL())
	}

	// With the free agent URL already stored, the public URL alone saves.
	g, gcookie, gcsrf := ingressHostsFixture(t, hosts)
	if rec := g.patchSettings(t, gcookie, gcsrf, map[string]string{"agentUrl": "https://tierplan.internal:7480"}); rec.Code != http.StatusOK {
		t.Fatalf("seed agent URL: %d %s", rec.Code, rec.Body)
	}
	if rec := g.patchSettings(t, gcookie, gcsrf, map[string]string{"publicUrl": "https://TP.apps.example.org/"}); rec.Code != http.StatusOK {
		t.Fatalf("route host with a stored free agent URL: %d %s", rec.Code, rec.Body)
	}
	if g.inst.PublicURL() != "https://tp.apps.example.org" {
		t.Fatalf("public URL = %q", g.inst.PublicURL())
	}
}
