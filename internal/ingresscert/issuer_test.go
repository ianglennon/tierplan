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

// D-03, DOM-05, PROH-20-03: production, then staging, then production again before the first production
// certificate expires. The way back orders a new production certificate; the retired production row is never
// revived, and the staging certificate stops at once.
func TestOtherIssuerCertificatesAreNeverReused(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")

	g.setIssuer(ingress.IssuerStaging)
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the staging run ended %q, want issued", end.outcome)
	}
	staging := g.live("app.example.com")

	if n := g.setIssuer(ingress.IssuerProduction); n != 1 {
		t.Fatalf("the way back was confirmed against %d certificates, want 1 (the staging one)", n)
	}
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the second production run ended %q, want issued", end.outcome)
	}
	again := g.live("app.example.com")
	if again.ID == first.ID || again.ID == staging.ID || again.Issuer != IssuerProduction || again.Serial == first.Serial {
		t.Errorf("live row = id %s issuer %s serial %s, want a new production certificate (first %s serial %s)", again.ID, again.Issuer, again.Serial, first.ID, first.Serial)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 3 {
		t.Errorf("the CA saw %d newOrder requests, want 3: one per issuance, none reused", n)
	}
	old, ok := g.retiredRow(first.ID)
	if !ok || old.RetiredReason != RetiredIssuerChanged || old.RetiredAt.IsZero() {
		t.Errorf("first row = %+v (found %v), want it still retired as %s", old, ok, RetiredIssuerChanged)
	}
	// The new production certificate is ready, so the old one is deleted as replaced by the next round.
	m.round(ctx)
	if _, ok := g.retiredRow(first.ID); ok {
		t.Error("the retired production row is still there although its replacement is ready")
	}
	if _, ok := g.retiredRow(staging.ID); ok {
		t.Error("the staging row survived the change back to production")
	}
}

// D-24: an uploaded certificate under the domain keeps its source, its row and its readiness across a change of
// issuer and the change back, and nothing is ordered for it. The preview lists it apart and does not count it.
func TestUploadsIgnoreIssuerChanges(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	m := g.newManager()
	ca := newTestCA(t)
	chain, key := ca.ecLeaf(t, "up.example.com")
	world, err := g.dom.World(ctx, "env-1", nil)
	if err != nil {
		t.Fatalf("World: %v", err)
	}
	up, _, err := g.s.Upload(ctx, UploadInput{EnvID: "env-1", Hostname: "up.example.com", Actor: "user:1", ChainPEM: chain, KeyPEM: key, World: world}, ca.pool, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	g.claim("up.example.com")
	m.round(ctx)
	before := g.live("up.example.com")

	p, err := g.dom.PreviewIssuerChange(ctx, "env-1", g.domain.ID, ingress.IssuerStaging)
	if err != nil || p.Certificates != 0 || !slices.Equal(p.Uploads, []string{"up.example.com"}) {
		t.Fatalf("PreviewIssuerChange = %+v, %v, want no certificates and the upload listed", p, err)
	}
	for _, target := range []ingress.Issuer{ingress.IssuerStaging, ingress.IssuerProduction} {
		g.setIssuer(target)
		m.round(ctx)
		got := g.live("up.example.com")
		if got.ID != up.ID || got.Source != SourceUpload || !got.RetiredAt.IsZero() || got.Ready != before.Ready || got.Phase != before.Phase {
			t.Errorf("after the change to %s the upload = %+v, want it unchanged (ready %v)", target, got, before.Ready)
		}
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d newOrder requests, want none for an upload", n)
	}
}

// D-26: a staging certificate that a change to production retires is deleted by the same round, with the reason
// staging_retired, and a production order replaces it.
func TestStagingCertificatesStopAtOnce(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	g.setIssuer(ingress.IssuerStaging) // nothing is issued yet, so no certificate is affected
	m, staging := g.issueOne("app.example.com")
	if staging.Issuer != IssuerStaging {
		t.Fatalf("the first certificate is from %q, want staging", staging.Issuer)
	}

	if n := g.setIssuer(ingress.IssuerProduction); n != 1 {
		t.Fatalf("the change was confirmed against %d certificates, want 1", n)
	}
	m.round(ctx)
	if _, ok := g.retiredRow(staging.ID); ok {
		t.Error("the retired staging certificate was kept past the round that retired it")
	}
	if !slices.Contains(g.deletions(), [2]string{"app.example.com", DeletedStagingRetired}) {
		t.Errorf("deletions = %v, want ingress.cert_deleted with reason %s", g.deletions(), DeletedStagingRetired)
	}
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the production run ended %q, want issued", end.outcome)
	}
	if live := g.live("app.example.com"); live.Issuer != IssuerProduction || live.ID == staging.ID {
		t.Errorf("live row = id %s issuer %s, want a new production certificate", live.ID, live.Issuer)
	}
}

// DOM-05, D-08: a request ahead for a hostname whose live row is from the other issuer does not reuse it. The
// next round retires the row and orders one from the domain's issuer, which carries the request.
func TestRequestAheadDoesNotReuseTheOtherIssuer(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	g.claim() // released: only the request refers to the hostname
	m.round(ctx)
	g.setIssuer(ingress.IssuerStaging)

	asked, created, err := g.request(m, "env-1", "app.example.com", nil)
	if err != nil || created || asked.ID != first.ID {
		t.Fatalf("RequestAhead = %+v, created %v, %v, want the existing row asked for", asked, created, err)
	}
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	old, ok := g.retiredRow(first.ID)
	if !ok || old.RetiredReason != RetiredIssuerChanged {
		t.Errorf("first row = %+v (found %v), want it retired as %s", old, ok, RetiredIssuerChanged)
	}
	fresh := g.live("app.example.com")
	if fresh.ID == first.ID || fresh.Issuer != IssuerStaging || fresh.RequestedBy != "user:1" || fresh.RequestedAt.IsZero() {
		t.Errorf("live row = id %s issuer %s requested %v by %q, want a staging row that carries the request", fresh.ID, fresh.Issuer, fresh.RequestedAt, fresh.RequestedBy)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d newOrder requests, want 2", n)
	}
}
