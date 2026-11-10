// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"slices"
	"time"
)

// The reasons Let's Encrypt is paused, as Status.PausedReason reports them.
const (
	// PausedCertFiles is the pause certificate files cause (D-13): derived, never stored.
	PausedCertFiles = "cert_files"
)

// Status is everything the Let's Encrypt page shows, read from what is stored and never from the CA.
type Status struct {
	DirectoryURL        string
	DirectoryOverridden bool
	// Mode is ModeOff or ModeActive.
	Mode string
	// PausedReason is "" when renewals run, PausedRestore after a restore, or PausedCertFiles when
	// certificate files are set and Let's Encrypt state is kept.
	PausedReason string
	CertFiles    bool
	// HasState is whether a stored certificate or an open order exists.
	HasState      bool
	Account       *AccountView
	ChallengeType string
	// Names and Refusals are what the URLs say today; Refusals is empty when an order could start.
	Names    []string
	Refusals []Refusal
	// Certificate is the stored Let's Encrypt certificate, whether or not it is being served.
	Certificate *CertView
	// NamesMismatch is true when the certificate's names are not the names wanted today.
	NamesMismatch bool
	Profile       string
	// RenewAt and RenewBasis are when and why the certificate is next renewed; zero unless Mode is
	// active.
	RenewAt    time.Time
	RenewBasis RenewBasis
	// Fallback is true when Let's Encrypt is in use but its certificate has expired, so the
	// self-signed one is served (D-15); ExpiredAt is when it expired.
	Fallback      bool
	ExpiredAt     time.Time
	LastError     *ErrorView
	Failures      int
	NextAttemptAt time.Time
	// Block is set while a manual attempt is refused.
	Block     *BlockView
	ActiveRun *RunView
	LastRun   *RunView
}

// AccountView is the registered account, without its key.
type AccountView struct {
	RegisteredAt   time.Time
	Contact        string
	TermsOfService string
}

// CertView is the stored certificate.
type CertView struct {
	Names                         []string
	NotBefore, NotAfter, IssuedAt time.Time
	// Renewed is whether the newest issue was a renewal.
	Renewed bool
}

// ErrorView is the last failure: its class, the facts the headline needs, and the raw detail for the
// Details disclosure.
type ErrorView struct {
	Class, Host, Profile, TermsURL, Detail string
	At                                     time.Time
}

// BlockView says until when a manual attempt is refused, and why (BlockToldToWait, BlockFailureGuard).
type BlockView struct {
	Until  time.Time
	Reason string
}

// RunView is one run.
type RunView struct {
	JobID, Kind, Trigger, Outcome string
	StartedAt, FinishedAt         time.Time
}

// Status reads the page's state. It makes no request to the CA.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return Status{}, err
	}
	now := m.clock.Now()
	dir := m.d.Client.Directory()
	ns, refusals := m.preflight(st)

	out := Status{
		DirectoryURL: dir.URL, DirectoryOverridden: dir.Overridden, Mode: st.Mode,
		CertFiles: m.d.CertFiles, ChallengeType: st.ChallengeType, Profile: st.Profile,
		Names: slices.Clone(ns.Names), Refusals: refusals,
		Failures: st.ConsecutiveFailures, NextAttemptAt: st.NextAttemptAt,
	}
	if out.ChallengeType == "" {
		out.ChallengeType = ChallengeHTTP01
	}

	acct, hasAcct, err := m.d.Client.Account(ctx)
	if err != nil {
		return Status{}, err
	}
	if hasAcct {
		out.Account = &AccountView{RegisteredAt: acct.CreatedAt, Contact: acct.Contact, TermsOfService: acct.TermsOfService}
	}

	leaf, hasCert, certErr := m.storedLeaf(ctx)
	if certErr != nil {
		// Something is stored but cannot be read: it counts as state, and there is nothing to show of it.
		m.log.Warn("the stored Let's Encrypt certificate cannot be read for the status", "error", certErr)
		hasCert = true
	}
	hasOrder := false
	if _, _, found, err := m.d.Store.OpenOrder(ctx); err != nil {
		m.log.Warn("the open ACME order cannot be read for the status", "error", err)
		hasOrder = true
	} else {
		hasOrder = found
	}
	out.HasState = hasCert || hasOrder

	switch {
	case m.d.CertFiles && out.HasState:
		out.PausedReason = PausedCertFiles
	case st.PausedReason != "":
		out.PausedReason = st.PausedReason
	}

	if leaf != nil {
		issuedAt := st.LastSuccessAt
		if issuedAt.IsZero() {
			issuedAt = leaf.NotBefore
		}
		out.Certificate = &CertView{
			Names: slices.Clone(leaf.DNSNames), NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
			IssuedAt: issuedAt, Renewed: st.LastSuccessKind == KindRenew,
		}
		out.NamesMismatch = !SameNames(leaf.DNSNames, ns.Names)
		if st.Mode == ModeActive && !now.Before(leaf.NotAfter) {
			out.Fallback, out.ExpiredAt = true, leaf.NotAfter
		}
	}
	if st.Mode == ModeActive {
		out.RenewAt, out.RenewBasis = st.RenewAt, RenewBasis(st.RenewBasis)
	}

	if st.LastErrorClass != "" {
		d := decodeError(st.LastErrorDetail)
		out.LastError = &ErrorView{
			Class: st.LastErrorClass, Host: d.Host, Profile: d.Profile, TermsURL: d.TermsURL,
			Detail: d.Detail, At: st.LastErrorAt,
		}
	}

	failed, err := m.d.Store.FailedRunsSince(ctx, now.Add(-FailureGuardWindow))
	if err != nil {
		return Status{}, err
	}
	if until, reason := ManualBlock(ledgerOf(st), failed, now); reason != "" {
		out.Block = &BlockView{Until: until, Reason: reason}
	}

	m.mu.Lock()
	if ar := m.active; ar != nil {
		out.ActiveRun = &RunView{
			JobID: ar.jobID, Kind: ar.kind, Trigger: ar.trigger, Outcome: OutcomeRunning, StartedAt: ar.startedAt,
		}
	}
	m.mu.Unlock()

	runs, err := m.d.Store.Runs(ctx, 5)
	if err != nil {
		return Status{}, err
	}
	for _, r := range runs {
		if r.Outcome == OutcomeRunning {
			continue
		}
		out.LastRun = &RunView{
			JobID: r.ID, Kind: r.Kind, Trigger: r.Trigger, Outcome: r.Outcome,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		}
		break
	}
	return out, nil
}
