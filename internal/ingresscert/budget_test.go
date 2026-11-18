// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// budgetRig is the manager rig with environment env-1 holding apps.example.com (production) and shop.other.org
// (staging) instead of the rig's example.com, which is removed.
func budgetRig(t *testing.T) *mgrRig {
	t.Helper()
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	g.addDomain("env-1", "apps.example.com", ingress.IssuerProduction)
	g.addDomain("env-1", "shop.other.org", ingress.IssuerStaging)
	return g
}

func (g *mgrRig) addDomain(envID, suffix string, issuer ingress.Issuer) {
	g.t.Helper()
	if _, err := g.dom.CreateDomain(context.Background(), ingressstore.CreateDomainInput{
		EnvID: envID, Suffix: suffix, Issuer: issuer, Token: []byte(rigToken), Actor: "user:1",
	}, nil); err != nil {
		g.t.Fatalf("CreateDomain(%s, %s): %v", envID, suffix, err)
	}
}

// budget is Budget of the environment, failing the test on an error.
func (g *mgrRig) budget(m *Manager, envID string) []BudgetRow {
	g.t.Helper()
	rows, err := m.Budget(context.Background(), envID)
	if err != nil {
		g.t.Fatalf("Budget(%s): %v", envID, err)
	}
	return rows
}

// budgetRowOf is the budget row of a registered domain and issuer, failing the test when there is none.
func budgetRowOf(t *testing.T, rows []BudgetRow, rd, issuer string) BudgetRow {
	t.Helper()
	for _, r := range rows {
		if r.RegisteredDomain == rd && r.Issuer == issuer {
			return r
		}
	}
	t.Fatalf("no budget row for %s on %s in %+v", rd, issuer, rows)
	return BudgetRow{}
}

// D-18: the budget is reported per registered domain and issuer, computed install-wide, with the environment's own
// suffixes under it, and flagged as shared when another domain in any environment, or TierPlan's own host, sits
// under the same registered domain on the same issuer.
func TestBudgetPerRegisteredDomain(t *testing.T) {
	t.Run("used, cap, limit, headroom and no slot while there is headroom", func(t *testing.T) {
		g := budgetRig(t)
		g.seedNew(12)
		rows := g.budget(g.newManager(), "env-1")
		if len(rows) != 2 || rows[0].RegisteredDomain != "example.com" || rows[1].RegisteredDomain != "other.org" {
			t.Fatalf("rows = %+v, want example.com then other.org", rows)
		}
		prod := rows[0]
		if prod.Issuer != IssuerProduction || prod.Used != 12 || prod.Cap != NewCertsCap || prod.Limit != NewCertsLimit || prod.Headroom != 28 ||
			!prod.NextSlot.IsZero() || prod.Shared || !slices.Equal(prod.Domains, []string{"apps.example.com"}) {
			t.Errorf("example.com row = %+v, want production 12 of 40 (limit 50), headroom 28, no slot, not shared, [apps.example.com]", prod)
		}
		stg := rows[1]
		if stg.Issuer != IssuerStaging || stg.Used != 0 || stg.Cap != StagingNewCertsCap || stg.Limit != StagingNewCertsLimit ||
			stg.Headroom != StagingNewCertsCap || stg.Shared || !slices.Equal(stg.Domains, []string{"shop.other.org"}) {
			t.Errorf("other.org row = %+v, want staging 0 of %d, headroom %d, not shared, [shop.other.org]", stg, StagingNewCertsCap, StagingNewCertsCap)
		}
	})

	t.Run("another environment's domain under the same registered domain makes it shared", func(t *testing.T) {
		g := budgetRig(t)
		g.seedNew(12)
		seedEnv(t, g.st, "env-2", "two")
		g.addDomain("env-2", "example.com", ingress.IssuerProduction)
		m := g.newManager()
		a := budgetRowOf(t, g.budget(m, "env-1"), "example.com", IssuerProduction)
		if !a.Shared || !slices.Equal(a.Domains, []string{"apps.example.com"}) || a.Used != 12 {
			t.Errorf("A's row = %+v, want shared, only its own suffix, 12 used", a)
		}
		// The count is install-wide: the other environment sees the same figure and its own suffix only.
		b := budgetRowOf(t, g.budget(m, "env-2"), "example.com", IssuerProduction)
		if !b.Shared || !slices.Equal(b.Domains, []string{"example.com"}) || b.Used != 12 {
			t.Errorf("B's row = %+v, want shared, only its own suffix, 12 used", b)
		}
		// The other.org budget is nobody else's.
		if o := budgetRowOf(t, g.budget(m, "env-1"), "other.org", IssuerStaging); o.Shared {
			t.Errorf("other.org is shared: %+v", o)
		}
	})

	t.Run("a domain on the other issuer does not share the budget", func(t *testing.T) {
		g := budgetRig(t)
		seedEnv(t, g.st, "env-2", "two")
		g.addDomain("env-2", "www.example.com", ingress.IssuerStaging)
		if a := budgetRowOf(t, g.budget(g.newManager(), "env-1"), "example.com", IssuerProduction); a.Shared {
			t.Errorf("a staging domain makes the production budget shared: %+v", a)
		}
	})

	t.Run("TierPlan's own host shares it and its certificate is used", func(t *testing.T) {
		g := budgetRig(t)
		g.seedNew(12)
		g.ownRun("own-ok", "issue", "succeeded", `["tierplan.example.com"]`, g.clock.Now().Add(-24*time.Hour))
		a := budgetRowOf(t, g.budget(g.newManager(), "env-1"), "example.com", IssuerProduction)
		if !a.Shared || a.Used != 13 || a.Headroom != 27 {
			t.Errorf("row = %+v, want shared with 13 used (12 and the own certificate) and headroom 27", a)
		}
	})

	t.Run("an own host under another registered domain shares nothing", func(t *testing.T) {
		g := budgetRig(t)
		g.ownRun("own-ok", "issue", "succeeded", `["tierplan.elsewhere.net"]`, g.clock.Now().Add(-24*time.Hour))
		rows := g.budget(g.newManager(), "env-1")
		if a := budgetRowOf(t, rows, "example.com", IssuerProduction); a.Shared || a.Used != 0 {
			t.Errorf("row = %+v, want unshared and unused", a)
		}
	})

	t.Run("40 used leaves no headroom and the next slot is the oldest plus a week", func(t *testing.T) {
		g := budgetRig(t)
		oldest := g.seedNew(NewCertsCap)
		a := budgetRowOf(t, g.budget(g.newManager(), "env-1"), "example.com", IssuerProduction)
		if a.Used != 40 || a.Headroom != 0 || !a.NextSlot.Equal(oldest.Add(Window)) {
			t.Errorf("row = %+v, want 40 used, headroom 0 and next slot %v", a, oldest.Add(Window))
		}
	})

	t.Run("a renewal is not counted and an issuance a week old has left", func(t *testing.T) {
		g := budgetRig(t)
		now := g.clock.Now()
		g.seedIssuance(IssuerProduction, "example.com", "r.example.com", "renewal", now.Add(-time.Hour))
		g.seedIssuance(IssuerProduction, "example.com", "w.example.com", "new", now.Add(-Window))
		g.seedIssuance(IssuerProduction, "example.com", "n.example.com", "new", now.Add(-Window+time.Second))
		if a := budgetRowOf(t, g.budget(g.newManager(), "env-1"), "example.com", IssuerProduction); a.Used != 1 {
			t.Errorf("used = %d, want 1: only the issuance a second inside the window", a.Used)
		}
	})

	t.Run("an environment with no domains has no rows", func(t *testing.T) {
		g := budgetRig(t)
		seedEnv(t, g.st, "env-2", "two")
		for _, env := range []string{"env-2", "no-such-env"} {
			rows := g.budget(g.newManager(), env)
			if rows == nil || len(rows) != 0 {
				t.Errorf("Budget(%s) = %#v, want an empty list", env, rows)
			}
		}
	})
}
