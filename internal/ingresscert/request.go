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

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// RequestInput is a request for a certificate ahead of any claim: an Admin of environment EnvID asks for the
// hostname's certificate (D-08). CanView says which environments the caller may see, for the sentence of a
// refusal that names the holder of the name; nil names none.
type RequestInput struct {
	EnvID, Hostname, Actor string
	CanView                func(envID string) bool
}

// RequestAhead creates, or reuses, the live certificate row of a hostname for an environment before any stack
// claims the name (D-08). The hostname must pass the environment's request rules (ingress.CheckRequest: under a
// domain of the environment, not reserved, not claimed by another environment) and the second gate before
// anything is written, and a refusal is a *RequestRefusedError with nothing written. The row has requested_at
// and requested_by set: a request counts as a reference for Grace from the request, and a request again
// restarts it, and brings back a row that was stopped for being unreferenced. created is true when a new row was
// made and false when an existing one was reused.
//
// A live row of another environment is never taken from a request-ahead that still holds it (RequestHeld): that
// is refused as ingress_host_taken in words that name no environment, with nothing written. One that nothing
// holds goes through rehome after the second gate, so it is re-pointed to this environment when it can be reused
// and retired and replaced when it cannot, audited as ingress.cert_moved, and certs_version of both
// environments rises in the same write. hook, which may be nil, runs in the write for every successful request.
// Requests are accepted while issuing is paused: nothing is ordered until it resumes. The manager is woken.
func (m *Manager) RequestAhead(ctx context.Context, in RequestInput, hook CertHook) (Cert, bool, error) {
	host := ingress.NormalizeHost(in.Hostname)
	world, err := m.d.Domains.World(ctx, in.EnvID, in.CanView)
	if err != nil {
		return Cert{}, false, err
	}
	dom, problem, bad := ingress.CheckRequest(host, world)
	if bad {
		return Cert{}, false, &RequestRefusedError{Problem: problem}
	}
	// The second gate: the hostname is strictly under the domain and is a name an order could be made for.
	if !m.secondGate(host, dom) {
		return Cert{}, false, &RequestRefusedError{Problem: ingress.Problem{
			Severity: ingress.SeverityError, Code: ingress.CodeHostInvalid,
			Message: "The hostname “" + host + "” cannot carry a certificate.",
		}}
	}

	// The environment of an existing live row is read first, to know whose version the write must raise, and
	// compared again inside the write.
	envs := []string{in.EnvID}
	preEnv := ""
	if pre, found, err := m.d.Store.LiveCert(ctx, host); err != nil {
		return Cert{}, false, err
	} else if found && pre.EnvID != in.EnvID {
		preEnv = pre.EnvID
		envs = append(envs, preEnv)
	}

	now := m.clock.Now()
	ts := toNanos(now)
	var out Cert
	created := false
	err = m.d.Store.write(ctx, envs, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		id := ""
		row, err := q.GetLiveIngressCertByHostname(ctx, host)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			made, err := m.d.Store.newRequestedRow(ctx, q, host, in, dom, now)
			if err != nil {
				return false, err
			}
			id, created = made, true
		case err != nil:
			return false, fmt.Errorf("ingresscert: read the certificate of %q: %w", host, err)
		default:
			live, err := certOf(row)
			if err != nil {
				return false, err
			}
			// A row of an environment outside the set this write bumps changed since it was read; one of this
			// environment that a concurrent request just made is reused below, never refused.
			if live.EnvID != in.EnvID && live.EnvID != preEnv {
				return false, refuseHostChanged()
			}
			if live.EnvID != in.EnvID && RequestHeld(live, now) {
				return false, refuseHeldElsewhere()
			}
			id = live.ID
			if live.EnvID != in.EnvID || live.DomainID != dom.ID {
				if live.Phase == PhaseOrdering {
					return false, refuseHostChanged() // its order is in flight: not while it runs
				}
				moved, err := m.rehome(ctx, tx, q, live, rehomeTarget{EnvID: in.EnvID, Domain: dom}, causeRequest, in.Actor, now)
				if err != nil {
					return false, err
				}
				id = moved.ID
			}
			if err := q.SetIngressCertRequested(ctx, sqlc.SetIngressCertRequestedParams{
				RequestedAt: ts, RequestedBy: in.Actor, UpdatedAt: ts, ID: id,
			}); err != nil {
				return false, fmt.Errorf("ingresscert: record the request for %q: %w", host, err)
			}
		}
		stored, err := q.GetIngressCert(ctx, id)
		if err != nil {
			return false, fmt.Errorf("ingresscert: read back the certificate of %q: %w", host, err)
		}
		if out, err = certOf(stored); err != nil {
			return false, err
		}
		if hook != nil {
			if err := hook(ctx, tx, out); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return Cert{}, false, err
	}
	m.Wake()
	return out, created, nil
}

// newRequestedRow inserts the queued ACME row of a request ahead, under the domain the request resolved to and
// with that domain's issuer, and returns its id. It runs inside the caller's write, so a second request for the
// hostname finds this row and reuses it.
func (s *Store) newRequestedRow(ctx context.Context, q *sqlc.Queries, host string, in RequestInput, dom ingress.Domain, now time.Time) (string, error) {
	uid, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("ingresscert: make a certificate id: %w", err)
	}
	ts := toNanos(now)
	id := uid.String()
	if err := q.InsertIngressCert(ctx, sqlc.InsertIngressCertParams{
		ID: id, Hostname: host, EnvID: in.EnvID, DomainID: dom.ID, Source: SourceACME, Issuer: string(dom.Issuer),
		Phase: PhaseQueued, RequestedAt: ts, RequestedBy: in.Actor, CreatedBy: in.Actor, CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		return "", fmt.Errorf("ingresscert: insert the certificate of %q: %w", host, err)
	}
	return id, nil
}
