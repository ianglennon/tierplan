// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// errAutomaticWait means the ledger does not allow an automatic attempt yet: the CA told us to wait, or
// the soft backoff after a failure has not run out. The scheduler stays quiet about it.
var errAutomaticWait = errors.New("an automatic certificate attempt is not allowed yet")

// registerSpec is the account a run registers before it orders (TIERPLAN_ACME=on with no account yet).
type registerSpec struct {
	email string
	tos   bool
}

// issueLocked is Issue's guard order, shared with the start-time issue of TIERPLAN_ACME=on so the two
// cannot drift apart. The caller holds opMu and has read st through mutable (or, for the scheduler, has
// checked the same conditions). The guards run in this order, each before anything that could reach the
// CA: the account (unless the run registers one), a run in progress, every preflight refusal (ACME-10),
// for an automatic start the ledger's backoff, the ledger's manual block (three failed runs in an hour),
// and reuse of a stored certificate that already covers the names and is not yet due.
func (m *Manager) issueLocked(ctx context.Context, st State, actor, trigger string, reg *registerSpec) (RunStart, error) {
	if reg == nil {
		if err := m.needAccount(ctx); err != nil {
			return RunStart{}, err
		}
	}
	if m.runActive() {
		return RunStart{}, ErrRunActive
	}
	ns, refusals := m.preflight(st)
	if len(refusals) > 0 {
		return RunStart{}, &RefusedError{Refusals: refusals}
	}
	if trigger == TriggerSchedule && !AutomaticAllowed(ledgerOf(st), m.clock.Now()) {
		return RunStart{}, errAutomaticWait
	}
	if err := m.manualBlock(ctx, st); err != nil {
		return RunStart{}, err
	}
	if rs, reused, err := m.reuse(ctx, st, ns.Names); err != nil || reused {
		return rs, err
	}
	return m.startRun(runParams{
		trigger: trigger, actor: actor, challenge: m.challenge(st),
		req:      OrderRequest{Kind: KindIssue, Names: slices.Clone(ns.Names), Profile: m.profile(st)},
		register: reg,
	})
}

// envIssueRound is the scheduler's start-time issue for TIERPLAN_ACME=on (D-07): with Let's Encrypt
// switched on by the environment and no certificate in use, it registers the account, if there is none,
// and orders, as a scheduled run under the persisted ledger. It does nothing until the listener is
// bound, because the CA validates through it, and an install already on Let's Encrypt is the renewal
// round's business. It reports whether a run was started.
func (m *Manager) envIssueRound(ctx context.Context, st State) bool {
	if !m.envOn() || !m.ready() || m.d.CertFiles || st.PausedReason != "" || st.Mode == ModeActive {
		return false
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.runActive() {
		return false
	}
	// The state is read again under the admin lock: a click may have changed it since the round read it.
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the ACME state", "error", err)
		return false
	}
	if st.PausedReason != "" || st.Mode == ModeActive {
		return false
	}
	// An open order is the resume round's: it continues it when the ledger allows, and a person's
	// cancel is left alone.
	if _, _, found, err := m.d.Store.OpenOrder(ctx); err != nil {
		m.log.Error("the scheduler could not read the open ACME order", "error", err)
		return false
	} else if found {
		return false
	}
	var reg *registerSpec
	if _, ok, err := m.d.Client.Account(ctx); err != nil {
		m.noteEnvIssue("the scheduler could not read the ACME account", err)
		return false
	} else if !ok {
		reg = &registerSpec{email: m.d.Env.Email, tos: m.d.Env.AcceptTOS}
	}
	rs, err := m.issueLocked(ctx, st, "", TriggerSchedule, reg)
	if err != nil {
		var blocked *BlockedError
		var refused *RefusedError
		switch {
		case errors.Is(err, ErrRunActive), errors.Is(err, errAutomaticWait), errors.As(err, &blocked):
			// The ledger or a run in progress says wait; the next tick asks again.
		case errors.As(err, &refused):
			m.noteEnvIssue("TIERPLAN_ACME is on but the certificate cannot be ordered yet: "+err.Error(), nil)
		default:
			m.noteEnvIssue("the scheduler could not start the Let's Encrypt order that TIERPLAN_ACME asks for", err)
		}
		return false
	}
	m.resetEnvNote()
	if !rs.Reused {
		m.log.Info("TIERPLAN_ACME is on and no Let's Encrypt certificate is in use, so one is being ordered")
	}
	return true
}

// registerAccount registers the account a start-time issue needs, inside the run, as its first step. An
// account that appeared meanwhile is as good as one registered here.
func (m *Manager) registerAccount(ctx context.Context, steps *jobSteps, reg registerSpec) error {
	steps.Running(StepAccount, "Registering an account with the certificate authority.")
	if _, err := m.d.Client.Register(ctx, reg.email, reg.tos); err != nil && !errors.Is(err, ErrAccountExists) {
		steps.Failed(StepAccount, err)
		return err
	}
	steps.Done(StepAccount)
	return nil
}

// noteEnvIssue logs a refusal of the start-time issue once for each distinct message, so one that lasts
// does not fill the log at a line a minute.
func (m *Manager) noteEnvIssue(msg string, err error) {
	key := msg
	if err != nil {
		key += ": " + err.Error()
	}
	m.mu.Lock()
	same := m.envNote == key
	m.envNote = key
	m.mu.Unlock()
	if same {
		return
	}
	if err != nil {
		m.log.Error(msg, "error", err)
		return
	}
	m.log.Warn(strings.TrimSpace(msg))
}

func (m *Manager) resetEnvNote() {
	m.mu.Lock()
	m.envNote = ""
	m.mu.Unlock()
}
