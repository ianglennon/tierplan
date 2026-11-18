// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// drainWake empties the manager's wake channel, so a test can tell a Wake from the signal a run that ended leaves.
func drainWake(m *Manager) {
	for {
		select {
		case <-m.wake:
		default:
			return
		}
	}
}

func (g *mgrRig) pauseReason() string {
	g.t.Helper()
	var reason string
	if err := g.st.Write.QueryRow(`SELECT paused_reason FROM ingress_cert_state WHERE id = 1`).Scan(&reason); err != nil {
		g.t.Fatalf("read the pause: %v", err)
	}
	return reason
}

// D-32: a paused install makes no call however many rounds run; Resume lifts the pause in its own transaction with
// the reason it lifted, wakes the manager, and the next round orders the certificate.
func TestResumeLiftsThePauseAndTheNextRoundOrders(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = ? WHERE id = 1`, PausedRestore); err != nil {
		t.Fatal(err)
	}
	m := g.newManager()

	for i := range 3 {
		m.round(ctx)
		if n := g.requests(); n != 0 {
			t.Fatalf("round %d: the CA saw %d requests while paused, want none", i+1, n)
		}
		if calls := g.prov.Calls(); len(calls) != 0 {
			t.Fatalf("round %d: the DNS provider saw %+v while paused, want no call", i+1, calls)
		}
	}
	if n := len(g.ingressJobs()); n != 0 {
		t.Fatalf("%d jobs while paused, want none", n)
	}

	drainWake(m)
	var ran bool
	var hookReason, seenInTx string
	err := m.Resume(ctx, func(ctx context.Context, tx *sql.Tx, reason string) error {
		ran = true
		hookReason = reason
		// The pause is already cleared inside this transaction, and not yet outside it.
		return tx.QueryRowContext(ctx, `SELECT paused_reason FROM ingress_cert_state WHERE id = 1`).Scan(&seenInTx)
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !ran || hookReason != PausedRestore {
		t.Errorf("hook ran %v with reason %q, want it run with %q", ran, hookReason, PausedRestore)
	}
	if seenInTx != "" {
		t.Errorf("inside the transaction paused_reason = %q, want it already cleared", seenInTx)
	}
	if got := g.pauseReason(); got != "" {
		t.Errorf("paused_reason = %q after Resume, want empty", got)
	}
	if st, err := g.s.State(ctx); err != nil || st.UpdatedAt.IsZero() {
		t.Errorf("state = %+v, %v; want updated_at stamped by the resume", st, err)
	}
	select {
	case <-m.wake:
	default:
		t.Error("Resume did not wake the manager")
	}

	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		t.Fatalf("the run ended %q after the resume, want issued", end.outcome)
	}
	if row := g.live("app.example.com"); row.Phase != PhaseIdle || len(row.ChainPEM) == 0 {
		t.Errorf("row = phase %q with %d chain bytes, want an issued certificate", row.Phase, len(row.ChainPEM))
	}
}

// Resume with no pause is refused and runs no hook; a hook that fails rolls the lift back, so the pause stays and
// nothing is woken.
func TestResumeRefusesWhenNotPausedAndKeepsThePauseOnAHookError(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	m := g.newManager()

	drainWake(m)
	hooked := false
	err := m.Resume(ctx, func(context.Context, *sql.Tx, string) error { hooked = true; return nil })
	if !errors.Is(err, ErrNotPaused) {
		t.Fatalf("Resume with no pause = %v, want ErrNotPaused", err)
	}
	if hooked {
		t.Error("the hook ran although there was no pause to lift")
	}
	select {
	case <-m.wake:
		t.Error("a refused Resume woke the manager")
	default:
	}

	if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = ?, updated_at = 7 WHERE id = 1`, PausedRestore); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("the audit row could not be written")
	err = m.Resume(ctx, func(context.Context, *sql.Tx, string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Resume with a failing hook = %v, want the hook's error", err)
	}
	if got := g.pauseReason(); got != PausedRestore {
		t.Errorf("paused_reason = %q after a failed hook, want the pause kept (%q)", got, PausedRestore)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_cert_state WHERE updated_at = 7`); n != 1 {
		t.Error("updated_at moved although the resume was rolled back")
	}
	select {
	case <-m.wake:
		t.Error("a Resume rolled back by its hook woke the manager")
	default:
	}

	// A nil hook is allowed, and a second resume finds nothing to lift.
	if err := m.Resume(ctx, nil); err != nil {
		t.Fatalf("Resume with no hook: %v", err)
	}
	if err := m.Resume(ctx, nil); !errors.Is(err, ErrNotPaused) {
		t.Errorf("a second Resume = %v, want ErrNotPaused", err)
	}
}
