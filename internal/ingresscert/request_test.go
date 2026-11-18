// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// request asks for host in environment envID as an Admin would and returns the result.
func (g *mgrRig) request(m *Manager, envID, host string, hook CertHook) (Cert, bool, error) {
	g.t.Helper()
	return m.RequestAhead(context.Background(), RequestInput{EnvID: envID, Hostname: host, Actor: "user:1"}, hook)
}

// D-08: a request ahead makes one queued row with requested_at and requested_by, in the write that runs the hook,
// and the next round issues it. The request is a reference for the grace measured from it; requesting again
// restarts it and brings a stopped row back with no new order.
func TestRequestAheadCreatesOneRowAndCountsAsAReference(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager(zeroRand)
	requestedAt := g.clock.Now()

	hooked := 0
	hook := func(_ context.Context, _ *sql.Tx, c Cert) error {
		hooked++
		if c.Hostname != "web.example.com" {
			t.Errorf("the hook saw %q", c.Hostname)
		}
		return nil
	}
	row, created, err := g.request(m, "env-1", "Web.Example.com", hook)
	if err != nil || !created {
		t.Fatalf("RequestAhead = created %v, %v", created, err)
	}
	if row.Hostname != "web.example.com" || row.Phase != PhaseQueued || row.EnvID != "env-1" || row.DomainID != g.domain.ID ||
		row.Issuer != IssuerProduction || !row.RequestedAt.Equal(requestedAt) || row.RequestedBy != "user:1" {
		t.Errorf("row = %+v, want a queued production row of env-1 requested now by user:1", row)
	}
	if hooked != 1 {
		t.Errorf("the hook ran %d times, want 1", hooked)
	}

	// No claim anywhere: the next round issues it, and the request is its reference.
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued || end.certID != row.ID {
		t.Fatalf("the run ended %+v, want %s issued", end, row.ID)
	}
	g.clock.Advance(29 * oneDay)
	m.round(ctx)
	if got := g.live("web.example.com"); got.Phase != PhaseIdle || got.StoppedReason != "" {
		t.Fatalf("row = phase %q stopped %q after 29 days, want it still idle: the request holds it", got.Phase, got.StoppedReason)
	}
	g.clock.Advance(oneDay + time.Minute)
	m.round(ctx)
	if got := g.live("web.example.com"); got.Phase != PhaseStopped || got.StoppedReason != StoppedUnreferenced {
		t.Fatalf("row = phase %q stopped %q after the grace, want stopped unreferenced", got.Phase, got.StoppedReason)
	}

	again, created, err := g.request(m, "env-1", "web.example.com", hook)
	if err != nil || created {
		t.Fatalf("a second RequestAhead = created %v, %v, want the row reused", created, err)
	}
	if again.ID != row.ID || !again.RequestedAt.Equal(g.clock.Now()) || again.Phase != PhaseIdle || again.StoppedReason != "" || !again.UnreferencedSince.IsZero() {
		t.Errorf("row = id %s requested_at %v phase %q stopped %q, want the same row, requested again now and idle", again.ID, again.RequestedAt, again.Phase, again.StoppedReason)
	}
	if hooked != 2 {
		t.Errorf("the hook ran %d times, want 2", hooked)
	}
	m.round(ctx)
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d newOrder requests, want 1: a request again orders nothing", n)
	}
	if got := g.live("web.example.com"); got.Phase != PhaseIdle {
		t.Errorf("phase = %q after a round, want idle", got.Phase)
	}
}

// D-08, CRT-03: a request is refused, with nothing written, for a name another environment's stack claims, a
// reserved name and a name under no domain of the environment.
func TestRequestAheadRefusals(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.secondEnv(ingress.IssuerProduction, rigToken)
	m := g.newManager()
	g.claimIn("env-2", "stack-2", "taken.example.com")
	if _, _, err := g.dom.Reserve(ctx, ingressstore.ReserveInput{EnvID: "env-1", DomainID: g.domain.ID, Hostname: "held.example.com", Actor: "user:1"}, nil); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	aBefore, _ := versions(t, g.st, "env-1")
	bBefore, _ := versions(t, g.st, "env-2")

	for _, tc := range []struct{ name, host, code string }{
		{"claimed by a stack of another environment", "taken.example.com", ingress.CodeHostTaken},
		{"reserved", "held.example.com", ingress.CodeHostReserved},
		{"under no domain of the environment", "web.other.org", ingress.CodeDomainNotAllowed},
		{"not a hostname", "not a host", ingress.CodeHostInvalid},
	} {
		row, created, err := g.request(m, "env-1", tc.host, nil)
		if refusalCode(err) != tc.code || created || row.ID != "" {
			t.Errorf("%s: RequestAhead = %q, created %v, %v; want %s", tc.name, row.ID, created, err, tc.code)
		}
		if _, found, err := g.s.LiveCert(ctx, tc.host); err != nil || found {
			t.Errorf("%s: a row was written (%v, %v)", tc.name, found, err)
		}
	}
	if aAfter, _ := versions(t, g.st, "env-1"); aAfter != aBefore {
		t.Errorf("env-1 certs_version %d -> %d, want a refusal to move nothing", aBefore, aAfter)
	}
	if bAfter, _ := versions(t, g.st, "env-2"); bAfter != bBefore {
		t.Errorf("env-2 certs_version %d -> %d, want a refusal to move nothing", bBefore, bAfter)
	}
}

// D-08, D-04: a live certificate another environment released (and did not request ahead) is re-pointed to the
// requesting environment after the second gate, with no order, audited, and both versions raised.
func TestRequestAheadReusesAnUnreferencedCertificate(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	d2 := g.secondEnv(ingress.IssuerProduction, rigToken)
	m, first := g.issueOne("app.example.com")
	g.claim() // environment 1 releases the hostname
	m.round(ctx)
	aBefore, _ := versions(t, g.st, "env-1")
	bBefore, _ := versions(t, g.st, "env-2")

	row, created, err := g.request(m, "env-2", "app.example.com", nil)
	if err != nil || created {
		t.Fatalf("RequestAhead = created %v, %v, want the certificate reused", created, err)
	}
	if row.ID != first.ID || row.Serial != first.Serial || row.EnvID != "env-2" || row.DomainID != d2.ID || !row.RequestedAt.Equal(g.clock.Now()) || row.RequestedBy != "user:1" {
		t.Errorf("row = id %s serial %s env %s domain %s requested_at %v, want the same certificate in env-2 under %s requested now", row.ID, row.Serial, row.EnvID, row.DomainID, row.RequestedAt, d2.ID)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d newOrder requests, want 1", n)
	}
	if aAfter, _ := versions(t, g.st, "env-1"); aAfter != aBefore+1 {
		t.Errorf("env-1 certs_version %d -> %d, want one rise", aBefore, aAfter)
	}
	if bAfter, _ := versions(t, g.st, "env-2"); bAfter != bBefore+1 {
		t.Errorf("env-2 certs_version %d -> %d, want one rise", bBefore, bAfter)
	}
	moved := g.movedRows()
	if len(moved) != 1 || moved[0]["from_env"] != "env-1" || moved[0]["to_env"] != "env-2" || moved[0]["outcome"] != movedRepointed ||
		moved[0]["cause"] != causeRequest || moved[0]["target"] != first.ID {
		t.Errorf("moved rows = %v, want one request re-point from env-1 to env-2", moved)
	}
}

// D-08, D-31: a live row of another environment that environment's request ahead still holds is refused, in words
// that name no environment, with nothing written; once the request has lapsed the same request takes it.
func TestRequestAheadRefusesAnotherEnvironmentsLiveRequest(t *testing.T) {
	g := newMgrRig(t, nil, "")
	d2 := g.secondEnv(ingress.IssuerProduction, rigToken)
	m := g.newManager()
	mine, created, err := g.request(m, "env-1", "web.example.com", nil)
	if err != nil || !created {
		t.Fatalf("RequestAhead in env-1 = created %v, %v", created, err)
	}
	aBefore, _ := versions(t, g.st, "env-1")
	bBefore, _ := versions(t, g.st, "env-2")

	_, created, err = g.request(m, "env-2", "web.example.com", nil)
	if refusalCode(err) != ingress.CodeHostTaken || created {
		t.Fatalf("RequestAhead in env-2 = created %v, %v, want ingress_host_taken", created, err)
	}
	var refused *RequestRefusedError
	if !errors.As(err, &refused) || refused.Problem.Message != refuseHeldElsewhere().Problem.Message {
		t.Errorf("the refusal says %q, want the held-elsewhere sentence", err)
	}
	for _, leak := range []string{"env-1", "env-2", "stack", "user:1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the refusal %q names %q", err, leak)
		}
	}
	kept := g.live("web.example.com")
	if kept.ID != mine.ID || kept.EnvID != "env-1" || kept.DomainID != g.domain.ID || !kept.RequestedAt.Equal(mine.RequestedAt) {
		t.Errorf("row = id %s env %s domain %s requested_at %v, want the other environment's row untouched", kept.ID, kept.EnvID, kept.DomainID, kept.RequestedAt)
	}
	if n := len(g.movedRows()); n != 0 {
		t.Errorf("%d ingress.cert_moved rows, want none for a refusal", n)
	}
	if aAfter, _ := versions(t, g.st, "env-1"); aAfter != aBefore {
		t.Errorf("env-1 certs_version %d -> %d, want it unmoved", aBefore, aAfter)
	}
	if bAfter, _ := versions(t, g.st, "env-2"); bAfter != bBefore {
		t.Errorf("env-2 certs_version %d -> %d, want it unmoved", bBefore, bAfter)
	}

	// The request holds for the grace and no longer.
	g.clock.Advance(Grace)
	if _, _, err := g.request(m, "env-2", "web.example.com", nil); refusalCode(err) != ingress.CodeHostTaken {
		t.Fatalf("RequestAhead after exactly the grace = %v, want it still refused", err)
	}
	g.clock.Advance(time.Minute)
	row, created, err := g.request(m, "env-2", "web.example.com", nil)
	if err != nil || created {
		t.Fatalf("RequestAhead after the grace = created %v, %v, want the lapsed row taken", created, err)
	}
	if row.ID != mine.ID || row.EnvID != "env-2" || row.DomainID != d2.ID || !row.RequestedAt.Equal(g.clock.Now()) {
		t.Errorf("row = id %s env %s domain %s requested_at %v, want the row re-pointed to env-2 and requested now", row.ID, row.EnvID, row.DomainID, row.RequestedAt)
	}
	moved := g.movedRows()
	if len(moved) != 1 || moved[0]["from_env"] != "env-1" || moved[0]["to_env"] != "env-2" || moved[0]["cause"] != causeRequest {
		t.Errorf("moved rows = %v, want one from env-1 to env-2", moved)
	}
	if aAfter, _ := versions(t, g.st, "env-1"); aAfter != aBefore+1 {
		t.Errorf("env-1 certs_version %d -> %d, want one rise", aBefore, aAfter)
	}
	if bAfter, _ := versions(t, g.st, "env-2"); bAfter != bBefore+1 {
		t.Errorf("env-2 certs_version %d -> %d, want one rise", bBefore, bAfter)
	}
}

// CRT-03: two requests for one hostname at the same instant leave one live row; one reports created.
func TestConcurrentRequestsMakeOneRow(t *testing.T) {
	g := newMgrRig(t, nil, "")
	m := g.newManager()

	const n = 8
	type result struct {
		row     Cert
		created bool
		err     error
	}
	results := make([]result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			row, created, err := g.request(m, "env-1", "web.example.com", nil)
			results[i] = result{row, created, err}
		}()
	}
	close(start)
	wg.Wait()

	madeNew := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d: %v", i, r.err)
		}
		if r.created {
			madeNew++
		}
		if r.row.ID != results[0].row.ID {
			t.Errorf("request %d got row %s, want %s", i, r.row.ID, results[0].row.ID)
		}
	}
	if madeNew != 1 {
		t.Errorf("%d requests created the row, want exactly 1", madeNew)
	}
	if rows := g.count(`SELECT COUNT(*) FROM ingress_certs WHERE hostname = 'web.example.com' AND retired_at = 0`); rows != 1 {
		t.Errorf("%d live rows, want 1", rows)
	}
}
