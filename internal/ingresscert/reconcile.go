// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The outcomes of a move, as the ingress.cert_moved audit row says them.
const (
	// movedRepointed means the row was kept and handed to the claim's environment and domain.
	movedRepointed = "repointed"
	// movedReplaced means the row could not be reused: it was retired and a new row was queued.
	movedReplaced = "replaced"
)

// What caused a move, as the audit row says it.
const (
	causeClaim   = "claim"
	causeRequest = "request"
)

// reconcile is one idempotent scan over the claims of the install and the certificates, and it is the only place
// the lifecycle of a certificate is decided, because a claim also vanishes by foreign-key cascade, which no Go
// call sees. In this order:
//
//   - every live row is looked at against the claim of its hostname. A claim that resolves to another domain than
//     the row's (a move between environments, another domain, a domain deleted and registered again) re-points the
//     row, or replaces it when it cannot be reused, after the second gate (D-04). A claim that matches the row
//     refers to it. Without a claim the row counts as unreferenced: it is stopped for want of a certificate or
//     after Grace, deleted once expired, and a row whose domain is gone stops or goes (D-06, D-07, D-12).
//   - a claimed hostname that has no live row gets one queued, in the claim's environment and under the claim's
//     domain, with that domain's issuer (D-39). The environment's ingress switch is not read (D-40). A row created
//     here is not a request ahead.
//   - retired rows that serve nothing any more are deleted.
//
// A row whose order is in flight is left alone for the round: the run finishes first.
func (m *Manager) reconcile(ctx context.Context) {
	claims, err := m.d.Domains.InstallClaims(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not list the claims", "error", err)
		return
	}
	live, err := m.d.Store.LiveCerts(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not list the certificates", "error", err)
		return
	}
	now := m.clock.Now()
	claimOf := make(map[string]ingress.ClaimInfo, len(claims))
	for _, claim := range claims {
		claimOf[claim.Hostname] = claim
	}
	doms := domainCache{}
	have := make(map[string]struct{}, len(live))
	for _, c := range live {
		if ctx.Err() != nil {
			return
		}
		have[c.Hostname] = struct{}{}
		if c.Phase == PhaseOrdering {
			continue
		}
		if claim, ok := claimOf[c.Hostname]; ok {
			m.reconcileClaimed(ctx, c, claim, doms, now)
		} else {
			m.reconcileUnclaimed(ctx, c, doms, now)
		}
	}
	for _, claim := range claims {
		if ctx.Err() != nil {
			return
		}
		if _, ok := have[claim.Hostname]; ok {
			continue
		}
		dom, found, ok := m.domain(ctx, doms, claim.DomainID)
		if !ok || !found {
			continue
		}
		if _, _, err := m.d.Store.CreatePending(ctx, PendingInput{
			Hostname: claim.Hostname, EnvID: claim.EnvID, DomainID: claim.DomainID, Issuer: string(dom.Issuer),
			CreatedBy: JobOwner, NotRequested: true,
		}, nil); err != nil {
			m.log.Error("the certificate manager could not queue a certificate", "host", claim.Hostname, "error", err)
		}
	}
	m.reconcileRetired(ctx, now)
}

// domainCache is the domains one scan has read, by id. found is false for a domain that does not exist.
type domainCache map[string]domainEntry

// domainEntry is one domain read of a scan.
type domainEntry struct {
	dom   ingress.Domain
	found bool
}

// domain reads the domain with this id, once per scan. ok is false when the read failed for a reason other than
// the domain not existing, in which case the caller leaves the row as it is.
func (m *Manager) domain(ctx context.Context, cache domainCache, id string) (dom ingress.Domain, found, ok bool) {
	if hit, seen := cache[id]; seen {
		return hit.dom, hit.found, true
	}
	d, err := m.d.Domains.DomainByID(ctx, id)
	switch {
	case err == nil:
		cache[id] = domainEntry{dom: d, found: true}
		return d, true, true
	case errors.Is(err, ingressstore.ErrDomainNotFound):
		cache[id] = domainEntry{}
		return ingress.Domain{}, false, true
	}
	m.log.Error("the certificate manager could not read a domain", "domain", id, "error", err)
	return ingress.Domain{}, false, false
}

// secondGate is the check made again right before a certificate is handed to a hostname or ordered for it
// (D-21): the hostname is strictly under the domain's suffix and is a name an order could be made for, and the
// domain's issuer has a directory.
func (m *Manager) secondGate(host string, dom ingress.Domain) bool {
	client, dir := m.client(string(dom.Issuer))
	if client == nil {
		return false
	}
	return ingress.Under(host, dom.Suffix) && acme.CheckName(host, !dir.Overridden) == nil
}

// reconcileClaimed handles a live row whose hostname a claim holds.
func (m *Manager) reconcileClaimed(ctx context.Context, c Cert, claim ingress.ClaimInfo, doms domainCache, now time.Time) {
	if c.DomainID == claim.DomainID && c.EnvID == claim.EnvID {
		if !c.UnreferencedSince.IsZero() || (c.Phase == PhaseStopped && c.StoppedReason == StoppedUnreferenced) {
			m.must("bring back a certificate that is claimed again", c, m.d.Store.clearUnreferenced(ctx, c, now))
		}
		return
	}
	dom, found, ok := m.domain(ctx, doms, claim.DomainID)
	if !ok || !found {
		return
	}
	// The second gate on the claim's domain. A refusal leaves the row as it is and orders nothing.
	if !m.secondGate(c.Hostname, dom) {
		m.warnOnce("refused:"+c.ID+":"+dom.ID, "a certificate is not moved to the domain of its claim because the hostname fails the second gate", "host", c.Hostname)
		return
	}
	to := rehomeTarget{EnvID: claim.EnvID, Domain: dom}
	err := m.d.Store.write(ctx, []string{c.EnvID, to.EnvID}, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := q.GetLiveIngressCertByHostname(ctx, c.Hostname)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ingresscert: read the certificate of %q: %w", c.Hostname, err)
		}
		if row.ID != c.ID || row.EnvID != c.EnvID || row.DomainID != c.DomainID || row.Phase == PhaseOrdering {
			return false, nil // it changed since the scan began: the next round looks again
		}
		_, err = m.rehome(ctx, tx, q, c, to, causeClaim, JobOwner, now)
		return err == nil, err
	})
	m.must("move a certificate to the domain of its claim", c, err)
}

// reconcileUnclaimed handles a live row whose hostname no claim holds.
func (m *Manager) reconcileUnclaimed(ctx context.Context, c Cert, doms domainCache, now time.Time) {
	hasChain := len(c.ChainPEM) > 0
	_, found, ok := m.domain(ctx, doms, c.DomainID)
	if !ok {
		return
	}
	if !found {
		// D-12: the domain is gone and no claim resolves the hostname elsewhere. A row that holds nothing has
		// nothing to keep; one that holds a certificate stops and is never retried.
		if !hasChain {
			m.must("delete a row whose domain is gone", c, m.d.Store.deleteCert(ctx, c, DeletedDomainRemoved))
			return
		}
		if c.Phase != PhaseStopped || c.StoppedReason != StoppedDomainRemoved {
			if !m.must("stop a certificate whose domain is gone", c, m.d.Store.Stop(ctx, c.ID, StoppedDomainRemoved)) {
				return
			}
			c.Phase, c.StoppedReason = PhaseStopped, StoppedDomainRemoved
		}
	}
	if Referenced(c, false, now) {
		// A request ahead refers to it.
		if !c.UnreferencedSince.IsZero() || (c.Phase == PhaseStopped && c.StoppedReason == StoppedUnreferenced) {
			m.must("bring back a certificate that is requested again", c, m.d.Store.clearUnreferenced(ctx, c, now))
		}
		return
	}
	past := PastGrace(c, false, now)
	expired := hasChain && !c.NotAfter.IsZero() && !now.Before(c.NotAfter)
	switch {
	case !hasChain && past:
		m.must("delete a certificate that was never issued", c, m.d.Store.deleteCert(ctx, c, DeletedNeverIssued))
	case expired && past:
		m.must("delete an expired certificate nobody refers to", c, m.d.Store.deleteCert(ctx, c, DeletedExpiredUnreferenced))
	default:
		// A name nobody refers to is not ordered: a row with no certificate stops at once. One that holds a
		// certificate keeps renewing until the grace is over (D-06).
		stop := (!hasChain || past) && c.Phase != PhaseStopped
		if stop || c.UnreferencedSince.IsZero() {
			m.must("mark a certificate nobody refers to", c, m.d.Store.markUnreferenced(ctx, c, now, stop))
		}
	}
}

// reconcileRetired deletes the retired rows that serve nothing any more: a staging row at once, a row with no
// certificate at once, and a production or uploaded one when it has expired or when the hostname's live row is
// ready (the stored flag).
func (m *Manager) reconcileRetired(ctx context.Context, now time.Time) {
	retired, err := m.d.Store.RetiredCerts(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not list the retired certificates", "error", err)
		return
	}
	if len(retired) == 0 {
		return
	}
	// Read again: a move above may have put a new live row where one was.
	live, err := m.d.Store.LiveCerts(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not list the certificates", "error", err)
		return
	}
	ready := make(map[string]bool, len(live))
	for _, c := range live {
		ready[c.Hostname] = c.Ready
	}
	for _, r := range retired {
		if ctx.Err() != nil {
			return
		}
		reason := ""
		switch {
		case r.Source == SourceACME && r.Issuer == IssuerStaging:
			reason = DeletedStagingRetired
		case len(r.ChainPEM) == 0:
			reason = DeletedNeverIssued
		case !r.NotAfter.IsZero() && !now.Before(r.NotAfter):
			reason = DeletedRetiredExpired
		case ready[r.Hostname]:
			reason = DeletedReplaced
		}
		if reason != "" {
			m.must("delete a retired certificate", r, m.d.Store.deleteCert(ctx, r, reason))
		}
	}
}

// must logs a failed lifecycle write and reports whether it succeeded.
func (m *Manager) must(what string, c Cert, err error) bool {
	if err != nil {
		m.log.Error("the certificate manager could not "+what, "host", c.Hostname, "error", err)
		return false
	}
	return true
}

// warnOnce logs a warning the first time key is seen, so a refusal that a scan meets every round is said once.
func (m *Manager) warnOnce(key, msg string, args ...any) {
	m.mu.Lock()
	_, seen := m.warned[key]
	m.warned[key] = struct{}{}
	m.mu.Unlock()
	if !seen {
		m.log.Warn(msg, args...)
	}
}

// rehomeTarget is where a hostname's certificate goes: the claim's environment and the domain the claim resolves
// to.
type rehomeTarget struct {
	EnvID  string
	Domain ingress.Domain
}

// rehome is the one step that moves the live row c to target, inside the transaction tx of the caller (reconcile
// and RequestAhead share it). When the row can be reused (Reusable: an upload, or an ACME row of the target
// domain's issuer that holds no certificate yet or an unexpired one whose key opens) it is re-pointed to the
// target's environment and domain, any stop and unreferenced_since cleared, and nothing is ordered. Any other row
// is retired with RetiredDomainChanged and a queued row under the target's domain, with that domain's issuer, is
// inserted in the same transaction (retire first, so the one-live-row index allows the insert). Either way the
// move is audited as ingress.cert_moved by system:ingress, so a row never leaves its environment or domain
// without a record. The second gate is the caller's, and so is raising both environments' versions (the write
// that owns tx does). It returns the live row.
func (m *Manager) rehome(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, c Cert, to rehomeTarget, cause, createdBy string, now time.Time) (Cert, error) {
	keyOpens := false
	if c.Source == SourceACME && len(c.ChainPEM) > 0 {
		_, err := m.d.Store.OpenKey(c)
		keyOpens = err == nil
	}
	reuse := Reusable(c, string(to.Domain.Issuer), now, keyOpens)
	ts := toNanos(now)
	id, outcome := c.ID, movedRepointed
	if reuse {
		if err := q.RepointIngressCert(ctx, sqlc.RepointIngressCertParams{EnvID: to.EnvID, DomainID: to.Domain.ID, UpdatedAt: ts, ID: c.ID}); err != nil {
			return Cert{}, fmt.Errorf("ingresscert: re-point the certificate of %q: %w", c.Hostname, err)
		}
	} else {
		uid, err := uuid.NewV7()
		if err != nil {
			return Cert{}, fmt.Errorf("ingresscert: make a certificate id: %w", err)
		}
		if err := q.RetireIngressCert(ctx, sqlc.RetireIngressCertParams{RetiredAt: ts, RetiredReason: RetiredDomainChanged, UpdatedAt: ts, ID: c.ID}); err != nil {
			return Cert{}, fmt.Errorf("ingresscert: retire the certificate of %q: %w", c.Hostname, err)
		}
		id, outcome = uid.String(), movedReplaced
		if err := q.InsertIngressCert(ctx, sqlc.InsertIngressCertParams{
			ID: id, Hostname: c.Hostname, EnvID: to.EnvID, DomainID: to.Domain.ID, Source: SourceACME, Issuer: string(to.Domain.Issuer),
			Phase: PhaseQueued, CreatedBy: createdBy, CreatedAt: ts, UpdatedAt: ts,
		}); err != nil {
			return Cert{}, fmt.Errorf("ingresscert: queue the certificate of %q: %w", c.Hostname, err)
		}
	}
	row, err := q.GetIngressCert(ctx, id)
	if err != nil {
		return Cert{}, fmt.Errorf("ingresscert: read back the certificate of %q: %w", c.Hostname, err)
	}
	out, err := certOf(row)
	if err != nil {
		return Cert{}, err
	}
	if aw := m.d.Store.audit; aw != nil {
		if err := aw.RecordTx(ctx, tx, movedEntry(c, out, to, outcome, cause)); err != nil {
			return Cert{}, fmt.Errorf("ingresscert: record the move of %q: %w", c.Hostname, err)
		}
	}
	return out, nil
}

// movedEntry is the ingress.cert_moved audit row of a move.
func movedEntry(from, to Cert, target rehomeTarget, outcome, cause string) audit.Entry {
	return audit.Entry{
		Actor: audit.SystemIngress, Action: audit.ActionIngressCertMoved, TargetType: auditTargetCertificate,
		TargetID: to.ID, EnvID: target.EnvID, Result: audit.ResultSuccess,
		Details: map[string]string{
			"hostname": from.Hostname, "from_env": from.EnvID, "to_env": target.EnvID,
			"from_domain": from.DomainID, "to_domain": target.Domain.ID, "outcome": outcome, "cause": cause,
		},
	}
}

// The lifecycle writes of the scan. None of them changes what is served, except a deletion of a row that was
// ready, so only that raises certs_version.

// markUnreferenced records since when nothing refers to the row's hostname, when it was not recorded, and stops
// the row for being unreferenced when stop is true.
func (s *Store) markUnreferenced(ctx context.Context, c Cert, now time.Time, stop bool) error {
	return s.write(ctx, nil, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		ts := toNanos(now)
		if c.UnreferencedSince.IsZero() {
			if err := q.SetIngressCertUnreferenced(ctx, sqlc.SetIngressCertUnreferencedParams{UnreferencedSince: ts, UpdatedAt: ts, ID: c.ID}); err != nil {
				return false, fmt.Errorf("ingresscert: mark certificate %q unreferenced: %w", c.ID, err)
			}
		}
		if stop {
			if err := q.StopIngressCert(ctx, sqlc.StopIngressCertParams{StoppedReason: StoppedUnreferenced, UpdatedAt: ts, ID: c.ID}); err != nil {
				return false, fmt.Errorf("ingresscert: stop certificate %q: %w", c.ID, err)
			}
		}
		return false, nil
	})
}

// clearUnreferenced marks the row referred to again, bringing back one that was stopped for being unreferenced.
func (s *Store) clearUnreferenced(ctx context.Context, c Cert, now time.Time) error {
	return s.write(ctx, nil, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.ClearIngressCertUnreferenced(ctx, sqlc.ClearIngressCertUnreferencedParams{UpdatedAt: toNanos(now), ID: c.ID}); err != nil {
			return false, fmt.Errorf("ingresscert: clear the unreferenced time of certificate %q: %w", c.ID, err)
		}
		return false, nil
	})
}

// deleteCert deletes row c, with its sealed key and its orders, and records ingress.cert_deleted as system:ingress
// with the reason, in the same transaction. A row that is gone already writes nothing. Deleting a row that was
// ready raises the environment's certs_version, because what is served changes.
func (s *Store) deleteCert(ctx context.Context, c Cert, reason string) error {
	return s.write(ctx, []string{c.EnvID}, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		n, err := q.DeleteIngressCert(ctx, c.ID)
		if err != nil {
			return false, fmt.Errorf("ingresscert: delete certificate %q: %w", c.ID, err)
		}
		if n == 0 {
			return false, nil
		}
		if s.audit != nil {
			if err := s.audit.RecordTx(ctx, tx, audit.Entry{
				Actor: audit.SystemIngress, Action: audit.ActionIngressCertDeleted, TargetType: auditTargetCertificate,
				TargetID: c.ID, EnvID: c.EnvID, Result: audit.ResultSuccess,
				Details: map[string]string{"hostname": c.Hostname, "reason": reason, "source": c.Source},
			}); err != nil {
				return false, fmt.Errorf("ingresscert: record the deletion of %q: %w", c.Hostname, err)
			}
		}
		return c.Ready, nil
	})
}

// RetiredCerts returns every retired certificate row of the install, ordered by hostname.
func (s *Store) RetiredCerts(ctx context.Context) ([]Cert, error) {
	rows, err := s.st.RQ.ListRetiredIngressCerts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the retired certificates: %w", err)
	}
	return certsOf(rows)
}
