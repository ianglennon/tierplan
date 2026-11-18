// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"
	"testing"
)

// Phase 20 review WR-06: when the row a certificate was ordered for is gone by the time it is stored (deleted,
// retired, or replaced by an upload), the certificate is not stored, but the CA issued and counted it, so the
// issuance ledger the governor reads still gets one row for the hostname, its issuer and the kind of order, at the
// time it was stored. A renewal (the row held a chain) counts as a renewal.
func TestStoreIssuedCountsWhatTheCAIssuedForAGoneRow(t *testing.T) {
	const host = "app.example.com"
	for _, tc := range []struct {
		name     string
		renewing bool
		apply    func(t *testing.T, r *issueRig, c Cert)
	}{
		{"deleted", false, func(t *testing.T, r *issueRig, c Cert) {
			if _, err := r.st.Write.Exec(`DELETE FROM ingress_certs WHERE id = ?`, c.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"retired", false, func(t *testing.T, r *issueRig, c Cert) {
			if _, err := r.st.Write.Exec(`UPDATE ingress_certs SET retired_at = ?, retired_reason = 'replaced' WHERE id = ?`,
				r.clock.Now().UnixNano(), c.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"retired while renewing", true, func(t *testing.T, r *issueRig, c Cert) {
			if _, err := r.st.Write.Exec(`UPDATE ingress_certs SET retired_at = ?, retired_reason = 'replaced' WHERE id = ?`,
				r.clock.Now().UnixNano(), c.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced by an upload", false, func(t *testing.T, r *issueRig, c Cert) {
			if _, err := r.st.Write.Exec(`DELETE FROM ingress_certs WHERE id = ?`, c.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := r.st.Write.Exec(
				`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, phase, created_at, updated_at)
				 VALUES ('upload-1', ?, ?, ?, 'upload', '', 'idle', 1, 1)`, host, c.EnvID, c.DomainID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newIssueRig(t)
			c := r.pending(host)
			if tc.renewing {
				_, chain, _ := selfSigned(t, host)
				if _, err := r.st.Write.Exec(`UPDATE ingress_certs SET chain_pem = ? WHERE id = ?`, string(chain), c.ID); err != nil {
					t.Fatal(err)
				}
				var err error
				if c, err = r.s.Cert(ctx, c.ID); err != nil {
					t.Fatal(err)
				}
			}
			order, _ := r.insertOrder(c.ID, host)
			store := r.s.runOrderStore(c) // what a run builds before it orders
			tc.apply(t, r, c)

			key, chain, _ := selfSigned(t, host)
			if err := store.StoreIssued(ctx, order.ID, chain, key, r.clock.Now()); !errors.Is(err, ErrCertGone) {
				t.Fatalf("StoreIssued = %v, want ErrCertGone", err)
			}

			wantKind := "new"
			if tc.renewing {
				wantKind = "renewal"
			}
			var issuer, rd, hostname, kind string
			var issuedAt int64
			var n int
			if err := r.st.Read.QueryRow(`SELECT COUNT(*), COALESCE(MAX(issuer), ''), COALESCE(MAX(registered_domain), ''), COALESCE(MAX(hostname), ''), COALESCE(MAX(kind), ''), COALESCE(MAX(issued_at), 0) FROM ingress_cert_issuances`).
				Scan(&n, &issuer, &rd, &hostname, &kind, &issuedAt); err != nil {
				t.Fatal(err)
			}
			if n != 1 || issuer != IssuerProduction || rd != "example.com" || hostname != host || kind != wantKind || issuedAt != r.clock.Now().UnixNano() {
				t.Errorf("issuances = %d (%s %s %s %s at %d), want one %s production row for %s at now", n, issuer, rd, hostname, kind, issuedAt, wantKind, host)
			}
			if n := r.count(`SELECT COUNT(*) FROM ingress_certs WHERE hostname = ? AND source = 'acme' AND chain_pem != '' AND retired_at = 0`, host); n != 0 {
				t.Errorf("%d live ACME rows hold a chain, want none", n)
			}
		})
	}
}
