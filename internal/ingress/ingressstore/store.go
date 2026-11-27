// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingressstore keeps the ingress model in SQLite: the domains an environment may serve names
// under, and the hostnames and routes its stacks claim. The rules live in the pure package
// internal/ingress; this package reads the world, runs those rules inside its write transaction and
// writes what they allow.
package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// The refusals of the store. Callers match them with errors.Is.
var (
	// ErrDomainExists is a second registration of a suffix in one environment.
	ErrDomainExists = errors.New("ingressstore: the domain is already registered for this environment")
	// ErrDomainNotFound is a domain id that names no domain of the environment.
	ErrDomainNotFound = errors.New("ingressstore: no such ingress domain")
	// ErrStackNotFound is a stack id that names no stack of the environment.
	ErrStackNotFound = errors.New("ingressstore: no such stack in the environment")
)

// RefusedError is a plan the store would not write: Check, run again on the world as the write
// transaction sees it, found a problem. Nothing was written.
type RefusedError struct {
	Problems []ingress.Problem
}

// Error implements error.
func (e *RefusedError) Error() string {
	if len(e.Problems) == 0 {
		return "ingressstore: the ingress plan was refused"
	}
	return "ingressstore: the ingress plan was refused: " + e.Problems[0].Message
}

// DomainHook runs inside the transaction that registers a domain, after the insert. An error rolls
// the domain back: the audit row of a registration is written here, so a domain never exists
// without it.
type DomainHook func(ctx context.Context, tx *sql.Tx, d ingress.Domain) error

// ClaimDelta is what a replacement of a stack's claims changed, in sorted hostnames. Added are the hostnames the
// stack did not hold before, Kept the ones it held and still holds, Changed the subset of Kept whose domain or
// routes differ from what was stored, and Removed the ones it held and no longer holds.
type ClaimDelta struct {
	Added, Kept, Changed, Removed []string
}

// ClaimHook runs inside the transaction that replaces a stack's claims, after they are written, with the plan that
// was stored and the delta from what the stack held. An error rolls the replacement back.
type ClaimHook func(ctx context.Context, tx *sql.Tx, p ingress.Plan, d ClaimDelta) error

// Deps are what a Store needs.
type Deps struct {
	St    *store.Store
	Vault *vault.Vault
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Notifier is told the environment's id after every committed change; nil means nobody is told.
	Notifier *ingress.Notifier
	// AgentHosts returns the hosts agents and proxies dial: the agent URL's host, else the public URL's, lower-cased and
	// without a port. It is read on every World read, so a settings change applies at once; nil leaves the world's
	// AgentHosts empty (D-10).
	AgentHosts func() []string
}

// Store is the ingress model in the database. DNS tokens are sealed by the vault under the domain's
// own id, so a sealed token copied into another domain's row does not open.
type Store struct {
	st         *store.Store
	v          *vault.Vault
	now        func() time.Time
	notifier   *ingress.Notifier
	agentHosts func() []string
}

// New returns a Store.
func New(d Deps) *Store {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Store{st: d.St, v: d.Vault, now: now, notifier: d.Notifier, agentHosts: d.AgentHosts}
}

// toNanos is the stored form of a time: Unix nanoseconds, 0 for the zero time.
func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNanos is the inverse of toNanos.
func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// write runs fn in one write transaction. When fn reports a change, the environment's
// ingress_env.config_version is raised in the same transaction; a write that changes nothing leaves
// the version where it was and nobody is notified. fn's error rolls everything back. After the commit
// of a change the notifier, if there is one, is told the environment's id; Notify never blocks.
func (s *Store) write(ctx context.Context, envID, actor string, now time.Time, fn func(tx *sql.Tx, q *sqlc.Queries) (changed bool, err error)) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ingressstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	changed, err := fn(tx, q)
	if err != nil {
		return err
	}
	if changed {
		if err := q.BumpIngressConfigVersion(ctx, sqlc.BumpIngressConfigVersionParams{
			EnvID: envID, UpdatedAt: toNanos(now), UpdatedBy: actor,
		}); err != nil {
			return fmt.Errorf("ingressstore: count the ingress change of environment %q: %w", envID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ingressstore: commit: %w", err)
	}
	if changed {
		s.notifier.Notify(envID)
	}
	return nil
}

// domainOf converts a row. The sealed token stays in the row.
func domainOf(row sqlc.IngressDomain) ingress.Domain {
	return ingress.Domain{
		ID: row.ID, EnvID: row.EnvID, Suffix: row.Suffix, Issuer: ingress.Issuer(row.Issuer),
		DNSProvider: row.DnsProvider, HasToken: row.TokenSealed != "", TokenUpdatedAt: fromNanos(row.TokenUpdatedAt),
		CreatedBy: row.CreatedBy, CreatedAt: fromNanos(row.CreatedAt), UpdatedAt: fromNanos(row.UpdatedAt),
	}
}

// CreateDomainInput is a domain to register.
type CreateDomainInput struct {
	EnvID  string
	Suffix string
	Issuer ingress.Issuer
	// Token is the DNS provider token; it may be empty, and is sealed before it reaches a row.
	Token []byte
	Actor string
}

// CreateDomain registers a suffix for an environment. The suffix is normalised and validated here,
// so no caller can store a name the hostname rules would not accept. The same suffix may be
// registered in several environments, each with its own token; it may not be registered twice in
// one (ErrDomainExists). hook, which may be nil, runs in the same transaction after the insert.
func (s *Store) CreateDomain(ctx context.Context, in CreateDomainInput, hook DomainHook) (ingress.Domain, error) {
	suffix := ingress.NormalizeHost(in.Suffix)
	if err := ingress.ValidateSuffix(suffix); err != nil {
		return ingress.Domain{}, err
	}
	if !in.Issuer.Valid() {
		return ingress.Domain{}, fmt.Errorf("ingressstore: the issuer %q is not staging or production", in.Issuer)
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return ingress.Domain{}, fmt.Errorf("ingressstore: new domain id: %w", err)
	}
	id := uid.String()
	sealed := ""
	if len(in.Token) > 0 {
		sealed, err = s.v.Seal(vault.ContextIngressDNSToken+id, in.Token)
		if err != nil {
			return ingress.Domain{}, fmt.Errorf("ingressstore: seal the DNS token: %w", err)
		}
	}
	now := s.now().UTC()
	ts := toNanos(now)
	params := sqlc.InsertIngressDomainParams{
		ID: id, EnvID: in.EnvID, Suffix: suffix, Issuer: string(in.Issuer), DnsProvider: ingress.ProviderCloudflare,
		TokenSealed: sealed, CreatedBy: in.Actor, CreatedAt: ts, UpdatedAt: ts,
	}
	if sealed != "" {
		params.TokenUpdatedAt = ts
	}
	var out ingress.Domain
	err = s.write(ctx, in.EnvID, in.Actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.InsertIngressDomain(ctx, params); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed: ingress_domains.env_id, ingress_domains.suffix") {
				return false, ErrDomainExists
			}
			return false, fmt.Errorf("ingressstore: insert the domain %q: %w", suffix, err)
		}
		row, err := q.GetIngressDomainInEnv(ctx, sqlc.GetIngressDomainInEnvParams{ID: id, EnvID: in.EnvID})
		if err != nil {
			return false, fmt.Errorf("ingressstore: read back the domain %q: %w", suffix, err)
		}
		out = domainOf(row)
		if hook != nil {
			if err := hook(ctx, tx, out); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return ingress.Domain{}, err
	}
	return out, nil
}

// ErrNoToken is a domain that has no DNS token saved.
var ErrNoToken = errors.New("ingressstore: the domain has no DNS token saved")

// ErrTokenEmpty is an attempt to replace a DNS token with nothing.
var ErrTokenEmpty = errors.New("ingressstore: the DNS token is empty")

// ErrNotUnderDomain is a hostname that is the domain's own suffix or is not below it.
var ErrNotUnderDomain = errors.New("ingressstore: the hostname is not below the domain")

// domainInEnv reads a domain by id within one environment. A domain of another environment is
// ErrDomainNotFound, exactly like a missing id, so an id never confirms that another environment
// has it.
func domainInEnv(ctx context.Context, q *sqlc.Queries, envID, domainID string) (sqlc.IngressDomain, error) {
	row, err := q.GetIngressDomainInEnv(ctx, sqlc.GetIngressDomainInEnvParams{ID: domainID, EnvID: envID})
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.IngressDomain{}, ErrDomainNotFound
	}
	if err != nil {
		return sqlc.IngressDomain{}, fmt.Errorf("ingressstore: read the domain %q: %w", domainID, err)
	}
	return row, nil
}

// DomainView is a domain with the hostnames reserved under it, sorted, and the count of its live certificates.
type DomainView struct {
	ingress.Domain
	Reserved []string
	// Certificates is how many live certificates sit under the domain. Deleting the domain leaves them to expire:
	// they stop renewing (D-28).
	Certificates int
}

// Domains lists the domains of an environment by suffix, each with its reserved names.
func (s *Store) Domains(ctx context.Context, envID string) ([]DomainView, error) {
	rows, err := s.st.RQ.ListIngressDomainsByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the domains of environment %q: %w", envID, err)
	}
	out := make([]DomainView, 0, len(rows))
	for _, row := range rows {
		reserved, err := s.st.RQ.ListIngressReservedByDomain(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("ingressstore: list the reserved names of %q: %w", row.Suffix, err)
		}
		certs, err := s.st.RQ.CountLiveIngressCertsByDomain(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("ingressstore: count the certificates of %q: %w", row.Suffix, err)
		}
		v := DomainView{Domain: domainOf(row), Reserved: make([]string, 0, len(reserved)), Certificates: int(certs)}
		for _, r := range reserved {
			v.Reserved = append(v.Reserved, r.Hostname)
		}
		out = append(out, v)
	}
	return out, nil
}

// DomainToken opens the domain's DNS token. It is the only way the token leaves the store, and no
// Domain value carries it. ErrNoToken when none is saved; a sealed value that does not open under
// this domain's id (a copy from another row, a damaged value) is an error that names no content.
func (s *Store) DomainToken(ctx context.Context, envID, domainID string) ([]byte, error) {
	row, err := domainInEnv(ctx, s.st.RQ, envID, domainID)
	if err != nil {
		return nil, err
	}
	if row.TokenSealed == "" {
		return nil, ErrNoToken
	}
	token, err := s.v.Open(vault.ContextIngressDNSToken+row.ID, row.TokenSealed)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: open the DNS token of domain %q: %w", row.Suffix, err)
	}
	return token, nil
}

// SetDomainToken replaces the domain's DNS token. It is the only writer of token_sealed after the
// domain is registered. The token is sealed under the domain's own id; the domain, its issuer and
// every claim under it stay. hook, which may be nil, runs in the same transaction after the update.
// The token is never logged and never put in an error. It is refused with *CleanupPendingError while the ledger
// holds a record the current token may still delete; the records the provider refused the current token for are
// made due again, so the next sweep tries them with the new token.
func (s *Store) SetDomainToken(ctx context.Context, envID, domainID string, token []byte, actor string, hook DomainHook) (ingress.Domain, error) {
	if len(token) == 0 {
		return ingress.Domain{}, ErrTokenEmpty
	}
	now := s.now().UTC()
	var out ingress.Domain
	err := s.write(ctx, envID, actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := domainInEnv(ctx, q, envID, domainID)
		if err != nil {
			return false, err
		}
		// Records the provider refuses the current token for do not hold the replacement back: the new token is the
		// only credential that can delete them.
		if err := cleanupPendingForReplace(ctx, q, row.ID); err != nil {
			return false, err
		}
		sealed, err := s.v.Seal(vault.ContextIngressDNSToken+row.ID, token)
		if err != nil {
			return false, fmt.Errorf("ingressstore: seal the DNS token: %w", err)
		}
		ts := toNanos(now)
		if _, err := q.SetIngressDomainToken(ctx, sqlc.SetIngressDomainTokenParams{
			TokenSealed: sealed, TokenUpdatedAt: ts, UpdatedAt: ts, ID: row.ID, EnvID: envID,
		}); err != nil {
			return false, fmt.Errorf("ingressstore: save the DNS token of domain %q: %w", row.Suffix, err)
		}
		// Every record left with the old token is tried with the new one at the next sweep.
		if _, err := q.RetryACMEDNSRecordsByCredential(ctx, row.ID); err != nil {
			return false, fmt.Errorf("ingressstore: retry the DNS records of domain %q: %w", row.Suffix, err)
		}
		row.TokenSealed, row.TokenUpdatedAt, row.UpdatedAt = sealed, ts, ts
		out = domainOf(row)
		if hook != nil {
			if err := hook(ctx, tx, out); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return ingress.Domain{}, err
	}
	return out, nil
}

// Holder is a claim that keeps a domain from being deleted.
type Holder struct {
	Hostname string
	// StackID and StackName are empty for a system claim.
	StackID   string
	StackName string
	EnvID     string
	EnvName   string
}

// DomainInUseError is a domain that cannot be deleted because claims sit under it. Holders are
// sorted by environment name, stack name and hostname.
type DomainInUseError struct {
	Holders []Holder
}

// Error implements error.
func (e *DomainInUseError) Error() string {
	return fmt.Sprintf("ingressstore: the domain is in use by %d hostname(s)", len(e.Holders))
}

// holdersOf lists the claims whose domain is domainID.
func holdersOf(ctx context.Context, q *sqlc.Queries, domainID string) ([]Holder, error) {
	rows, err := q.ListIngressHolders(ctx, domainID)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the holders of domain %q: %w", domainID, err)
	}
	out := make([]Holder, 0, len(rows))
	for _, r := range rows {
		out = append(out, Holder{Hostname: r.Hostname, StackID: r.StackID.String, StackName: r.StackName.String, EnvID: r.EnvID, EnvName: r.EnvName})
	}
	return out, nil
}

// CleanupPendingError is a domain that cannot be deleted, nor its token replaced, because the DNS ledger
// still holds TXT records TierPlan made with that token. The records are deleted by the ingress sweep with
// the token that made them; until then the token must stay, so no record is orphaned from the one credential
// that can delete it (D-27). A record the provider refuses that token for is the exception for a replacement:
// the token cannot delete it, and a new one is the way to.
type CleanupPendingError struct {
	// Records is how many ledger rows the domain still holds.
	Records int
	// TokenRefused is how many of them last failed because the provider refused the domain's token. Those wait for
	// a token that works, not for time: a token replacement is never held back by them.
	TokenRefused int
}

// Error implements error.
func (e *CleanupPendingError) Error() string {
	return fmt.Sprintf("ingressstore: %d DNS record(s) made with the domain's token are still to be removed", e.Records)
}

// cleanupPending is *CleanupPendingError when the ledger holds a row made with domainID's token. It reads
// inside the caller's write transaction, so a row cannot land between the check and the change.
func cleanupPending(ctx context.Context, q *sqlc.Queries, domainID string) error {
	n, refused, err := ledgerRows(ctx, q, domainID)
	if err != nil {
		return err
	}
	if n > 0 {
		return &CleanupPendingError{Records: n, TokenRefused: refused}
	}
	return nil
}

// cleanupPendingForReplace is cleanupPending for a token replacement: only the rows the current token may still
// delete hold it back. A row the provider last refused the token for waits for a working token, and the
// replacement is how it gets one, so refusing it would leave the records with no way out (D-27).
func cleanupPendingForReplace(ctx context.Context, q *sqlc.Queries, domainID string) error {
	n, refused, err := ledgerRows(ctx, q, domainID)
	if err != nil {
		return err
	}
	if n > refused {
		return &CleanupPendingError{Records: n - refused}
	}
	return nil
}

// ledgerRows counts the ledger rows made with domainID's token, and how many of them the provider last refused
// the token for.
func ledgerRows(ctx context.Context, q *sqlc.Queries, domainID string) (total, refused int, err error) {
	n, err := q.CountACMEDNSRecordsByCredential(ctx, domainID)
	if err != nil {
		return 0, 0, fmt.Errorf("ingressstore: count the DNS records of domain %q: %w", domainID, err)
	}
	if n == 0 {
		return 0, 0, nil
	}
	r, err := q.CountACMEDNSRecordsTokenRefused(ctx, domainID)
	if err != nil {
		return 0, 0, fmt.Errorf("ingressstore: count the refused DNS records of domain %q: %w", domainID, err)
	}
	return int(n), int(r), nil
}

// DeleteDomain removes a domain, with the names reserved under it. A domain whose token still has
// DNS records in the ledger is refused with *CleanupPendingError, and one that claims sit under is refused
// with *DomainInUseError, and nothing changes in either case; only claims that reference this domain row
// count, so a claim under apps.example.com does not block example.com. hook, which may be nil, runs
// in the same transaction after the delete, with the domain as it was.
func (s *Store) DeleteDomain(ctx context.Context, envID, domainID string, hook DomainHook) error {
	now := s.now().UTC()
	return s.write(ctx, envID, "", now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := domainInEnv(ctx, q, envID, domainID)
		if err != nil {
			return false, err
		}
		if err := cleanupPending(ctx, q, row.ID); err != nil {
			return false, err
		}
		holders, err := holdersOf(ctx, q, row.ID)
		if err != nil {
			return false, err
		}
		if len(holders) > 0 {
			return false, &DomainInUseError{Holders: holders}
		}
		if _, err := q.DeleteIngressDomain(ctx, sqlc.DeleteIngressDomainParams{ID: row.ID, EnvID: envID}); err != nil {
			if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
				// A claim landed between the read and the delete; say who holds the domain now.
				if holders, herr := holdersOf(ctx, q, row.ID); herr == nil && len(holders) > 0 {
					return false, &DomainInUseError{Holders: holders}
				}
			}
			return false, fmt.Errorf("ingressstore: delete the domain %q: %w", row.Suffix, err)
		}
		if hook != nil {
			if err := hook(ctx, tx, domainOf(row)); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// Reservation is a hostname an admin holds back under a domain.
type Reservation struct {
	DomainID  string
	Suffix    string
	Hostname  string
	CreatedBy string
	CreatedAt time.Time
}

// ReservedHook runs inside the transaction that reserves or releases a name, after the change and
// only when something changed. An error rolls the change back.
type ReservedHook func(ctx context.Context, tx *sql.Tx, r Reservation) error

// ReserveInput is a hostname to reserve.
type ReserveInput struct {
	EnvID    string
	DomainID string
	Hostname string
	Actor    string
	// CanView says which environments the caller may see, for the refusal of a claimed name; nil
	// names none.
	CanView func(envID string) bool
}

// ReservedClaimedError is a name that cannot be reserved because a stack or the system holds it.
// Message is the sentence for the caller, redacted by ingress.DescribeHolder with the caller's
// CanView; Holder carries names only when the caller may see them.
type ReservedClaimedError struct {
	Holder  ingress.ClaimInfo
	Message string
}

// Error implements error.
func (e *ReservedClaimedError) Error() string { return e.Message }

// Reserve holds back an exact hostname strictly below the domain, so no stack in any environment
// can claim it (D-11). The name is normalised like a claim's. The apex and names outside the domain
// are ErrNotUnderDomain; a name that is not a valid hostname is a *ingress.NameError; a name that is
// already claimed is a *ReservedClaimedError. The bool is true when the name was newly reserved;
// reserving a name that is reserved already changes nothing, runs no hook and returns the existing
// reservation.
func (s *Store) Reserve(ctx context.Context, in ReserveInput, hook ReservedHook) (Reservation, bool, error) {
	now := s.now().UTC()
	var out Reservation
	created := false
	err := s.write(ctx, in.EnvID, in.Actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		dom, err := domainInEnv(ctx, q, in.EnvID, in.DomainID)
		if err != nil {
			return false, err
		}
		host := ingress.NormalizeHost(in.Hostname)
		if err := ingress.ValidateHost(host); err != nil {
			msg := "The hostname is empty."
			if host != "" {
				msg = "“" + host + "” is not a valid hostname: " + err.Error() + "."
			}
			return false, &ingress.NameError{Code: ingress.CodeHostInvalid, Message: msg}
		}
		if !ingress.Under(host, dom.Suffix) {
			return false, ErrNotUnderDomain
		}
		c, err := q.GetIngressClaim(ctx, host)
		switch {
		case err == nil:
			return false, reservedClaimed(c, in.CanView)
		case !errors.Is(err, sql.ErrNoRows):
			return false, fmt.Errorf("ingressstore: read the claim of %q: %w", host, err)
		}
		ts := toNanos(now)
		n, err := q.InsertIngressReserved(ctx, sqlc.InsertIngressReservedParams{DomainID: dom.ID, Hostname: host, CreatedBy: in.Actor, CreatedAt: ts})
		if err != nil {
			return false, fmt.Errorf("ingressstore: reserve %q: %w", host, err)
		}
		if n == 0 {
			existing, err := reservationOf(ctx, q, dom, host)
			if err != nil {
				return false, err
			}
			out = existing
			return false, nil
		}
		out = Reservation{DomainID: dom.ID, Suffix: dom.Suffix, Hostname: host, CreatedBy: in.Actor, CreatedAt: fromNanos(ts)}
		created = true
		if hook != nil {
			if err := hook(ctx, tx, out); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return Reservation{}, false, err
	}
	return out, created, nil
}

// reservedClaimed builds the refusal of a claimed name. The sentence is DescribeHolder's, so it
// redacts exactly as the policy does; the Holder keeps names only for a caller who may see them.
func reservedClaimed(c sqlc.GetIngressClaimRow, canView func(string) bool) *ReservedClaimedError {
	info := ingress.ClaimInfo{
		Claim: ingress.Claim{
			EnvID: c.EnvID, Hostname: c.Hostname, DomainID: c.DomainID, OwnerKind: ingress.OwnerKind(c.OwnerKind),
			StackID: c.StackID.String, ClaimedBy: c.ClaimedBy, ClaimedAt: fromNanos(c.ClaimedAt),
		},
		EnvName: c.EnvName, StackName: c.StackName.String,
	}
	e := &ReservedClaimedError{Holder: info, Message: ingress.DescribeHolder(info, canView)}
	if info.OwnerKind == ingress.OwnerStack && (canView == nil || !canView(info.EnvID)) {
		e.Holder = ingress.ClaimInfo{Claim: ingress.Claim{Hostname: c.Hostname, OwnerKind: ingress.OwnerStack}}
	}
	return e
}

// reservationOf reads one reservation of a domain.
func reservationOf(ctx context.Context, q *sqlc.Queries, dom sqlc.IngressDomain, host string) (Reservation, error) {
	rows, err := q.ListIngressReservedByDomain(ctx, dom.ID)
	if err != nil {
		return Reservation{}, fmt.Errorf("ingressstore: list the reserved names of %q: %w", dom.Suffix, err)
	}
	for _, r := range rows {
		if r.Hostname == host {
			return Reservation{DomainID: dom.ID, Suffix: dom.Suffix, Hostname: r.Hostname, CreatedBy: r.CreatedBy, CreatedAt: fromNanos(r.CreatedAt)}, nil
		}
	}
	return Reservation{}, fmt.Errorf("ingressstore: the reservation of %q vanished inside its own transaction", host)
}

// Unreserve lets a reserved name go. It reports whether a reservation was removed; a name that was
// not reserved is a successful no-op that runs no hook and leaves config_version where it was.
func (s *Store) Unreserve(ctx context.Context, envID, domainID, hostname string, hook ReservedHook) (bool, error) {
	now := s.now().UTC()
	removed := false
	err := s.write(ctx, envID, "", now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		dom, err := domainInEnv(ctx, q, envID, domainID)
		if err != nil {
			return false, err
		}
		host := ingress.NormalizeHost(hostname)
		rows, err := q.ListIngressReservedByDomain(ctx, dom.ID)
		if err != nil {
			return false, fmt.Errorf("ingressstore: list the reserved names of %q: %w", dom.Suffix, err)
		}
		var r Reservation
		found := false
		for _, row := range rows {
			if row.Hostname == host {
				r = Reservation{DomainID: dom.ID, Suffix: dom.Suffix, Hostname: host, CreatedBy: row.CreatedBy, CreatedAt: fromNanos(row.CreatedAt)}
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
		if _, err := q.DeleteIngressReserved(ctx, sqlc.DeleteIngressReservedParams{DomainID: dom.ID, Hostname: host}); err != nil {
			return false, fmt.Errorf("ingressstore: release the reserved name %q: %w", host, err)
		}
		removed = true
		if hook != nil {
			if err := hook(ctx, tx, r); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// World reads what the install holds that a stack of envID is judged against: the environment's
// domains, every reserved name and every claim, with the names a message about them uses. canView
// is carried into the World for the clash message and may be nil.
func (s *Store) World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error) {
	return s.readWorld(ctx, s.st.RQ, envID, canView)
}

// readWorld reads the world through q. ReplaceStackRoutes passes the write transaction's queries,
// so two writers cannot both pass the check.
func (s *Store) readWorld(ctx context.Context, q *sqlc.Queries, envID string, canView func(string) bool) (ingress.World, error) {
	domains, err := q.ListIngressDomainsByEnv(ctx, envID)
	if err != nil {
		return ingress.World{}, fmt.Errorf("ingressstore: list the domains of environment %q: %w", envID, err)
	}
	reserved, err := q.ListIngressReservedInstall(ctx)
	if err != nil {
		return ingress.World{}, fmt.Errorf("ingressstore: list the reserved hostnames: %w", err)
	}
	claims, err := q.ListIngressClaimsInstall(ctx)
	if err != nil {
		return ingress.World{}, fmt.Errorf("ingressstore: list the claims: %w", err)
	}
	w := ingress.World{
		Domains:  make([]ingress.Domain, 0, len(domains)),
		Reserved: make(map[string]bool, len(reserved)),
		Claims:   make(map[string]ingress.ClaimInfo, len(claims)),
		CanView:  canView,
	}
	if s.agentHosts != nil {
		w.AgentHosts = normalizedHosts(s.agentHosts())
	}
	for _, d := range domains {
		w.Domains = append(w.Domains, domainOf(d))
	}
	for _, r := range reserved {
		w.Reserved[r.Hostname] = true
	}
	for _, c := range claims {
		w.Claims[c.Hostname] = claimInfoOf(c)
	}
	return w, nil
}

// normalizedHosts lower-cases hosts, drops blanks and removes duplicates, keeping the first of each.
func normalizedHosts(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		h = ingress.NormalizeHost(h)
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// claimInfoOf converts an install-wide claim row, with the names a message about it uses.
func claimInfoOf(c sqlc.ListIngressClaimsInstallRow) ingress.ClaimInfo {
	return ingress.ClaimInfo{
		Claim: ingress.Claim{
			EnvID: c.EnvID, Hostname: c.Hostname, DomainID: c.DomainID, OwnerKind: ingress.OwnerKind(c.OwnerKind),
			StackID: c.StackID.String, ClaimedBy: c.ClaimedBy, ClaimedAt: fromNanos(c.ClaimedAt),
		},
		EnvName: c.EnvName, StackName: c.StackName.String,
	}
}

// ReplaceInput is a stack's declarations to store.
type ReplaceInput struct {
	EnvID   string
	StackID string
	Actor   string
	// Decls are the routes the stack's labels declare; none releases every hostname the stack holds.
	Decls []ingress.Declaration
	// CanView says which environments the caller may see, for the clash message; nil names none.
	CanView func(envID string) bool
	// Hook, which may be nil, runs in the same transaction after the claims are written.
	Hook ClaimHook
}

// ReplaceStackRoutes makes the claims and routes of a stack exactly what its declarations say. It
// reads the world again through its own write transaction and runs ingress.Check on it, so a change
// that landed since the caller last looked is judged here; a refusal is a *RefusedError and nothing
// is written. The database's one-owner key is the backstop if the policy ever lets a clash through.
// A plan identical to what the stack already holds (hostnames, domains and every route's name, path,
// service, port, scheme and strip) writes nothing, runs no hook, leaves the environment's
// config_version where it was and notifies nobody; so does a stack with no declarations that holds
// no claim. A hostname the stack keeps keeps its claimed_by and claimed_at. It returns the plan that
// was stored.
//
// The deploy, roll back and restore of a revision and the git auto-deploy call it through the stack pipeline's claim
// write; an update images keeps the last good revision's routes and so writes only when that revision declares a
// hostname the stack does not hold.
func (s *Store) ReplaceStackRoutes(ctx context.Context, in ReplaceInput) (ingress.Plan, error) {
	now := s.now().UTC()
	var plan ingress.Plan
	err := s.write(ctx, in.EnvID, in.Actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		// No constraint ties a claim's environment, its stack's and its domain's together, so the
		// stack must be confirmed to belong to the environment here.
		if _, err := q.GetStack(ctx, sqlc.GetStackParams{EnvID: in.EnvID, ID: in.StackID}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrStackNotFound
			}
			return false, fmt.Errorf("ingressstore: read the stack %q: %w", in.StackID, err)
		}
		w, err := s.readWorld(ctx, q, in.EnvID, in.CanView)
		if err != nil {
			return false, err
		}
		p, problems := ingress.Check(in.StackID, in.Decls, w)
		if ingress.HasErrors(problems) {
			return false, &RefusedError{Problems: problems}
		}
		stack := sql.NullString{String: in.StackID, Valid: true}
		held, err := q.ListIngressHostnamesByStack(ctx, stack)
		if err != nil {
			return false, fmt.Errorf("ingressstore: list the claims of stack %q: %w", in.StackID, err)
		}
		routes, err := q.ListIngressRoutesByStack(ctx, stack)
		if err != nil {
			return false, fmt.Errorf("ingressstore: list the routes of stack %q: %w", in.StackID, err)
		}
		if samePlan(p, held, routes) {
			plan = p // what the stack holds is what its labels say: nothing to write, nothing to tell
			return false, nil
		}
		kept := make(map[string]sqlc.IngressHostname, len(held))
		for _, h := range held {
			kept[h.Hostname] = h
		}
		delta := claimDeltaOf(p, held, routes)
		// The routes go with their claims (ON DELETE CASCADE).
		if _, err := q.DeleteIngressHostnamesByStack(ctx, stack); err != nil {
			return false, fmt.Errorf("ingressstore: release the claims of stack %q: %w", in.StackID, err)
		}
		ts := toNanos(now)
		for _, c := range p.Claims {
			by, at := in.Actor, ts
			if prior, ok := kept[c.Hostname]; ok {
				by, at = prior.ClaimedBy, prior.ClaimedAt // a hostname the stack keeps keeps its history
			}
			if err := q.InsertIngressHostname(ctx, sqlc.InsertIngressHostnameParams{
				Hostname: c.Hostname, EnvID: in.EnvID, DomainID: c.DomainID, OwnerKind: string(ingress.OwnerStack),
				StackID: stack, ClaimedBy: by, ClaimedAt: at,
			}); err != nil {
				return false, fmt.Errorf("ingressstore: claim %q: %w", c.Hostname, err)
			}
			for _, r := range c.Routes {
				if err := insertRoute(ctx, q, in.EnvID, c.Hostname, r, ts); err != nil {
					return false, err
				}
			}
		}
		if in.Hook != nil {
			if err := in.Hook(ctx, tx, p, delta); err != nil {
				return false, err
			}
		}
		plan = p
		return true, nil
	})
	if err != nil {
		return ingress.Plan{}, err
	}
	return plan, nil
}

// ReleaseStack lets go of every hostname a stack holds, the same way a replacement with no
// declarations does: the claims and routes go, hook (which may be nil) runs with the empty plan in
// the same transaction, and the environment's version is raised and its notifier told once. A stack
// that holds nothing is a no-op.
//
// The stack removal job calls it, through stackops.Ingress, once the engine has nothing of the stack and before the
// definition is deleted, so the release is audited as ingress.claim_released in the claims' own transaction. The
// database trigger that counts a change when a stack row goes only backs that up.
func (s *Store) ReleaseStack(ctx context.Context, envID, stackID, actor string, hook ClaimHook) error {
	_, err := s.ReplaceStackRoutes(ctx, ReplaceInput{EnvID: envID, StackID: stackID, Actor: actor, Hook: hook})
	return err
}

// claimDeltaOf is the difference between what a stack holds (its claims and routes) and what the plan would store.
func claimDeltaOf(p ingress.Plan, held []sqlc.IngressHostname, routes []sqlc.IngressRoute) ClaimDelta {
	haveDomain := make(map[string]string, len(held))
	for _, h := range held {
		haveDomain[h.Hostname] = h.DomainID
	}
	haveRoutes := map[string][]string{}
	for _, r := range routes {
		haveRoutes[r.Hostname] = append(haveRoutes[r.Hostname], routeSig(r.Hostname, r.Name, r.PathPrefix, r.Service, r.Port, r.Scheme, r.Strip != 0))
	}
	var d ClaimDelta
	want := make(map[string]bool, len(p.Claims))
	for _, c := range p.Claims {
		want[c.Hostname] = true
		domain, ok := haveDomain[c.Hostname]
		if !ok {
			d.Added = append(d.Added, c.Hostname)
			continue
		}
		d.Kept = append(d.Kept, c.Hostname)
		var wantRoutes []string
		for _, r := range c.Routes {
			path := r.PathPrefix
			if path == "" {
				path = "/"
			}
			scheme := r.Scheme
			if scheme == "" {
				scheme = ingress.SchemeHTTP
			}
			wantRoutes = append(wantRoutes, routeSig(c.Hostname, r.Name, path, r.Service, int64(r.Port), string(scheme), r.Strip))
		}
		have := haveRoutes[c.Hostname]
		slices.Sort(wantRoutes)
		slices.Sort(have)
		if domain != c.DomainID || !slices.Equal(wantRoutes, have) {
			d.Changed = append(d.Changed, c.Hostname)
		}
	}
	for _, h := range held {
		if !want[h.Hostname] {
			d.Removed = append(d.Removed, h.Hostname)
		}
	}
	slices.Sort(d.Added)
	slices.Sort(d.Kept)
	slices.Sort(d.Changed)
	slices.Sort(d.Removed)
	return d
}

// samePlan reports whether what a stack holds is exactly what the plan would store: the same
// hostnames under the same domains, and the same routes. A route's empty path and scheme mean "/"
// and http, as they do when stored.
func samePlan(p ingress.Plan, held []sqlc.IngressHostname, routes []sqlc.IngressRoute) bool {
	var wantHosts, wantRoutes []string
	for _, c := range p.Claims {
		wantHosts = append(wantHosts, c.Hostname+"\x00"+c.DomainID)
		for _, r := range c.Routes {
			path := r.PathPrefix
			if path == "" {
				path = "/"
			}
			scheme := r.Scheme
			if scheme == "" {
				scheme = ingress.SchemeHTTP
			}
			wantRoutes = append(wantRoutes, routeSig(c.Hostname, r.Name, path, r.Service, int64(r.Port), string(scheme), r.Strip))
		}
	}
	var haveHosts, haveRoutes []string
	for _, h := range held {
		haveHosts = append(haveHosts, h.Hostname+"\x00"+h.DomainID)
	}
	for _, r := range routes {
		haveRoutes = append(haveRoutes, routeSig(r.Hostname, r.Name, r.PathPrefix, r.Service, r.Port, r.Scheme, r.Strip != 0))
	}
	slices.Sort(wantHosts)
	slices.Sort(haveHosts)
	slices.Sort(wantRoutes)
	slices.Sort(haveRoutes)
	return slices.Equal(wantHosts, haveHosts) && slices.Equal(wantRoutes, haveRoutes)
}

// routeSig is a route's identity for comparison.
func routeSig(host, name, path, service string, port int64, scheme string, strip bool) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%t", host, name, path, service, port, scheme, strip)
}

// insertRoute stores one route of a claimed hostname. The environment id is the claim's own.
func insertRoute(ctx context.Context, q *sqlc.Queries, envID, hostname string, r ingress.Declaration, ts int64) error {
	uid, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("ingressstore: new route id: %w", err)
	}
	path := r.PathPrefix
	if path == "" {
		path = "/"
	}
	scheme := r.Scheme
	if scheme == "" {
		scheme = ingress.SchemeHTTP
	}
	strip := int64(0)
	if r.Strip {
		strip = 1
	}
	if err := q.InsertIngressRoute(ctx, sqlc.InsertIngressRouteParams{
		ID: uid.String(), EnvID: envID, Hostname: hostname, Name: r.Name, PathPrefix: path, Service: r.Service,
		Port: int64(r.Port), Scheme: string(scheme), Strip: strip, CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		return fmt.Errorf("ingressstore: store the route of %q: %w", hostname, err)
	}
	return nil
}
