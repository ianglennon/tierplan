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

// ClaimHook runs inside the transaction that replaces a stack's claims, after they are written, with
// the plan that was stored. An error rolls the replacement back.
type ClaimHook func(ctx context.Context, tx *sql.Tx, p ingress.Plan) error

// Deps are what a Store needs.
type Deps struct {
	St    *store.Store
	Vault *vault.Vault
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Store is the ingress model in the database. DNS tokens are sealed by the vault under the domain's
// own id, so a sealed token copied into another domain's row does not open.
type Store struct {
	st  *store.Store
	v   *vault.Vault
	now func() time.Time
}

// New returns a Store.
func New(d Deps) *Store {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Store{st: d.St, v: d.Vault, now: now}
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
// the version where it was. fn's error rolls everything back.
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

// World reads what the install holds that a stack of envID is judged against: the environment's
// domains, every reserved name and every claim, with the names a message about them uses. canView
// is carried into the World for the clash message and may be nil.
func (s *Store) World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error) {
	return readWorld(ctx, s.st.RQ, envID, canView)
}

// readWorld reads the world through q. ReplaceStackRoutes passes the write transaction's queries,
// so two writers cannot both pass the check.
func readWorld(ctx context.Context, q *sqlc.Queries, envID string, canView func(string) bool) (ingress.World, error) {
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
	for _, d := range domains {
		w.Domains = append(w.Domains, domainOf(d))
	}
	for _, r := range reserved {
		w.Reserved[r.Hostname] = true
	}
	for _, c := range claims {
		w.Claims[c.Hostname] = ingress.ClaimInfo{
			Claim: ingress.Claim{
				EnvID: c.EnvID, Hostname: c.Hostname, DomainID: c.DomainID, OwnerKind: ingress.OwnerKind(c.OwnerKind),
				StackID: c.StackID.String, ClaimedBy: c.ClaimedBy, ClaimedAt: fromNanos(c.ClaimedAt),
			},
			EnvName: c.EnvName, StackName: c.StackName.String,
		}
	}
	return w, nil
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
// A stack with no declarations that holds no claim commits nothing and leaves the environment's
// config_version where it was. It returns the plan that was stored.
//
// Only tests call it in this phase: no deploy, rollback, git or update path stores claims yet.
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
		w, err := readWorld(ctx, q, in.EnvID, in.CanView)
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
		if len(p.Claims) == 0 && len(held) == 0 {
			return false, nil
		}
		// The routes go with their claims (ON DELETE CASCADE).
		if _, err := q.DeleteIngressHostnamesByStack(ctx, stack); err != nil {
			return false, fmt.Errorf("ingressstore: release the claims of stack %q: %w", in.StackID, err)
		}
		ts := toNanos(now)
		for _, c := range p.Claims {
			if err := q.InsertIngressHostname(ctx, sqlc.InsertIngressHostnameParams{
				Hostname: c.Hostname, EnvID: in.EnvID, DomainID: c.DomainID, OwnerKind: string(ingress.OwnerStack),
				StackID: stack, ClaimedBy: in.Actor, ClaimedAt: ts,
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
			if err := in.Hook(ctx, tx, p); err != nil {
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
