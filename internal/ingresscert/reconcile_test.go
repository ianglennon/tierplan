// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// oneDay is a day of the injected clock.
const oneDay = 24 * time.Hour

// secondEnv is a second environment with a stack of its own and the domain example.com registered there with the
// given issuer and token. The token is the rig's, so the fake DNS provider accepts it.
func (g *mgrRig) secondEnv(issuer ingress.Issuer, token string) ingress.Domain {
	g.t.Helper()
	seedEnv(g.t, g.st, "env-2", "two")
	if _, err := g.st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-2', 'env-2', 'web', 'standalone', 'tester', 1)`); err != nil {
		g.t.Fatalf("seed the second stack: %v", err)
	}
	return g.registerDomain("env-2", issuer, token)
}

// registerDomain registers example.com for an environment.
func (g *mgrRig) registerDomain(envID string, issuer ingress.Issuer, token string) ingress.Domain {
	g.t.Helper()
	d, err := g.dom.CreateDomain(context.Background(), ingressstore.CreateDomainInput{
		EnvID: envID, Suffix: "example.com", Issuer: issuer, Token: []byte(token), Actor: "user:1",
	}, nil)
	if err != nil {
		g.t.Fatalf("CreateDomain(%s): %v", envID, err)
	}
	return d
}

// claimIn makes a stack of an environment claim exactly these hostnames.
func (g *mgrRig) claimIn(envID, stackID string, hosts ...string) {
	g.t.Helper()
	var decls []ingress.Declaration
	for i, h := range hosts {
		decls = append(decls, ingress.Declaration{Service: "web", Name: string(rune('a' + i)), Host: h, Label: "tierplan.ingress.host", Port: 80})
	}
	if _, err := g.dom.ReplaceStackRoutes(context.Background(), ingressstore.ReplaceInput{
		EnvID: envID, StackID: stackID, Actor: "user:1", Decls: decls,
	}); err != nil {
		g.t.Fatalf("claim %v in %s: %v", hosts, envID, err)
	}
}

// tokenLog records every DNS token a provider was built with.
type tokenLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *tokenLog) add(token string) {
	l.mu.Lock()
	l.seen = append(l.seen, token)
	l.mu.Unlock()
}

func (l *tokenLog) reset() {
	l.mu.Lock()
	l.seen = nil
	l.mu.Unlock()
}

func (l *tokenLog) has(token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.seen {
		if s == token {
			return true
		}
	}
	return false
}

// recordTokens makes the manager's DNS provider accept any token and remember which ones it was built with, so a
// test can tell whose token wrote a challenge record.
func (g *mgrRig) recordTokens() (func(*Deps), *tokenLog) {
	l := &tokenLog{}
	return func(d *Deps) {
		d.DNS.Providers = map[string]dns.Factory{ingress.ProviderCloudflare: func(token string) dns.Provider {
			l.add(token)
			return g.prov.Factory(token)(token)
		}}
	}, l
}

// movedRows reads the ingress.cert_moved audit rows: the actor, the target and the details of each.
func (g *mgrRig) movedRows() []map[string]string {
	g.t.Helper()
	rows, err := g.st.Read.Query(`SELECT actor_id, target_id, details FROM audit_log WHERE action = ? ORDER BY id`, audit.ActionIngressCertMoved)
	if err != nil {
		g.t.Fatalf("read the moved rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]string
	for rows.Next() {
		var actor, target, details string
		if err := rows.Scan(&actor, &target, &details); err != nil {
			g.t.Fatalf("scan: %v", err)
		}
		d := map[string]string{}
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			g.t.Fatalf("details %q: %v", details, err)
		}
		d["actor"], d["target"] = actor, target
		out = append(out, d)
	}
	return out
}

// deletedReasons reads the reason of every ingress.cert_deleted audit row, by hostname.
func (g *mgrRig) deletedReasons() map[string]string {
	g.t.Helper()
	rows, err := g.st.Read.Query(`SELECT actor_id, details FROM audit_log WHERE action = ?`, audit.ActionIngressCertDeleted)
	if err != nil {
		g.t.Fatalf("read the deleted rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var actor, details string
		if err := rows.Scan(&actor, &details); err != nil {
			g.t.Fatalf("scan: %v", err)
		}
		if actor != audit.SystemIngress.ID {
			g.t.Errorf("an ingress.cert_deleted row was written by %q, want %s", actor, audit.SystemIngress.ID)
		}
		d := map[string]string{}
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			g.t.Fatalf("details %q: %v", details, err)
		}
		out[d["hostname"]] = d["reason"]
	}
	return out
}

// gone reports whether host has no live row.
func (g *mgrRig) gone(host string) bool {
	g.t.Helper()
	_, found, err := g.s.LiveCert(context.Background(), host)
	if err != nil {
		g.t.Fatalf("LiveCert(%s): %v", host, err)
	}
	return !found
}

// retire retires a row the way the lifecycle does, for a test that needs a retired row.
func (g *mgrRig) retire(id, reason string) {
	g.t.Helper()
	ts := toNanos(g.clock.Now())
	err := g.s.write(context.Background(), nil, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		return false, q.RetireIngressCert(context.Background(), sqlc.RetireIngressCertParams{RetiredAt: ts, RetiredReason: reason, UpdatedAt: ts, ID: id})
	})
	if err != nil {
		g.t.Fatalf("retire %s: %v", id, err)
	}
}

// retiredRow reads a row by id and reports whether it still exists.
func (g *mgrRig) retiredRow(id string) (Cert, bool) {
	g.t.Helper()
	c, err := g.s.Cert(context.Background(), id)
	if err != nil {
		return Cert{}, false
	}
	return c, true
}

// CRT-02, D-05, the tracer: a stack's hostname is released and claimed again, and the same certificate row and
// serial come back with no new order. The release only starts the clock of the grace; the claim stops it.
func TestTracerARedeployReusesTheCertificate(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	if !first.RequestedAt.IsZero() || !first.UnreferencedSince.IsZero() {
		t.Fatalf("a freshly issued row has requested_at %v unreferenced_since %v, want neither", first.RequestedAt, first.UnreferencedSince)
	}

	g.claim() // the stack's routes replaced with none: the hostname is released
	m.round(ctx)
	released := g.live("app.example.com")
	if released.UnreferencedSince.IsZero() || !released.UnreferencedSince.Equal(g.clock.Now()) {
		t.Errorf("unreferenced_since = %v after the release, want %v", released.UnreferencedSince, g.clock.Now())
	}
	if released.ID != first.ID || released.Serial != first.Serial || len(released.ChainPEM) == 0 || released.Phase != PhaseIdle {
		t.Errorf("released row = id %s serial %s phase %q, want the same certificate, idle", released.ID, released.Serial, released.Phase)
	}

	g.claim("app.example.com")
	m.round(ctx)
	again := g.live("app.example.com")
	if again.ID != first.ID || again.Serial != first.Serial {
		t.Errorf("re-claimed row = id %s serial %s, want id %s serial %s", again.ID, again.Serial, first.ID, first.Serial)
	}
	if !again.UnreferencedSince.IsZero() || again.Phase != PhaseIdle {
		t.Errorf("re-claimed row has unreferenced_since %v phase %q, want it cleared and idle", again.UnreferencedSince, again.Phase)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d newOrder requests, want 1: a redeploy orders nothing", n)
	}
}

// D-04, D-31: environment A releases a hostname and environment B, which has the same issuer, claims it. The row
// goes to B with no new order, both environments' certs_version rise, and the move is audited. A hostname that
// fails the second gate on the claim's domain is not moved.
func TestMoveRepointsAfterTheSecondGate(t *testing.T) {
	ctx := context.Background()

	t.Run("a move between environments", func(t *testing.T) {
		g := newMgrRig(t, nil, "")
		d2 := g.secondEnv(ingress.IssuerProduction, rigToken)
		m, first := g.issueOne("app.example.com")
		g.claim() // environment A releases the hostname
		m.round(ctx)
		if g.live("app.example.com").UnreferencedSince.IsZero() {
			t.Fatal("the release did not start the grace")
		}
		aBefore, _ := versions(t, g.st, "env-1")
		bBefore, _ := versions(t, g.st, "env-2")

		g.claimIn("env-2", "stack-2", "app.example.com")
		m.round(ctx)

		row := g.live("app.example.com")
		if row.ID != first.ID || row.Serial != first.Serial || row.EnvID != "env-2" || row.DomainID != d2.ID {
			t.Errorf("row = id %s serial %s env %s domain %s, want the same certificate in env-2 under %s", row.ID, row.Serial, row.EnvID, row.DomainID, d2.ID)
		}
		if row.Phase != PhaseIdle || !row.UnreferencedSince.IsZero() || row.StoppedReason != "" {
			t.Errorf("row = phase %q unreferenced_since %v stopped %q, want idle with the grace cleared", row.Phase, row.UnreferencedSince, row.StoppedReason)
		}
		if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
			t.Errorf("the CA saw %d newOrder requests, want 1: a move orders nothing", n)
		}
		if aAfter, _ := versions(t, g.st, "env-1"); aAfter != aBefore+1 {
			t.Errorf("env-1 certs_version %d -> %d, want one rise", aBefore, aAfter)
		}
		if bAfter, _ := versions(t, g.st, "env-2"); bAfter != bBefore+1 {
			t.Errorf("env-2 certs_version %d -> %d, want one rise", bBefore, bAfter)
		}
		moved := g.movedRows()
		if len(moved) != 1 {
			t.Fatalf("%d ingress.cert_moved rows, want 1", len(moved))
		}
		d := moved[0]
		if d["actor"] != audit.SystemIngress.ID || d["target"] != first.ID || d["hostname"] != "app.example.com" ||
			d["from_env"] != "env-1" || d["to_env"] != "env-2" || d["from_domain"] != g.domain.ID || d["to_domain"] != d2.ID ||
			d["outcome"] != movedRepointed || d["cause"] != causeClaim {
			t.Errorf("moved row = %v", d)
		}
		// A round that finds nothing to do writes nothing more.
		m.round(ctx)
		if again, _ := versions(t, g.st, "env-2"); again != bBefore+1 || len(g.movedRows()) != 1 {
			t.Errorf("a second round moved again: env-2 certs_version %d, %d moved rows", again, len(g.movedRows()))
		}
	})

	t.Run("a hostname that fails the second gate on the claim's domain stays where it is", func(t *testing.T) {
		g := newMgrRig(t, nil, "")
		d2 := g.secondEnv(ingress.IssuerProduction, rigToken)
		m, first := g.issueOne("app.example.com")
		g.claim()
		m.round(ctx)
		g.claimIn("env-2", "stack-2", "app.example.com")
		// The claim's domain no longer covers the hostname: only a changed row can reach this, so it is set up in
		// the table.
		if _, err := g.st.Write.Exec(`UPDATE ingress_domains SET suffix = 'other.org' WHERE id = ?`, d2.ID); err != nil {
			t.Fatalf("change the suffix: %v", err)
		}
		bBefore, _ := versions(t, g.st, "env-2")

		m.round(ctx)
		m.round(ctx)

		row := g.live("app.example.com")
		if row.ID != first.ID || row.EnvID != "env-1" || row.DomainID != g.domain.ID {
			t.Errorf("row = id %s env %s domain %s, want it left in env-1 under %s", row.ID, row.EnvID, row.DomainID, g.domain.ID)
		}
		if n := len(g.movedRows()); n != 0 {
			t.Errorf("%d ingress.cert_moved rows, want none", n)
		}
		if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
			t.Errorf("the CA saw %d newOrder requests, want 1", n)
		}
		if bAfter, _ := versions(t, g.st, "env-2"); bAfter != bBefore {
			t.Errorf("env-2 certs_version %d -> %d, want it unmoved", bBefore, bAfter)
		}
	})
}

// PROH-20-07: a certificate is never handed to another environment's claim when the claim's domain has another
// issuer. The row is retired as domain_changed and a queued row under the claim's domain, with that domain's
// issuer, is ordered; the retired row keeps serving where it is until it expires or the new one is ready.
func TestMoveNeverReusesAnotherIssuer(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	d2 := g.secondEnv(ingress.IssuerStaging, rigToken)
	m, first := g.issueOne("app.example.com")
	g.claim()
	m.round(ctx)
	aBefore, _ := versions(t, g.st, "env-1")
	bBefore, _ := versions(t, g.st, "env-2")

	g.claimIn("env-2", "stack-2", "app.example.com")
	m.round(ctx)
	end := g.awaitEnd() // the new row is ordered in the same round

	row := g.live("app.example.com")
	if row.ID == first.ID || row.EnvID != "env-2" || row.DomainID != d2.ID || row.Issuer != IssuerStaging || end.certID != row.ID {
		t.Fatalf("live row = id %s env %s domain %s issuer %s (the run ended %+v), want a new staging row of env-2 under %s", row.ID, row.EnvID, row.DomainID, row.Issuer, end, d2.ID)
	}
	if row.Serial == first.Serial || row.Phase != PhaseIdle {
		t.Errorf("the new row = serial %s phase %q, want its own certificate", row.Serial, row.Phase)
	}
	old, ok := g.retiredRow(first.ID)
	if !ok || old.RetiredAt.IsZero() || old.RetiredReason != RetiredDomainChanged || old.EnvID != "env-1" {
		t.Errorf("old row = %+v (found %v), want it retired as %s and still in env-1", old, ok, RetiredDomainChanged)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d newOrder requests, want 2: the claim's domain needed its own certificate", n)
	}
	if aAfter, _ := versions(t, g.st, "env-1"); aAfter <= aBefore {
		t.Errorf("env-1 certs_version %d -> %d, want a rise", aBefore, aAfter)
	}
	if bAfter, _ := versions(t, g.st, "env-2"); bAfter <= bBefore {
		t.Errorf("env-2 certs_version %d -> %d, want a rise", bBefore, bAfter)
	}
	moved := g.movedRows()
	if len(moved) != 1 || moved[0]["outcome"] != movedReplaced || moved[0]["target"] != row.ID ||
		moved[0]["from_env"] != "env-1" || moved[0]["to_env"] != "env-2" || moved[0]["cause"] != causeClaim {
		t.Errorf("moved rows = %v, want one replaced move to %s", moved, row.ID)
	}
	// The retired production row stays: the live row is a staging certificate, which is never ready.
	m.round(ctx)
	if _, ok := g.retiredRow(first.ID); !ok {
		t.Error("the retired row was deleted while its replacement is not ready")
	}
}

// D-12, D-04, T-20-45: release, delete the domain (allowed with no claim), round, register the same suffix again
// (a new domain id and another token), claim again, round. The same row is re-pointed to the new domain with the
// stop cleared and no new order, and its next renewal writes the challenge record with the new domain's token.
func TestReclaimAfterDomainReRegistrationGetsACertificate(t *testing.T) {
	ctx := context.Background()
	const newToken = "domain-token-second-7d2f"
	g := newRenewRig(t, true)
	mod, tokens := g.recordTokens()
	m, _ := g.issueOne("app.example.com", mod)
	m.round(ctx) // gives the certificate its renewal clock
	first := g.live("app.example.com")

	g.claim() // release
	m.round(ctx)
	if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	m.round(ctx)
	if row := g.live("app.example.com"); row.Phase != PhaseStopped || row.StoppedReason != StoppedDomainRemoved || row.ID != first.ID {
		t.Fatalf("row = phase %q stopped %q after its domain was deleted, want stopped %s", row.Phase, row.StoppedReason, StoppedDomainRemoved)
	}

	d2 := g.registerDomain("env-1", ingress.IssuerProduction, newToken)
	g.claim("app.example.com")
	m.round(ctx)

	row := g.live("app.example.com")
	if row.ID != first.ID || row.DomainID != d2.ID || row.EnvID != "env-1" || row.Serial != first.Serial {
		t.Fatalf("row = id %s domain %s serial %s, want the same row under %s", row.ID, row.DomainID, row.Serial, d2.ID)
	}
	if row.StoppedReason != "" || !row.UnreferencedSince.IsZero() || row.Phase != PhaseIdle {
		t.Errorf("row = stopped %q unreferenced_since %v phase %q, want the stop cleared and idle", row.StoppedReason, row.UnreferencedSince, row.Phase)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d newOrder requests, want 1", n)
	}
	moved := g.movedRows()
	if len(moved) != 1 || moved[0]["from_domain"] != g.domain.ID || moved[0]["to_domain"] != d2.ID ||
		moved[0]["outcome"] != movedRepointed || moved[0]["from_env"] != "env-1" || moved[0]["to_env"] != "env-1" {
		t.Errorf("moved rows = %v, want one re-point from %s to %s", moved, g.domain.ID, d2.ID)
	}

	// The next renewal uses the new domain's token.
	tokens.reset()
	g.clock.Advance(row.RenewAt.Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeRenewed || end.certID != first.ID {
		t.Fatalf("the renewal ended %+v, want %s renewed", end, first.ID)
	}
	if !tokens.has(newToken) || tokens.has(rigToken) {
		t.Errorf("the renewal built its DNS provider with tokens %v, want only the new domain's", tokens.seen)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d newOrder requests, want 2 (the renewal)", n)
	}
}

// D-04, T-20-45: when the re-registered domain cannot reuse the row (another issuer, or a stored key that does
// not open) the old row is retired as domain_changed and a new one is ordered under the new domain with its
// token; the hostname is never without a live row under the new domain after the re-claim round.
func TestReclaimUnderANewDomainReplacesAnUnusableCertificate(t *testing.T) {
	ctx := context.Background()
	const newToken = "domain-token-third-91ac"
	for _, tc := range []struct {
		name      string
		issuer    ingress.Issuer
		breakKey  bool
		wantIssue string
	}{
		{name: "the new domain has the other issuer", issuer: ingress.IssuerStaging, wantIssue: IssuerStaging},
		{name: "the stored key no longer opens", issuer: ingress.IssuerProduction, breakKey: true, wantIssue: IssuerProduction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newMgrRig(t, nil, "")
			mod, tokens := g.recordTokens()
			m, first := g.issueOne("app.example.com", mod)
			g.claim()
			m.round(ctx)
			if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
				t.Fatalf("DeleteDomain: %v", err)
			}
			m.round(ctx)
			d2 := g.registerDomain("env-1", tc.issuer, newToken)
			if tc.breakKey {
				if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET key_sealed = 'not-a-sealed-key' WHERE id = ?`, first.ID); err != nil {
					t.Fatalf("break the key: %v", err)
				}
			}
			tokens.reset()
			g.claim("app.example.com")
			m.round(ctx)

			// Right after the round, before the order ends, the hostname already has its live row under d2.
			row := g.live("app.example.com")
			if row.ID == first.ID || row.DomainID != d2.ID || row.EnvID != "env-1" || row.Issuer != tc.wantIssue {
				t.Fatalf("live row = id %s domain %s issuer %s, want a new %s row under %s", row.ID, row.DomainID, row.Issuer, tc.wantIssue, d2.ID)
			}
			if end := g.awaitEnd(); end.outcome != OutcomeIssued || end.certID != row.ID {
				t.Fatalf("the run ended %+v, want %s issued", end, row.ID)
			}
			if !tokens.has(newToken) || tokens.has(rigToken) {
				t.Errorf("the order built its DNS provider with tokens %v, want only the new domain's", tokens.seen)
			}
			if after := g.live("app.example.com"); after.ID != row.ID || after.Phase != PhaseIdle || len(after.ChainPEM) == 0 {
				t.Errorf("live row after the run = %+v, want the new row issued", after)
			}
			old, ok := g.retiredRow(first.ID)
			if !ok || old.RetiredReason != RetiredDomainChanged || old.RetiredAt.IsZero() {
				t.Errorf("old row = %+v (found %v), want it retired as %s", old, ok, RetiredDomainChanged)
			}
			if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
				t.Errorf("the CA saw %d newOrder requests, want 2", n)
			}
			moved := g.movedRows()
			if len(moved) != 1 || moved[0]["outcome"] != movedReplaced || moved[0]["to_domain"] != d2.ID || moved[0]["from_domain"] != g.domain.ID {
				t.Errorf("moved rows = %v, want one replaced move from %s to %s", moved, g.domain.ID, d2.ID)
			}
		})
	}
}

// D-06: a certificate nobody has referred to for more than Grace is stopped and never renewed, and a claim that
// comes back brings it back to idle.
func TestGraceStopsRenewalUntilReclaimed(t *testing.T) {
	ctx := context.Background()

	t.Run("claimed again before it is due places no order", func(t *testing.T) {
		g := newRenewRig(t, true)
		m, first := g.issueOne("app.example.com")
		g.claim()
		m.round(ctx)
		released := g.clock.Now()

		// The grace is over only after more than 30 days.
		g.clock.Advance(Grace)
		m.round(ctx)
		if row := g.live("app.example.com"); row.Phase != PhaseIdle || row.StoppedReason != "" {
			t.Fatalf("row = phase %q stopped %q after exactly the grace, want it still idle", row.Phase, row.StoppedReason)
		}
		g.clock.Advance(time.Minute)
		m.round(ctx)
		row := g.live("app.example.com")
		if row.Phase != PhaseStopped || row.StoppedReason != StoppedUnreferenced || !row.UnreferencedSince.Equal(released) {
			t.Fatalf("row = phase %q stopped %q unreferenced_since %v after the grace and a minute, want stopped unreferenced since %v", row.Phase, row.StoppedReason, row.UnreferencedSince, released)
		}

		g.claim("app.example.com")
		m.round(ctx)
		back := g.live("app.example.com")
		if back.ID != first.ID || back.Phase != PhaseIdle || back.StoppedReason != "" || !back.UnreferencedSince.IsZero() {
			t.Errorf("row = id %s phase %q stopped %q unreferenced_since %v, want the same row idle and referred to", back.ID, back.Phase, back.StoppedReason, back.UnreferencedSince)
		}
		if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
			t.Errorf("the CA saw %d newOrder requests, want 1", n)
		}
	})

	t.Run("a stopped certificate is not renewed even when its renewal is due", func(t *testing.T) {
		g := newRenewRig(t, true)
		m, first := g.issueOne("app.example.com")
		m.round(ctx) // the renewal clock
		due := g.live("app.example.com").RenewAt
		g.claim()
		m.round(ctx)

		g.clock.Advance(Grace + time.Minute)
		m.round(ctx)
		if row := g.live("app.example.com"); row.Phase != PhaseStopped || row.StoppedReason != StoppedUnreferenced {
			t.Fatalf("row = phase %q stopped %q, want stopped unreferenced", row.Phase, row.StoppedReason)
		}
		g.clock.Advance(due.Sub(g.clock.Now()) + time.Hour)
		m.round(ctx)
		m.round(ctx)
		if row := g.live("app.example.com"); row.Phase != PhaseStopped {
			t.Errorf("phase = %q after the renewal time, want it still stopped", row.Phase)
		}
		if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 || len(g.ended) != 0 {
			t.Errorf("the CA saw %d newOrder requests and %d runs ended, want 1 and 0: a stopped certificate is never renewed", n, len(g.ended))
		}

		// Claimed again before it expires, it is brought back, and its renewal (which is due) is the same row's.
		g.claim("app.example.com")
		m.round(ctx)
		if end := g.awaitEnd(); end.certID != first.ID || end.outcome != OutcomeRenewed {
			t.Fatalf("the run ended %+v, want %s renewed", end, first.ID)
		}
		if back := g.live("app.example.com"); back.ID != first.ID || back.StoppedReason != "" || back.Phase != PhaseIdle {
			t.Errorf("row = id %s stopped %q phase %q, want the same row, idle", back.ID, back.StoppedReason, back.Phase)
		}
	})
}

// D-07: an expired certificate that has been unreferenced past the grace is deleted, with its sealed key and its
// orders, and ingress.cert_deleted says expired_unreferenced.
func TestExpiredUnreferencedCertificateIsDeleted(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	if n := g.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, first.ID); n == 0 {
		t.Fatal("the issue left no order to delete with the row")
	}
	g.claim()
	m.round(ctx)

	// Past the grace but not yet expired: it is stopped, not deleted.
	g.clock.Advance(Grace + time.Minute)
	m.round(ctx)
	if g.gone("app.example.com") {
		t.Fatal("a certificate that has not expired was deleted")
	}

	g.clock.Advance(first.NotAfter.Sub(g.clock.Now()) + time.Minute)
	m.round(ctx)
	if !g.gone("app.example.com") {
		t.Fatal("the expired unreferenced certificate is still there")
	}
	if _, ok := g.retiredRow(first.ID); ok {
		t.Error("the row can still be read by id")
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, first.ID); n != 0 {
		t.Errorf("%d orders left, want none", n)
	}
	if got := g.deletedReasons(); len(got) != 1 || got["app.example.com"] != DeletedExpiredUnreferenced {
		t.Errorf("ingress.cert_deleted rows = %v, want one with reason %s", got, DeletedExpiredUnreferenced)
	}
}

// A claim that is released before the first order is placed makes no order: the row is stopped, and once the
// grace has passed it is deleted as never issued.
func TestReleasedBeforeIssueOrdersNothing(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	m := g.newManager()
	// The terms are not acknowledged, so the first round's run only learns that and the row waits, never ordered.
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeQueued {
		t.Fatalf("the run ended %q, want queued", end.outcome)
	}
	jobs := len(g.ingressJobs())

	g.claim()
	m.round(ctx)
	row := g.live("app.example.com")
	if row.Phase != PhaseStopped || row.StoppedReason != StoppedUnreferenced || row.UnreferencedSince.IsZero() {
		t.Fatalf("row = phase %q stopped %q unreferenced_since %v, want stopped unreferenced at once", row.Phase, row.StoppedReason, row.UnreferencedSince)
	}
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m.round(ctx)
	if g.requests() != 0 || len(g.ingressJobs()) != jobs {
		t.Errorf("the CA saw %d requests and %d jobs ran (was %d), want none placed for a name nobody refers to", g.requests(), len(g.ingressJobs()), jobs)
	}

	g.clock.Advance(Grace)
	m.round(ctx)
	if g.gone("app.example.com") {
		t.Fatal("the row was deleted before the grace was over")
	}
	g.clock.Advance(time.Minute)
	m.round(ctx)
	if !g.gone("app.example.com") {
		t.Fatal("the row that never held a certificate is still there after the grace")
	}
	if got := g.deletedReasons(); len(got) != 1 || got["app.example.com"] != DeletedNeverIssued {
		t.Errorf("ingress.cert_deleted rows = %v, want one with reason %s", got, DeletedNeverIssued)
	}
	if g.requests() != 0 {
		t.Errorf("the CA saw %d requests, want none", g.requests())
	}
}

// D-12, D-28: a certificate whose domain was deleted and that no claim resolves elsewhere is stopped and never
// retried; a row that holds none is deleted with its open order.
func TestDomainRemovedStopsRenewal(t *testing.T) {
	ctx := context.Background()

	t.Run("a certificate", func(t *testing.T) {
		g := newRenewRig(t, true)
		m, first := g.issueOne("app.example.com")
		m.round(ctx)
		due := g.live("app.example.com").RenewAt
		g.claim()
		m.round(ctx)
		if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
			t.Fatalf("DeleteDomain: %v", err)
		}
		m.round(ctx)
		row := g.live("app.example.com")
		if row.ID != first.ID || row.Phase != PhaseStopped || row.StoppedReason != StoppedDomainRemoved || len(row.ChainPEM) == 0 {
			t.Fatalf("row = phase %q stopped %q, want the certificate kept and stopped %s", row.Phase, row.StoppedReason, StoppedDomainRemoved)
		}
		g.clock.Advance(due.Sub(g.clock.Now()) + time.Hour)
		m.round(ctx)
		m.round(ctx)
		if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 || len(g.ended) != 0 {
			t.Errorf("the CA saw %d newOrder requests and %d runs ended, want 1 and 0: a row whose domain is gone is never renewed", n, len(g.ended))
		}
		if row := g.live("app.example.com"); row.StoppedReason != StoppedDomainRemoved {
			t.Errorf("stopped_reason = %q after more rounds, want it kept", row.StoppedReason)
		}
	})

	t.Run("a row that holds no certificate goes with its open order", func(t *testing.T) {
		g := newMgrRig(t, nil, "")
		row := g.pending("lonely.example.com")
		if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET domain_id = ? WHERE id = ?`, g.domain.ID, row.ID); err != nil {
			t.Fatalf("set the domain: %v", err)
		}
		g.insertOrder(row.ID, "lonely.example.com")
		if n := g.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, row.ID); n != 1 {
			t.Fatalf("%d open orders, want 1", n)
		}
		if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
			t.Fatalf("DeleteDomain: %v", err)
		}
		m := g.newManager()
		m.round(ctx)
		if !g.gone("lonely.example.com") {
			t.Fatal("the row whose domain is gone is still there")
		}
		if n := g.count(`SELECT COUNT(*) FROM ingress_cert_orders WHERE cert_id = ?`, row.ID); n != 0 {
			t.Errorf("%d orders left, want none", n)
		}
		if got := g.deletedReasons(); got["lonely.example.com"] != DeletedDomainRemoved {
			t.Errorf("ingress.cert_deleted rows = %v, want reason %s", got, DeletedDomainRemoved)
		}
		if g.requests() != 0 {
			t.Errorf("the CA saw %d requests, want none", g.requests())
		}
	})
}

// D-21, D-22: a retired staging row, and a retired row that holds no certificate, are deleted at once; a retired
// production or uploaded row is deleted when it expires or when the hostname's live row is ready.
func TestRetiredRowsAreDeletedWhenNoLongerNeeded(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	m := g.newManager()

	// A retired staging row and a retired row with no certificate.
	stagingRow, _, err := g.s.CreatePending(ctx, PendingInput{Hostname: "staging.example.com", EnvID: "env-1", DomainID: g.domain.ID, Issuer: IssuerStaging, CreatedBy: "user:1"}, nil)
	if err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	emptyRow := g.pending("empty.example.com")
	g.retire(stagingRow.ID, RetiredDomainChanged)
	g.retire(emptyRow.ID, RetiredDomainChanged)

	// A retired production row that serves until it expires.
	g.claim("old.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	oldRow := g.live("old.example.com")
	g.claim()
	g.retire(oldRow.ID, RetiredDomainChanged)

	// A retired upload that serves until its ACME replacement is ready.
	ca := newTestCA(t)
	chain, key := ca.ecLeaf(t, "up.example.com")
	world, err := g.dom.World(ctx, "env-1", nil)
	if err != nil {
		t.Fatalf("World: %v", err)
	}
	upRow, _, err := g.s.Upload(ctx, UploadInput{EnvID: "env-1", Hostname: "up.example.com", Actor: "user:1", ChainPEM: chain, KeyPEM: key, World: world}, ca.pool, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	g.claim("up.example.com")
	if _, err := g.s.SwitchToACME(ctx, "env-1", upRow.ID, "user:1", IssuerProduction, nil); err != nil {
		t.Fatalf("SwitchToACME: %v", err)
	}

	m.round(ctx)
	if _, ok := g.retiredRow(stagingRow.ID); ok {
		t.Error("the retired staging row was kept")
	}
	if _, ok := g.retiredRow(emptyRow.ID); ok {
		t.Error("the retired row with no certificate was kept")
	}
	if _, ok := g.retiredRow(oldRow.ID); !ok {
		t.Error("the retired production row was deleted before it expired")
	}
	if _, ok := g.retiredRow(upRow.ID); !ok {
		t.Error("the retired upload was deleted while its replacement was not ready")
	}
	// The round started the ACME order of up.example.com.
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q, want issued", end.outcome)
	}
	m.round(ctx)
	if _, ok := g.retiredRow(upRow.ID); ok {
		t.Error("the retired upload was kept after its replacement became ready")
	}
	if _, ok := g.retiredRow(oldRow.ID); !ok {
		t.Error("the retired production row was deleted before it expired")
	}

	g.clock.Advance(oldRow.NotAfter.Sub(g.clock.Now()) + time.Minute)
	m.round(ctx)
	if _, ok := g.retiredRow(oldRow.ID); ok {
		t.Error("the retired production row was kept after it expired")
	}
	want := map[string]string{
		"staging.example.com": DeletedStagingRetired, "empty.example.com": DeletedNeverIssued,
		"up.example.com": DeletedReplaced, "old.example.com": DeletedRetiredExpired,
	}
	got := g.deletedReasons()
	for host, reason := range want {
		if got[host] != reason {
			t.Errorf("deletion of %s: reason %q, want %q (all: %v)", host, got[host], reason, got)
		}
	}
}
