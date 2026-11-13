// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/audit"
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
// checked the same conditions). env marks the run as the one TIERPLAN_ACME=on starts, which only the audit rows of
// the run show: the stored trigger stays schedule. The guards run in this order, each before anything that could reach the
// CA: the account (unless the run registers one), a run in progress, every preflight refusal (ACME-10),
// for an automatic start the ledger's backoff, the ledger's manual block (three failed runs in an hour),
// and reuse of a stored certificate that already covers the names and is not yet due.
func (m *Manager) issueLocked(ctx context.Context, st State, actor, trigger string, reg *registerSpec, env bool) (RunStart, error) {
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
		register: reg, env: env,
	})
}

// envIssueRound is the scheduler's start-time issue for TIERPLAN_ACME=on (D-07, D-08): with Let's
// Encrypt switched on by the environment it registers the account, if there is none, and orders, as a
// scheduled run under the persisted ledger, in two cases: no certificate is in use, or the Let's Encrypt
// certificate in use does not carry the names wanted now (a new TIERPLAN_PUBLIC_URL, say). With the
// variable unset the second case waits for the admin, as it always has (14 D-05).
//
// Every guard is issueLocked's, on the ledger and run history the database holds: the backoff after a
// failure, the CA's Retry-After and the three failed runs in an hour. There is no timer or counter here
// that a restart could reset, so a process that keeps restarting orders no more often than a quiet one.
// A refusal of the preflight (ACME-10) or a DNS-01 setup with no token starts nothing and charges
// nothing, and is logged once; the round tries again on its own when it is put right. It does nothing
// until the listener is bound, because the CA validates through it, nor while paused after a restore
// (D-09): Resume lifts the pause and the next round orders. It reports whether a run was started.
func (m *Manager) envIssueRound(ctx context.Context, st State) bool {
	if !m.envOn() || !m.ready() || m.d.CertFiles || st.PausedReason != "" {
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
	if st.PausedReason != "" {
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
	if st.Mode == ModeActive && !m.namesChanged(ctx, st) {
		return false
	}
	// DNS-01 with no token anywhere could only fail at the provider, and the failure would be charged
	// to the ledger. It waits for a token instead, and the status shows that none is saved.
	if m.challenge(st) == ChallengeDNS01 {
		provider := m.provider(st)
		saved, err := m.tokenSaved(ctx, provider)
		if err != nil {
			m.noteEnvIssue("the scheduler could not look for the DNS API token", err)
			return false
		}
		if !saved {
			m.noteEnvIssue("TIERPLAN_ACME is on with DNS-01 validation but no DNS API token is set for "+provider+
				", so no certificate is ordered until one is", nil)
			return false
		}
	}
	var reg *registerSpec
	if _, ok, err := m.d.Client.Account(ctx); err != nil {
		m.noteEnvIssue("the scheduler could not read the ACME account", err)
		return false
	} else if !ok {
		reg = &registerSpec{email: m.d.Env.Email, tos: m.d.Env.AcceptTOS}
	}
	rs, err := m.issueLocked(ctx, st, "", TriggerSchedule, reg, true)
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
	switch {
	case rs.Reused:
	case st.Mode == ModeActive:
		m.log.Info("TIERPLAN_ACME is on and the Let's Encrypt certificate does not carry the wanted names, so one for the new names is being ordered")
	default:
		m.log.Info("TIERPLAN_ACME is on and no Let's Encrypt certificate is in use, so one is being ordered")
	}
	return true
}

// namesChanged reports whether the Let's Encrypt certificate in use lacks the names wanted now. A
// preflight that refuses counts as changed, so the round reaches issueLocked and says why nothing is
// ordered. No stored certificate is not a change: that case is the plain start-time issue's.
func (m *Manager) namesChanged(ctx context.Context, st State) bool {
	leaf, found, err := m.storedLeaf(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the stored Let's Encrypt certificate", "error", err)
		return false
	}
	if !found {
		return false
	}
	ns, refusals := m.preflight(st)
	return len(refusals) > 0 || !SameNames(leaf.DNSNames, ns.Names)
}

// registerAccount registers the account a start-time issue needs, inside the run, as its first step. An
// account that appeared meanwhile is as good as one registered here and writes no row. One this registers is
// audited as system:acme with trigger env: TierPlan did it, because the environment asked for Let's Encrypt (D-28).
// The values the variables fix are not audited as settings (D-14); the account they lead to is.
func (m *Manager) registerAccount(ctx context.Context, steps *jobSteps, reg registerSpec) error {
	steps.Running(StepAccount, "Registering an account with the certificate authority.")
	acct, err := m.d.Client.Register(ctx, reg.email, reg.tos)
	if err != nil && !errors.Is(err, ErrAccountExists) {
		steps.Failed(StepAccount, err)
		return err
	}
	if err == nil {
		d := map[string]string{"terms_agreed": "true", "trigger": triggerEnv}
		if contact := strings.TrimPrefix(acct.Contact, "mailto:"); contact != "" {
			d["contact"] = contact
		}
		if host := DirectoryHost(m.d.Client.Directory().URL); host != "" {
			d["directory"] = host
		}
		// The account exists whatever happens to the run, so the row is written even when the run was cancelled.
		m.record(context.WithoutCancel(ctx), audit.ActionHTTPSAccountRegistered, audit.ResultSuccess, "", d)
	}
	steps.Done(StepAccount)
	return nil
}

// DirectoryHost is the host of a CA directory URL, the form audit rows name the CA in; the whole URL when it does
// not parse.
func DirectoryHost(dirURL string) string {
	u, err := url.Parse(dirURL)
	if err != nil || u.Host == "" {
		return dirURL
	}
	return u.Host
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
