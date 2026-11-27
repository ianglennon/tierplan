// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ErrSystemClaimHeld is a second system claim: TierPlan's own route is already on, in this environment
// or another, so there is nothing to add. The install holds at most one (Phase 28, D-09).
var ErrSystemClaimHeld = errors.New("ingressstore: TierPlan's own route is already on")

// SystemRouteName is the name of the one route a system claim carries.
const SystemRouteName = "tierplan"

// SystemClaim is TierPlan's own route: the hostname that reaches TierPlan itself, the environment whose
// proxy serves it, and the service and port the proxy forwards to over https. It is read back as the
// install's one source of truth for the self route (D-07), never from a header.
type SystemClaim struct {
	EnvID     string
	Hostname  string
	DomainID  string
	Service   string
	Port      int
	ClaimedBy string
	ClaimedAt time.Time
}

// SystemClaimInput is the claim to store.
type SystemClaimInput struct {
	EnvID    string
	Hostname string
	// DomainID is the registered domain of EnvID the hostname sits under.
	DomainID string
	// Service is the name the proxy reaches TierPlan by; Port is the port TierPlan listens on.
	Service string
	Port    int
	Actor   string
	// Now stamps the claim and the version change; the zero time means the store's clock.
	Now time.Time
}

// SystemClaimHook runs inside the transaction that sets or clears the claim, after the change, with the
// claim as it was written or as it was before it was cleared. An error rolls the change back: the audit
// row of the switch is written here, so the claim never changes without it.
type SystemClaimHook func(ctx context.Context, tx *sql.Tx, c SystemClaim) error

// systemClaimOf converts the row.
func systemClaimOf(r sqlc.GetSystemClaimRow) SystemClaim {
	return SystemClaim{
		EnvID: r.EnvID, Hostname: r.Hostname, DomainID: r.DomainID, Service: r.Service, Port: int(r.Port),
		ClaimedBy: r.ClaimedBy, ClaimedAt: fromNanos(r.ClaimedAt),
	}
}

// SystemClaim reads TierPlan's own route install-wide. ok is false when it is not on.
func (s *Store) SystemClaim(ctx context.Context) (SystemClaim, bool, error) {
	row, err := s.st.RQ.GetSystemClaim(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return SystemClaim{}, false, nil
	}
	if err != nil {
		return SystemClaim{}, false, fmt.Errorf("ingressstore: read TierPlan's own route: %w", err)
	}
	return systemClaimOf(row), true, nil
}

// SetSystemClaim stores TierPlan's own route: one ingress_hostnames row (owner_kind 'system', no stack)
// and one ingress_routes row (path "/", the service and port given, scheme https), in one transaction
// through write, so the environment's config_version is raised and the notifier told. The install holds
// at most one: when one exists, in any environment, the result is ErrSystemClaimHeld and nothing is
// written. The partial unique index ingress_hostnames_one_system backs that check at the database.
//
// The domain must be one of EnvID's (ErrDomainNotFound otherwise) and the hostname strictly below it
// (ErrNotUnderDomain); a hostname that is not valid is a *ingress.NameError. A hostname already held by
// a stack or reserved is a *RefusedError with ingress.CodeHostTaken. hook, which may be nil, runs in the
// same transaction after the rows are written.
func (s *Store) SetSystemClaim(ctx context.Context, in SystemClaimInput, hook SystemClaimHook) error {
	host := ingress.NormalizeHost(in.Hostname)
	if err := ingress.ValidateHost(host); err != nil {
		msg := "The hostname is empty."
		if host != "" {
			msg = "“" + host + "” is not a valid hostname: " + err.Error() + "."
		}
		return &ingress.NameError{Code: ingress.CodeHostInvalid, Message: msg}
	}
	if in.Service == "" {
		return errors.New("ingressstore: the system claim names no service")
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("ingressstore: the system claim's port %d is not between 1 and 65535", in.Port)
	}
	now := in.Now
	if now.IsZero() {
		now = s.now()
	}
	now = now.UTC()
	return s.write(ctx, in.EnvID, in.Actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		if _, err := q.GetSystemClaim(ctx); err == nil {
			return false, ErrSystemClaimHeld
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("ingressstore: read TierPlan's own route: %w", err)
		}
		dom, err := domainInEnv(ctx, q, in.EnvID, in.DomainID)
		if err != nil {
			return false, err
		}
		if !ingress.Under(host, dom.Suffix) {
			return false, ErrNotUnderDomain
		}
		reserved, err := q.ListIngressReservedByDomain(ctx, dom.ID)
		if err != nil {
			return false, fmt.Errorf("ingressstore: list the reserved hostnames of domain %q: %w", dom.ID, err)
		}
		for _, r := range reserved {
			if r.Hostname == host {
				return false, &RefusedError{Problems: []ingress.Problem{{
					Severity: ingress.SeverityError, Code: ingress.CodeHostReserved,
					Message: "“" + host + "” is reserved by an Admin and cannot be used. Choose another name.",
				}}}
			}
		}
		ts := toNanos(now)
		if err := q.InsertIngressHostname(ctx, sqlc.InsertIngressHostnameParams{
			Hostname: host, EnvID: in.EnvID, DomainID: dom.ID, OwnerKind: string(ingress.OwnerSystem),
			ClaimedBy: in.Actor, ClaimedAt: ts,
		}); err != nil {
			return false, systemInsertError(ctx, q, host, err)
		}
		if err := insertRoute(ctx, q, in.EnvID, host, ingress.Declaration{
			Name: SystemRouteName, PathPrefix: "/", Service: in.Service, Port: in.Port, Scheme: ingress.SchemeHTTPS,
		}, ts); err != nil {
			return false, err
		}
		if hook != nil {
			claim := SystemClaim{
				EnvID: in.EnvID, Hostname: host, DomainID: dom.ID, Service: in.Service, Port: in.Port,
				ClaimedBy: in.Actor, ClaimedAt: fromNanos(ts),
			}
			if err := hook(ctx, tx, claim); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// systemInsertError names why the hostname row was refused: a second system claim (the partial unique
// index) is ErrSystemClaimHeld, and a hostname someone already holds is the policy's host-taken refusal.
func systemInsertError(ctx context.Context, q *sqlc.Queries, host string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: ingress_hostnames.owner_kind"):
		return ErrSystemClaimHeld
	case strings.Contains(msg, "UNIQUE constraint failed: ingress_hostnames.hostname"):
		row, gerr := q.GetIngressClaim(ctx, host)
		if gerr != nil {
			return &RefusedError{Problems: []ingress.Problem{{
				Severity: ingress.SeverityError, Code: ingress.CodeHostTaken,
				Message: "“" + host + "” is already claimed. A name has one owner across TierPlan. Choose another name.",
			}}}
		}
		info := claimInfoOf(sqlc.ListIngressClaimsInstallRow(row))
		return &RefusedError{Problems: []ingress.Problem{{
			Severity: ingress.SeverityError, Code: ingress.CodeHostTaken,
			Message: ingress.DescribeHolder(info, nil) + " Choose another name.",
		}}}
	}
	return fmt.Errorf("ingressstore: claim %q for TierPlan: %w", host, err)
}

// ClearSystemClaim removes TierPlan's own route from environment envID: the claim and its route go in
// one transaction (the route by cascade), the environment's config_version is raised and hook, which may
// be nil, runs in the same transaction with the claim as it was. An error of the hook rolls the removal
// back. With no system claim in envID it returns ok=false and writes nothing: no version change, no
// notification, no hook.
func (s *Store) ClearSystemClaim(ctx context.Context, envID, actor string, now time.Time, hook SystemClaimHook) (SystemClaim, bool, error) {
	if now.IsZero() {
		now = s.now()
	}
	now = now.UTC()
	var cleared SystemClaim
	var ok bool
	err := s.write(ctx, envID, actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := q.GetSystemClaim(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ingressstore: read TierPlan's own route: %w", err)
		}
		if row.EnvID != envID {
			return false, nil // the install's claim sits in another environment: nothing of this one's to clear
		}
		if _, err := q.DeleteSystemClaim(ctx, envID); err != nil {
			return false, fmt.Errorf("ingressstore: clear TierPlan's own route: %w", err)
		}
		c := systemClaimOf(row)
		if hook != nil {
			if err := hook(ctx, tx, c); err != nil {
				return false, err
			}
		}
		cleared, ok = c, true
		return true, nil
	})
	if err != nil {
		return SystemClaim{}, false, err
	}
	return cleared, ok, nil
}
