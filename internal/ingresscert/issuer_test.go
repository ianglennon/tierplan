// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// setIssuer changes the issuer of the rig's domain the way an Admin does: preview, then confirm the count shown.
func (g *mgrRig) setIssuer(target ingress.Issuer) int {
	g.t.Helper()
	ctx := context.Background()
	p, err := g.dom.PreviewIssuerChange(ctx, "env-1", g.domain.ID, target)
	if err != nil {
		g.t.Fatalf("PreviewIssuerChange(%s): %v", target, err)
	}
	d, changed, err := g.dom.SetDomainIssuer(ctx, "env-1", g.domain.ID, target, p.Certificates, "user:1", nil)
	if err != nil || !changed || d.Issuer != target {
		g.t.Fatalf("SetDomainIssuer(%s, %d) = %+v, changed %v, %v, want the issuer changed", target, p.Certificates, d, changed, err)
	}
	return p.Certificates
}

// deletions reads every ingress.cert_deleted audit row as (hostname, reason) pairs, in the order they were written.
func (g *mgrRig) deletions() [][2]string {
	g.t.Helper()
	rows, err := g.st.Read.Query(`SELECT details FROM audit_log WHERE action = ? ORDER BY id`, audit.ActionIngressCertDeleted)
	if err != nil {
		g.t.Fatalf("read the deleted rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err != nil {
			g.t.Fatalf("scan: %v", err)
		}
		d := map[string]string{}
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			g.t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, [2]string{d["hostname"], d["reason"]})
	}
	return out
}

// DOM-05, D-26, the tracer: the domain's issuer changes from production to staging. The next round retires the
// production certificate as issuer_changed; it keeps being served until it expires; a new row is queued under
// staging and ordered, reads "DNS proven on staging" and is never served. Once the production certificate has
// expired it is deleted.
func TestTracerIssuerChangeRetiresAndReordersOnStaging(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	if first.Issuer != IssuerProduction {
		t.Fatalf("the first certificate is from %q, want production", first.Issuer)
	}

	p, err := g.dom.PreviewIssuerChange(ctx, "env-1", g.domain.ID, ingress.IssuerStaging)
	if err != nil || p.Certificates != 1 || len(p.Uploads) != 0 {
		t.Fatalf("PreviewIssuerChange(staging) = %+v, %v, want one certificate and no uploads", p, err)
	}
	if n := g.setIssuer(ingress.IssuerStaging); n != 1 {
		t.Fatalf("the change was confirmed against %d certificates, want 1", n)
	}

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the staging run ended %q, want issued", end.outcome)
	}
	old, ok := g.retiredRow(first.ID)
	if !ok || old.RetiredAt.IsZero() || old.RetiredReason != RetiredIssuerChanged {
		t.Fatalf("old row = %+v (found %v), want it retired as %s", old, ok, RetiredIssuerChanged)
	}
	fresh := g.live("app.example.com")
	if fresh.ID == first.ID || fresh.Issuer != IssuerStaging || len(fresh.ChainPEM) == 0 {
		t.Fatalf("live row = id %s issuer %s chain %d bytes, want a new staging certificate", fresh.ID, fresh.Issuer, len(fresh.ChainPEM))
	}
	if state := StateOf(fresh, g.clock.Now()); state != StateDNSProvenOnStaging || StateText(state) != "DNS proven on staging" {
		t.Errorf("the staging certificate reads %q (%q), want DNS proven on staging", state, StateText(state))
	}
	served, err := g.s.Servable(ctx, "env-1")
	if err != nil || len(served) != 1 || served[0].Hostname != "app.example.com" || string(served[0].ChainPEM) != string(first.ChainPEM) {
		t.Fatalf("Servable = %+v, %v, want only the retired production certificate", served, err)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d newOrder requests, want 2", n)
	}

	// The staging certificate does not make the retired production one go early, and the production one goes once
	// it has expired. The hostname is released first so that the expired staging row is not renewed on the way.
	g.claim()
	m.round(ctx)
	if _, ok := g.retiredRow(first.ID); !ok {
		t.Fatal("the retired production certificate was deleted before it expired")
	}
	g.clock.Advance(first.NotAfter.Sub(g.clock.Now()) + time.Minute)
	m.round(ctx)
	if _, ok := g.retiredRow(first.ID); ok {
		t.Error("the retired production certificate was kept after it expired")
	}
	if !slices.Contains(g.deletions(), [2]string{"app.example.com", DeletedRetiredExpired}) {
		t.Errorf("deletions = %v, want ingress.cert_deleted with reason %s", g.deletions(), DeletedRetiredExpired)
	}
}
