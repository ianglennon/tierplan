// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"testing"
	"time"
)

// Phase 20 review WR-05: a running manager prunes the closed orders (with their sealed keys) and the issuance ledger
// every PruneInterval on its own clock, not only at Start. A round before the interval has passed prunes nothing.
func TestARunningManagerPrunesEveryInterval(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	t0 := g.clock.Now()
	if _, err := g.st.Write.Exec(
		`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, created_at, updated_at)
		 VALUES ('cert-1', 'app.example.com', 'env-1', ?, 'acme', 'production', 1, 1)`, g.domain.ID); err != nil {
		t.Fatalf("seed the certificate: %v", err)
	}
	// Closed 23 hours before Start: kept by Start, due for pruning a day later.
	if _, err := g.st.Write.Exec(`INSERT INTO ingress_cert_orders
		(id, cert_id, directory_url, order_url, kind, names, cert_key_sealed, stage, status, created_at, updated_at)
		VALUES ('order-1', 'cert-1', 'https://acme.test/dir', 'https://acme.test/o/1', 'issue', 'app.example.com', 'sealed', 'done', 'closed', ?, ?)`,
		t0.Add(-23*time.Hour).UnixNano(), t0.Add(-23*time.Hour).UnixNano()); err != nil {
		t.Fatalf("seed the closed order: %v", err)
	}
	// Issued an hour inside the retention at Start.
	if _, err := g.st.Write.Exec(`INSERT INTO ingress_cert_issuances (id, issuer, registered_domain, hostname, kind, issued_at)
		VALUES ('iss-1', 'production', 'example.com', 'app.example.com', 'new', ?)`, t0.Add(-IssuanceRetention+time.Hour).UnixNano()); err != nil {
		t.Fatalf("seed the issuance: %v", err)
	}
	orders := func() int { return g.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE id = 'order-1'`) }
	issuances := func() int { return g.count(`SELECT COUNT(*) FROM ingress_cert_issuances WHERE id = 'iss-1'`) }

	m := g.newManager() // Start prunes at t0
	if orders() != 1 || issuances() != 1 {
		t.Fatalf("after Start: order %d issuance %d, want both kept", orders(), issuances())
	}

	g.clk.Advance(2 * time.Hour) // both are past their retention now, but the interval has not passed
	m.round(ctx)
	if orders() != 1 || issuances() != 1 {
		t.Fatalf("a round 2h after Start: order %d issuance %d, want nothing pruned before the interval", orders(), issuances())
	}

	g.clk.Advance(PruneInterval - 2*time.Hour)
	m.round(ctx)
	if orders() != 0 {
		t.Errorf("a round %v after Start kept the closed order and its sealed key", PruneInterval)
	}
	if issuances() != 0 {
		t.Errorf("a round %v after Start kept the issuance past its retention", PruneInterval)
	}
}
