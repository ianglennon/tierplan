// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// runJob is the body of one run's hub job. jctx is the job's context (the hub cancels it on Cancel and Close) and
// mgr the context of the round that started the run: either ending ends the order, which stays open to be
// resumed. The slot is freed and the run's outcome reported only after the result is written, and the manager
// is woken so a queued row does not wait for the tick.
func (m *Manager) runJob(jctx, mgr context.Context, id string) {
	ctx, cancel := context.WithCancelCause(jctx)
	defer cancel(nil)
	stop := context.AfterFunc(mgr, func() { cancel(context.Cause(mgr)) })
	defer stop()

	outcome := OutcomeFailed // a panic still frees the slot
	defer func() {
		m.drop(id)
		if m.d.RunEnded != nil {
			m.d.RunEnded(id, outcome)
		}
		m.Wake()
	}()
	outcome = m.runCert(ctx, mgr, id)
}

// runCert orders the certificate of row id, which the scheduler has reserved (phase ordering), and returns how the
// run ended.
func (m *Manager) runCert(ctx, mgr context.Context, id string) string {
	bg := context.WithoutCancel(ctx)
	c, err := m.d.Store.Cert(ctx, id)
	if err != nil {
		if errors.Is(err, ErrCertNotFound) {
			return OutcomeGone
		}
		m.log.Error("a certificate run could not read its certificate", "error", err)
		return m.cut(ctx, mgr, id)
	}
	if !c.RetiredAt.IsZero() || c.Source != SourceACME || c.Phase != PhaseOrdering {
		return OutcomeGone
	}

	// The second gate (D-21). The domain is the one the row was made under, read by its stored id: never a
	// fresh match of the hostname against the suffixes, which could pick another environment's. The issuer
	// and the token come from it and from nowhere else.
	dom, err := m.d.Domains.DomainByID(ctx, c.DomainID)
	if errors.Is(err, ingressstore.ErrDomainNotFound) {
		if serr := m.d.Store.Stop(bg, id, StopDomainRemoved); serr != nil {
			m.log.Error("a certificate run could not stop a certificate whose domain is gone", "host", c.Hostname, "error", serr)
		}
		return OutcomeFailed
	}
	if err != nil {
		m.log.Error("a certificate run could not read the domain of its certificate", "host", c.Hostname, "error", err)
		return m.cut(ctx, mgr, id)
	}
	issuer := string(dom.Issuer)
	client, dir := m.client(issuer)
	if client == nil {
		return m.fail(ctx, mgr, c, errors.New("ingresscert: the domain has an issuer that is neither staging nor production"), "")
	}
	if !ingress.Under(c.Hostname, dom.Suffix) || acme.CheckName(c.Hostname, !dir.Overridden) != nil {
		// A refusal is a failure with backoff, not a permanent stop: fixing the domain lets the row recover with
		// no one acting.
		return m.charge(ctx, c, acme.Failure{Class: acme.ClassIdentifierRejected, Host: c.Hostname})
	}

	// The account: none is made before the terms are acknowledged (D-36).
	if _, _, found, err := m.d.Store.AccountStore().Account(ctx, dir.URL); err != nil {
		return m.fail(ctx, mgr, c, err, "")
	} else if !found {
		st, err := m.d.Store.State(ctx)
		if err != nil {
			return m.fail(ctx, mgr, c, err, "")
		}
		if st.TermsAcceptedAt.IsZero() {
			if err := m.d.Store.SetQueue(bg, id, QueueReason{Code: QueueNoAccount}); err != nil {
				m.log.Error("a certificate run could not record that it waits for the terms", "host", c.Hostname, "error", err)
			}
			return OutcomeQueued
		}
		contact := ""
		if m.d.Contact != nil {
			contact = m.d.Contact(ctx)
		}
		if _, err := client.Register(ctx, contact, true); err != nil && !errors.Is(err, acme.ErrAccountExists) {
			return m.fail(ctx, mgr, c, err, "")
		}
	}

	// D-09: the classic profile when the directory offers it, the CA's default otherwise, never short-lived.
	profile := m.d.Profile
	if profile == "" {
		meta, err := client.Meta(ctx)
		if err != nil {
			return m.fail(ctx, mgr, c, err, "")
		}
		if _, ok := meta.Profiles[ProfileClassic]; ok {
			profile = ProfileClassic
		}
	}

	solver := &acme.DNSSolver{
		Engine: m.engine, Provider: dom.DNSProvider, CredentialRef: dom.ID,
		Tokens: func(ctx context.Context) ([]byte, error) {
			token, err := m.d.Domains.DomainTokenByID(ctx, dom.ID)
			if errors.Is(err, ingressstore.ErrNoToken) {
				// No token is a token the provider will not accept: the same class as a refused one.
				return nil, &dns.Error{Provider: dom.DNSProvider, Op: "open token", Class: dns.ClassTokenInvalid}
			}
			return token, err
		},
		Owner: func(ctx context.Context) (string, error) {
			orderID, found, err := m.d.Store.OpenOrderID(ctx, id)
			if err != nil || !found {
				return "", err
			}
			return orderID, nil
		},
	}
	_, err = acme.NewOrderer(client, m.d.Store.OrderStore(id), solver, m.clock).Run(ctx, acme.OrderRequest{
		Kind: acme.KindIssue, Names: []string{c.Hostname}, Profile: profile,
	}, nil)
	switch {
	case err == nil:
		return OutcomeIssued
	case errors.Is(err, ErrCertGone):
		return OutcomeGone
	}
	return m.fail(ctx, mgr, c, err, profile)
}

// fail ends a run that did not end in a certificate. A run that was cut (the process is stopping, or a person
// cancelled the job) is not the certificate authority's fault, so it charges nothing. Any other failure is
// classified and charged.
func (m *Manager) fail(ctx, mgr context.Context, c Cert, runErr error, profile string) string {
	if ctx.Err() != nil {
		return m.cut(ctx, mgr, c.ID)
	}
	return m.charge(ctx, c, acme.Classify(runErr, acme.ClassifyInput{ChallengeType: acme.ChallengeDNS01, Profile: profile}))
}

// charge puts a failure in the row's backoff ledger and writes the ingress.cert_failed audit row, and the row waits
// queued for its next attempt. The audit row carries the class and never the CA's text.
func (m *Manager) charge(ctx context.Context, c Cert, f acme.Failure) string {
	bg := context.WithoutCancel(ctx)
	now := m.clock.Now()
	l := acme.RecordFailure(LedgerOf(c), now, f.RetryAfter, 0, m.d.Jitter())
	if err := m.d.Store.RecordFailure(bg, c, f.Class, l, now); err != nil {
		m.log.Error("a certificate run could not record its failure", "host", c.Hostname, "error", err)
	}
	m.log.Warn("a certificate run failed", "host", c.Hostname, "class", f.Class)
	return OutcomeFailed
}

// cut ends a run whose context ended. While the process is stopping (the manager's context or the hub's) the row
// is left ordering: the next Start queues it again and its open order resumes. When a person cancelled the job on
// a live manager, the reservation goes back at once.
func (m *Manager) cut(ctx, mgr context.Context, id string) string {
	if mgr.Err() != nil || jobs.Closing(ctx) {
		return OutcomeQueued
	}
	if err := m.d.Store.ReleaseOrdering(context.WithoutCancel(ctx), id); err != nil {
		m.log.Error("a certificate run could not release its reservation", "error", err)
	}
	return OutcomeQueued
}
