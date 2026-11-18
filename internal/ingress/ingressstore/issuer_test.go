// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
)

// seedCert inserts a certificate row straight into the table, so the store test does not depend on the manager.
// retired makes it a retired row.
func seedCert(t *testing.T, st *store.Store, id, host, envID, domainID, source, issuer string, retired bool) {
	t.Helper()
	retiredAt := 0
	if retired {
		retiredAt = 5
	}
	if _, err := st.Write.Exec(
		`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, retired_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1)`,
		id, host, envID, domainID, source, issuer, retiredAt); err != nil {
		t.Fatalf("seed certificate %s: %v", id, err)
	}
}

// issuerOf reads a domain's stored issuer.
func issuerOf(t *testing.T, st *store.Store, domainID string) string {
	t.Helper()
	var issuer string
	if err := st.Read.QueryRow(`SELECT issuer FROM ingress_domains WHERE id = ?`, domainID).Scan(&issuer); err != nil {
		t.Fatalf("read the issuer: %v", err)
	}
	return issuer
}

// D-25: the change of a domain's issuer needs the count of affected certificates echoed back, and the count is
// taken again inside the write.
func TestIssuerChangeNeedsTheCountEchoedBack(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-a", "Alpha")
	seedEnv(t, st, "env-b", "Beta")
	d, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-a", Suffix: "example.com", Issuer: ingress.IssuerProduction, Actor: "admin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedCert(t, st, "c1", "a.example.com", "env-a", d.ID, "acme", "production", false)
	seedCert(t, st, "c2", "b.example.com", "env-a", d.ID, "acme", "production", false)
	seedCert(t, st, "c3", "old.example.com", "env-a", d.ID, "acme", "production", true) // retired: not counted
	seedCert(t, st, "c4", "up2.example.com", "env-a", d.ID, "upload", "", false)
	seedCert(t, st, "c5", "up1.example.com", "env-a", d.ID, "upload", "", false)
	other := registerDomain(t, s, "env-a", "other.test") // a certificate under another domain is not counted
	seedCert(t, st, "c6", "x.other.test", "env-a", other.ID, "acme", "production", false)

	t.Run("the preview counts ACME certificates of the other issuer and lists the uploads apart", func(t *testing.T) {
		p, err := s.PreviewIssuerChange(ctx, "env-a", d.ID, ingress.IssuerStaging)
		if err != nil {
			t.Fatal(err)
		}
		if p.Certificates != 2 || !slices.Equal(p.Uploads, []string{"up1.example.com", "up2.example.com"}) ||
			p.Target != ingress.IssuerStaging || p.Domain.ID != d.ID {
			t.Errorf("preview = %+v, want 2 certificates, the two uploads sorted, target staging", p)
		}
		same, err := s.PreviewIssuerChange(ctx, "env-a", d.ID, ingress.IssuerProduction)
		if err != nil || same.Certificates != 0 {
			t.Errorf("preview to the current issuer = %+v, %v, want 0 certificates", same, err)
		}
	})

	t.Run("a stale count is refused and nothing is written", func(t *testing.T) {
		before := configVersion(t, st, "env-a")
		ran := false
		_, changed, err := s.SetDomainIssuer(ctx, "env-a", d.ID, ingress.IssuerStaging, 1, "admin",
			func(context.Context, *sql.Tx, ingress.Domain, IssuerChangePreview, ingress.Issuer) error {
				ran = true
				return nil
			})
		var mismatch *IssuerCountMismatchError
		if !errors.As(err, &mismatch) || mismatch.Current != 2 || changed {
			t.Fatalf("SetDomainIssuer(staging, 1) = changed %v, %v, want *IssuerCountMismatchError{Current: 2}", changed, err)
		}
		if got := issuerOf(t, st, d.ID); got != "production" || ran || configVersion(t, st, "env-a") != before {
			t.Errorf("issuer %q, hook ran %v, config_version %d -> %d, want nothing written", got, ran, before, configVersion(t, st, "env-a"))
		}
	})

	t.Run("a confirmed change writes, runs the hook with the previous issuer and raises config_version once", func(t *testing.T) {
		before := configVersion(t, st, "env-a")
		var gotPrev ingress.Issuer
		var gotPreview IssuerChangePreview
		var gotDomain ingress.Domain
		out, changed, err := s.SetDomainIssuer(ctx, "env-a", d.ID, ingress.IssuerStaging, 2, "admin",
			func(_ context.Context, tx *sql.Tx, dom ingress.Domain, p IssuerChangePreview, previous ingress.Issuer) error {
				if tx == nil {
					t.Error("the hook got no transaction")
				}
				gotPrev, gotPreview, gotDomain = previous, p, dom
				return nil
			})
		if err != nil || !changed || out.Issuer != ingress.IssuerStaging {
			t.Fatalf("SetDomainIssuer(staging, 2) = %+v, changed %v, %v", out, changed, err)
		}
		if got := issuerOf(t, st, d.ID); got != "staging" {
			t.Errorf("stored issuer = %q, want staging", got)
		}
		if gotPrev != ingress.IssuerProduction || gotPreview.Certificates != 2 || gotDomain.Issuer != ingress.IssuerStaging {
			t.Errorf("hook saw previous %q, %d certificates, domain issuer %q", gotPrev, gotPreview.Certificates, gotDomain.Issuer)
		}
		if after := configVersion(t, st, "env-a"); after != before+1 {
			t.Errorf("config_version %d -> %d, want one rise", before, after)
		}
	})

	t.Run("the same issuer writes nothing and runs no hook", func(t *testing.T) {
		before := configVersion(t, st, "env-a")
		ran := false
		out, changed, err := s.SetDomainIssuer(ctx, "env-a", d.ID, ingress.IssuerStaging, 0, "admin",
			func(context.Context, *sql.Tx, ingress.Domain, IssuerChangePreview, ingress.Issuer) error {
				ran = true
				return nil
			})
		if err != nil || changed || ran || out.ID != d.ID || out.Issuer != ingress.IssuerStaging {
			t.Fatalf("SetDomainIssuer(same) = %+v, changed %v, hook ran %v, %v, want the domain back and nothing done", out, changed, ran, err)
		}
		if after := configVersion(t, st, "env-a"); after != before {
			t.Errorf("config_version %d -> %d, want no change", before, after)
		}
	})

	t.Run("a domain of another environment is not found", func(t *testing.T) {
		if _, err := s.PreviewIssuerChange(ctx, "env-b", d.ID, ingress.IssuerProduction); !errors.Is(err, ErrDomainNotFound) {
			t.Errorf("PreviewIssuerChange from another environment = %v, want ErrDomainNotFound", err)
		}
		if _, _, err := s.SetDomainIssuer(ctx, "env-b", d.ID, ingress.IssuerProduction, 0, "admin", nil); !errors.Is(err, ErrDomainNotFound) {
			t.Errorf("SetDomainIssuer from another environment = %v, want ErrDomainNotFound", err)
		}
		if got := issuerOf(t, st, d.ID); got != "staging" {
			t.Errorf("issuer = %q, want it untouched", got)
		}
	})

	t.Run("a hook error rolls the change back", func(t *testing.T) {
		before := configVersion(t, st, "env-a")
		boom := errors.New("audit failed")
		// Back to production: the two certificates are still production, so the staging domain affects none.
		_, changed, err := s.SetDomainIssuer(ctx, "env-a", d.ID, ingress.IssuerProduction, 0, "admin",
			func(context.Context, *sql.Tx, ingress.Domain, IssuerChangePreview, ingress.Issuer) error { return boom })
		if !errors.Is(err, boom) || changed {
			t.Fatalf("SetDomainIssuer = changed %v, %v, want the hook's error", changed, err)
		}
		if got := issuerOf(t, st, d.ID); got != "staging" || configVersion(t, st, "env-a") != before {
			t.Errorf("issuer %q, config_version %d -> %d, want the change rolled back", got, before, configVersion(t, st, "env-a"))
		}
	})

	t.Run("an issuer that is neither is refused", func(t *testing.T) {
		if _, _, err := s.SetDomainIssuer(ctx, "env-a", d.ID, ingress.Issuer("other"), 0, "admin", nil); err == nil {
			t.Error("SetDomainIssuer accepted an unknown issuer")
		}
	})
}
