// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
)

// failTimes runs n failed orders of the one queued certificate, each at the next attempt time the ledger gave.
func (g *mgrRig) failTimes(m *Manager, n int) {
	g.t.Helper()
	for i := range n {
		if i > 0 {
			g.clock.Advance(g.live("app.example.com").NextAttemptAt.Sub(g.clock.Now()))
		}
		g.failedRound(m)
	}
}

// retryRig is a manager with terms acknowledged and one claimed hostname whose orders the DNS provider refuses.
func retryRig(t *testing.T) (*mgrRig, *Manager) {
	t.Helper()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(context.Background(), "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.tokenInvalid()
	return g, g.newManager()
}

// hookLog is a CertHook that remembers the rows it was called with.
type hookLog struct{ calls []Cert }

func (h *hookLog) hook(_ context.Context, _ *sql.Tx, c Cert) error {
	h.calls = append(h.calls, c)
	return nil
}

// D-13: a retry now is refused inside the CA's Retry-After (told_to_wait) and after three failures within an hour
// (failure_guard, until the first of them is an hour old), allowed at that instant, and refused with the right
// error while paused, for another environment's certificate, a retired one, an upload and a stopped one. A refusal
// writes nothing, runs no hook and wakes nobody.
func TestRetryNowRespectsTheManualGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("inside a Retry-After", func(t *testing.T) {
		g := newMgrRig(t, nil, "")
		g.claim("app.example.com")
		if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
			t.Fatal(err)
		}
		g.rateLimit(2*time.Hour, "slow down")
		m := g.newManager()
		g.failedRound(m)
		before := g.live("app.example.com")
		drainWake(m)
		var h hookLog

		err := m.RetryNow(ctx, "env-1", before.ID, h.hook)

		var blocked *BlockedError
		if !errors.As(err, &blocked) || blocked.Reason != acme.BlockToldToWait || !blocked.Until.Equal(before.RetryAfterUntil) {
			t.Fatalf("RetryNow = %v, want a *BlockedError told_to_wait until %v", err, before.RetryAfterUntil)
		}
		if blocked.Error() == "" {
			t.Error("a BlockedError says nothing")
		}
		after := g.live("app.example.com")
		if !after.NextAttemptAt.Equal(before.NextAttemptAt) || !after.UpdatedAt.Equal(before.UpdatedAt) || len(h.calls) != 0 || len(m.wake) != 0 {
			t.Errorf("a refused retry changed the row (next %v -> %v), ran the hook (%d) or woke the manager", before.NextAttemptAt, after.NextAttemptAt, len(h.calls))
		}
	})

	t.Run("three failures within an hour", func(t *testing.T) {
		g, m := retryRig(t)
		g.failTimes(m, 3)
		row := g.live("app.example.com")
		first := row.FailureTimes[0]
		drainWake(m)

		err := m.RetryNow(ctx, "env-1", row.ID, nil)

		var blocked *BlockedError
		if !errors.As(err, &blocked) || blocked.Reason != acme.BlockFailureGuard || !blocked.Until.Equal(first.Add(time.Hour)) {
			t.Fatalf("RetryNow = %v, want a *BlockedError failure_guard until %v", err, first.Add(time.Hour))
		}
		if len(m.wake) != 0 {
			t.Error("a refused retry woke the manager")
		}
		// At the instant the oldest failure is an hour old a retry is allowed.
		g.clock.Advance(first.Add(time.Hour).Sub(g.clock.Now()))
		if err := m.RetryNow(ctx, "env-1", row.ID, nil); err != nil {
			t.Fatalf("RetryNow at the end of the window = %v, want it allowed", err)
		}
	})

	t.Run("refused rows", func(t *testing.T) {
		g, m := retryRig(t)
		g.failTimes(m, 1)
		row := g.live("app.example.com")
		seedEnv(t, g.st, "env-2", "two")

		if err := m.RetryNow(ctx, "env-2", row.ID, nil); !errors.Is(err, ErrCertNotFound) {
			t.Errorf("another environment's certificate: %v, want ErrCertNotFound", err)
		}
		if err := m.RetryNow(ctx, "env-1", "no-such-id", nil); !errors.Is(err, ErrCertNotFound) {
			t.Errorf("an unknown id: %v, want ErrCertNotFound", err)
		}
		if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = ? WHERE id = 1`, PausedRestore); err != nil {
			t.Fatal(err)
		}
		if err := m.RetryNow(ctx, "env-1", row.ID, nil); !errors.Is(err, ErrPaused) {
			t.Errorf("while paused: %v, want ErrPaused", err)
		}
		if err := m.RetryNow(ctx, "env-2", row.ID, nil); !errors.Is(err, ErrPaused) {
			t.Errorf("while paused, another environment's id: %v, want ErrPaused first", err)
		}
		if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = '' WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET phase = 'stopped', stopped_reason = ? WHERE id = ?`, StopDomainRemoved, row.ID); err != nil {
			t.Fatal(err)
		}
		if err := m.RetryNow(ctx, "env-1", row.ID, nil); !errors.Is(err, ErrCertStopped) {
			t.Errorf("a stopped certificate: %v, want ErrCertStopped", err)
		}
		if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET phase = 'queued', stopped_reason = '', source = 'upload', issuer = '' WHERE id = ?`, row.ID); err != nil {
			t.Fatal(err)
		}
		if err := m.RetryNow(ctx, "env-1", row.ID, nil); !errors.Is(err, ErrCertNotFound) {
			t.Errorf("an upload: %v, want ErrCertNotFound", err)
		}
		if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET source = 'acme', issuer = 'production', retired_at = ?, retired_reason = 'domain_changed' WHERE id = ?`, g.clock.Now().UnixNano(), row.ID); err != nil {
			t.Fatal(err)
		}
		if err := m.RetryNow(ctx, "env-1", row.ID, nil); !errors.Is(err, ErrCertNotFound) {
			t.Errorf("a retired certificate: %v, want ErrCertNotFound", err)
		}
	})
}

// D-13: a retry now that the guards allow queues the row for the next round with the next attempt at now, runs the
// hook in the same transaction, wakes the manager, and the next round runs it like any order. A failing hook rolls
// the retry back. An idle certificate is queued for a renewal with its chain kept, and an order already in flight is
// left alone.
func TestRetryNowQueuesAndTheNextRoundRuns(t *testing.T) {
	ctx := context.Background()

	t.Run("a failed order", func(t *testing.T) {
		g, m := retryRig(t)
		g.failTimes(m, 2) // the second failure's next attempt is ten minutes away
		row := g.live("app.example.com")
		if !row.NextAttemptAt.After(g.clock.Now()) {
			t.Fatalf("next_attempt_at = %v, want it ahead of %v", row.NextAttemptAt, g.clock.Now())
		}
		g.ranNothing(m, g.requests(), len(g.prov.Calls())) // it is not due: the round starts nothing
		drainWake(m)
		boom := errors.New("the hook failed")
		if err := m.RetryNow(ctx, "env-1", row.ID, func(context.Context, *sql.Tx, Cert) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("RetryNow with a failing hook = %v, want the hook's error", err)
		}
		if after := g.live("app.example.com"); !after.NextAttemptAt.Equal(row.NextAttemptAt) || len(m.wake) != 0 {
			t.Fatalf("a failing hook left next_attempt_at %v (was %v) or woke the manager", after.NextAttemptAt, row.NextAttemptAt)
		}
		var h hookLog

		if err := m.RetryNow(ctx, "env-1", row.ID, h.hook); err != nil {
			t.Fatalf("RetryNow = %v, want it allowed (two failures, no Retry-After)", err)
		}

		got := g.live("app.example.com")
		if got.Phase != PhaseQueued || !got.NextAttemptAt.Equal(g.clock.Now()) || got.ConsecutiveFailures != 2 {
			t.Errorf("row = phase %q next %v failures %d, want queued now with the failures kept", got.Phase, got.NextAttemptAt, got.ConsecutiveFailures)
		}
		if len(h.calls) != 1 || h.calls[0].ID != row.ID || !h.calls[0].NextAttemptAt.Equal(g.clock.Now()) {
			t.Errorf("the hook ran %+v, want once with the queued row", h.calls)
		}
		if len(m.wake) != 1 {
			t.Error("the manager was not woken")
		}
		if n := len(g.ended); n != 0 {
			t.Errorf("%d runs ended: RetryNow must not start the order, the round does", n)
		}

		g.prov.FailCreate(nil)
		m.round(ctx)
		if end := g.awaitEnd(); end.outcome != OutcomeIssued || end.certID != row.ID {
			t.Fatalf("the next round's run ended %+v, want %s issued", end, row.ID)
		}
	})

	t.Run("an idle certificate", func(t *testing.T) {
		g := newRenewRig(t, true)
		m, first := g.issueOne("app.example.com")
		m.round(ctx) // the renewal clock
		first = g.live("app.example.com")
		if first.Phase != PhaseIdle {
			t.Fatalf("phase = %q, want idle", first.Phase)
		}
		var h hookLog

		if err := m.RetryNow(ctx, "env-1", first.ID, h.hook); err != nil {
			t.Fatalf("RetryNow = %v", err)
		}

		row := g.live("app.example.com")
		if row.Phase != PhaseQueued || !row.NextAttemptAt.Equal(g.clock.Now()) || string(row.ChainPEM) != string(first.ChainPEM) || !row.Ready {
			t.Errorf("row = phase %q next %v, want queued now with the chain kept and still ready", row.Phase, row.NextAttemptAt)
		}
		if len(h.calls) != 1 {
			t.Errorf("the hook ran %d times, want 1", len(h.calls))
		}
		m.round(ctx)
		if end := g.awaitEnd(); end.outcome != OutcomeRenewed || end.certID != first.ID {
			t.Fatalf("the run ended %+v, want %s renewed", end, first.ID)
		}
		if got := g.live("app.example.com"); got.Serial == first.Serial {
			t.Error("the renewal did not store a new certificate")
		}
	})

	t.Run("an order in flight", func(t *testing.T) {
		g, m := retryRig(t)
		g.failTimes(m, 1)
		row := g.live("app.example.com")
		if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET phase = 'ordering' WHERE id = ?`, row.ID); err != nil {
			t.Fatal(err)
		}
		drainWake(m)
		var h hookLog

		if err := m.RetryNow(ctx, "env-1", row.ID, h.hook); err != nil {
			t.Fatalf("RetryNow on an order in flight = %v, want nil", err)
		}
		if after := g.live("app.example.com"); after.Phase != PhaseOrdering || len(h.calls) != 0 || len(m.wake) != 0 {
			t.Errorf("phase %q, hook calls %d: an order in flight is left alone", after.Phase, len(h.calls))
		}
	})
}
