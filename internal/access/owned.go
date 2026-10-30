// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// States of an owned resource (owned_resources.state).
const (
	OwnedActive                = "active"
	OwnedPausedLacksPermission = "paused_lacks_permission"
	OwnedPausedDisabled        = "paused_disabled"
	OwnedPausedDeleted         = "paused_deleted"
)

var (
	// ErrOwnerLacksPermission is returned when an automation may not run: its
	// owner no longer holds what it needs, or is disabled or deleted. The row is
	// paused, with an audit entry, before this is returned (D-14).
	ErrOwnerLacksPermission = errors.New("access: the owner no longer holds the permission this needs")
	// ErrUnknownOwnedKind is a kind nothing registered with rbac.RegisterOwnedKind.
	ErrUnknownOwnedKind = errors.New("access: that kind of automation is not registered")
	// ErrUnknownSystemPrincipal is a system principal name that is not in
	// rbac.SystemPrincipals. Nothing runs as it.
	ErrUnknownSystemPrincipal = errors.New("access: that is not a system principal")
	// ErrOwnedNotFound is an automation that is not recorded.
	ErrOwnedNotFound = errors.New("access: no such automation")
	// ErrNotEligible is a new owner who could not run the automation right now:
	// unknown, deleted, disabled, already its owner, or without the permission its
	// kind needs on its environment (Admin when it needs env Admin). Nothing is
	// changed (AUTH-10, D-14).
	ErrNotEligible = errors.New("access: that user could not run this automation")
)

// OwnedResource is an automation recorded as acting on a user's authority: a
// webhook, a git sync (D-14). Only this package writes the rows.
type OwnedResource struct {
	Kind, ResourceID, EnvID, Name, OwnerID, State string
	// NeedsEnvAdmin is set when the automation's last run used host-escalating
	// compose keys, so its owner must hold env Admin (D-13).
	NeedsEnvAdmin bool
}

func ownedFromRow(r sqlc.OwnedResource) OwnedResource {
	return OwnedResource{
		Kind: r.Kind, ResourceID: r.ResourceID, EnvID: r.EnvID, Name: r.Name,
		OwnerID: r.OwnerID, State: r.State, NeedsEnvAdmin: r.NeedsEnvAdmin != 0,
	}
}

// ownerAccess is what a recheck needs to know about one owner.
type ownerAccess struct {
	state string // OwnedPausedDeleted or OwnedPausedDisabled for an owner who cannot act, else ""
	acc   rbac.Access
}

// readOwner reads an owner's standing through q, so a recheck inside a write
// transaction sees that transaction's own changes. An owner with no row, or a
// tombstoned one, is deleted; a disabled one is disabled.
func (s *Service) readOwner(ctx context.Context, q *sqlc.Queries, ownerID string) (ownerAccess, error) {
	f, err := q.GetUserAccessFlags(ctx, ownerID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ownerAccess{state: OwnedPausedDeleted}, nil
	case err != nil:
		return ownerAccess{}, fmt.Errorf("access: read owner %s: %w", ownerID, err)
	case f.DeletedAt.Valid:
		return ownerAccess{state: OwnedPausedDeleted}, nil
	case f.Disabled != 0:
		return ownerAccess{state: OwnedPausedDisabled}, nil
	}
	a, err := s.Checker.AccessQ(ctx, q, ownerID)
	if err != nil {
		return ownerAccess{}, err
	}
	return ownerAccess{acc: a}, nil
}

// desiredState is the one state a row should be in given its owner (D-14): a
// deleted owner pauses it for good, a disabled one pauses it until the owner
// is back, and an active one keeps it running only while the owner holds the
// kind's permission on the environment (env Admin when the row needs it).
func (s *Service) desiredState(r sqlc.OwnedResource, o ownerAccess) string {
	switch {
	case o.state != "":
		return o.state
	case !s.Checker.OwnerQualifies(o.acc, r.Kind, r.EnvID, r.NeedsEnvAdmin != 0):
		return OwnedPausedLacksPermission
	}
	return OwnedActive
}

// RegisterOwnedTx records an automation as owned by r.OwnerID, in the caller's
// write transaction (q is bound to tx). The kind must be registered, and the
// owner must qualify now, because an automation is never created with more
// rights than its owner holds. A new row is active.
func (s *Service) RegisterOwnedTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, r OwnedResource) error {
	_ = tx // the writes go through q, which the caller bound to tx
	if _, ok := rbac.LookupOwnedKind(r.Kind); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownOwnedKind, r.Kind)
	}
	var needs int64
	if r.NeedsEnvAdmin {
		needs = 1
	}
	row := sqlc.OwnedResource{Kind: r.Kind, ResourceID: r.ResourceID, EnvID: r.EnvID, OwnerID: r.OwnerID, NeedsEnvAdmin: needs}
	o, err := s.readOwner(ctx, q, r.OwnerID)
	if err != nil {
		return err
	}
	if s.desiredState(row, o) != OwnedActive {
		return ErrOwnerLacksPermission
	}
	now := s.now().UnixNano()
	if err := q.InsertOwnedResource(ctx, sqlc.InsertOwnedResourceParams{
		Kind: r.Kind, ResourceID: r.ResourceID, EnvID: r.EnvID, Name: r.Name, OwnerID: r.OwnerID,
		State: OwnedActive, NeedsEnvAdmin: needs, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("access: record owned %s %s: %w", r.Kind, r.ResourceID, err)
	}
	return nil
}

// DeregisterOwnedTx forgets an automation, in the caller's write transaction (q is bound to tx): the resource it ran for
// was removed, so nothing is left to pause or hand over. One that is not recorded is not an error, so removing the
// resource twice, or removing one that never had an owner, does the same.
func (s *Service) DeregisterOwnedTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, kind, resourceID string) error {
	_ = tx // the write goes through q, which the caller bound to tx
	if _, err := q.DeleteOwnedResource(ctx, sqlc.DeleteOwnedResourceParams{Kind: kind, ResourceID: resourceID}); err != nil {
		return fmt.Errorf("access: forget owned %s %s: %w", kind, resourceID, err)
	}
	return nil
}

// MarkNeedsEnvAdmin records that an automation's last run used host-escalating compose keys, so from now on its owner
// must hold env Admin to keep it running (D-13, D-14). The owner is rechecked at the next RunAs, not here. An automation
// that is not recorded is ErrOwnedNotFound; marking it twice changes nothing the second time.
func (s *Service) MarkNeedsEnvAdmin(ctx context.Context, kind, resourceID string) error {
	row, err := s.Store.RQ.GetOwnedResource(ctx, sqlc.GetOwnedResourceParams{Kind: kind, ResourceID: resourceID})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOwnedNotFound
	}
	if err != nil {
		return fmt.Errorf("access: read owned %s %s: %w", kind, resourceID, err)
	}
	if row.NeedsEnvAdmin != 0 {
		return nil
	}
	if err := s.Store.Q.SetOwnedNeedsEnvAdmin(ctx, sqlc.SetOwnedNeedsEnvAdminParams{
		NeedsEnvAdmin: 1, UpdatedAt: s.now().UnixNano(), Kind: kind, ResourceID: resourceID,
	}); err != nil {
		return fmt.Errorf("access: mark owned %s %s: %w", kind, resourceID, err)
	}
	return nil
}

// RecheckOwnedTx re-evaluates every automation the given owners hold, inside
// the caller's write transaction, and returns how many changed state. Callers
// run it in the transaction that disables or deletes a user, or changes a
// grant, so the pause lands with the change. Rechecking twice changes nothing
// the second time and writes no second entry.
func (s *Service) RecheckOwnedTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, ownerIDs []string, actor audit.Actor) (int, error) {
	if len(ownerIDs) == 0 {
		return 0, nil
	}
	owners, err := json.Marshal(ownerIDs)
	if err != nil {
		return 0, fmt.Errorf("access: encode owners: %w", err)
	}
	rows, err := q.ListOwnedByOwners(ctx, string(owners))
	if err != nil {
		return 0, fmt.Errorf("access: list owned resources: %w", err)
	}
	return s.recheckRowsTx(ctx, tx, q, rows, actor)
}

// recheckRowsTx moves each row to the state its owner now warrants and audits
// the move. A row paused because its owner was deleted never resumes by itself
// (D-14): only a transfer of ownership gives it a living owner.
func (s *Service) recheckRowsTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, rows []sqlc.OwnedResource, actor audit.Actor) (int, error) {
	owners := map[string]ownerAccess{}
	changed := 0
	for _, r := range rows {
		o, seen := owners[r.OwnerID]
		if !seen {
			var err error
			if o, err = s.readOwner(ctx, q, r.OwnerID); err != nil {
				return changed, err
			}
			owners[r.OwnerID] = o
		}
		want := s.desiredState(r, o)
		if want == r.State || r.State == OwnedPausedDeleted {
			continue
		}
		if err := s.setOwnedStateTx(ctx, tx, q, r, want, actor); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// setOwnedStateTx moves a row to a state and audits the move: a move back to
// active is automation.resumed, any other is automation.paused.
func (s *Service) setOwnedStateTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, r sqlc.OwnedResource, want string, actor audit.Actor) error {
	if err := q.UpdateOwnedState(ctx, sqlc.UpdateOwnedStateParams{
		State: want, UpdatedAt: s.now().UnixNano(), Kind: r.Kind, ResourceID: r.ResourceID,
	}); err != nil {
		return fmt.Errorf("access: update owned %s %s: %w", r.Kind, r.ResourceID, err)
	}
	action := audit.ActionAutomationPaused
	if want == OwnedActive {
		action = audit.ActionAutomationResumed
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: action, TargetType: "automation", TargetID: r.ResourceID, EnvID: r.EnvID,
		Result:  audit.ResultSuccess,
		Details: map[string]string{"kind": r.Kind, "resource": r.ResourceID, "state": want, "owner": r.OwnerID},
	}); err != nil {
		return fmt.Errorf("access: audit %s: %w", action, err)
	}
	return nil
}

// RunAs is called before an automation acts. It rechecks that one row in its own
// transaction, so the owner's rights are read now and a pause is audited, then
// returns the principal to act as: the system principal systemID on behalf of
// the owner, whose every permission check is the owner's current one. If the
// owner no longer qualifies the row is left paused and the error is
// ErrOwnerLacksPermission, so the run does not happen.
func (s *Service) RunAs(ctx context.Context, kind, resourceID, systemID string) (*rbac.Principal, OwnedResource, error) {
	if rbac.System(systemID) == nil {
		return nil, OwnedResource{}, ErrUnknownSystemPrincipal
	}
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, OwnedResource{}, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	row, err := q.GetOwnedResource(ctx, sqlc.GetOwnedResourceParams{Kind: kind, ResourceID: resourceID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, OwnedResource{}, ErrNotFound
	}
	if err != nil {
		return nil, OwnedResource{}, fmt.Errorf("access: read owned %s %s: %w", kind, resourceID, err)
	}
	actor := rbac.System(systemID).AuditActor()
	if _, err := s.recheckRowsTx(ctx, tx, q, []sqlc.OwnedResource{row}, actor); err != nil {
		return nil, OwnedResource{}, err
	}
	row, err = q.GetOwnedResource(ctx, sqlc.GetOwnedResourceParams{Kind: kind, ResourceID: resourceID})
	if err != nil {
		return nil, OwnedResource{}, fmt.Errorf("access: reread owned %s %s: %w", kind, resourceID, err)
	}
	// Commit before answering, so the pause and its entry persist with the refusal.
	if err := tx.Commit(); err != nil {
		return nil, OwnedResource{}, fmt.Errorf("access: commit: %w", err)
	}
	res := ownedFromRow(row)
	if res.State != OwnedActive {
		return nil, res, fmt.Errorf("%w (%s)", ErrOwnerLacksPermission, res.State)
	}
	return rbac.OnBehalf(systemID, res.OwnerID), res, nil
}

// UserRef names a user without anything private: who an automation could be
// handed to.
type UserRef struct {
	ID, Username, DisplayName string
}

// OwnedItem is an owned automation as an admin sees it: the row, the label of its
// kind and the name of its environment.
type OwnedItem struct {
	OwnedResource
	// Label names the kind to people ("Git sync"), as rbac.OwnedKind.Label gives it.
	Label string
	// EnvName is the environment's name, or its ID when it no longer exists.
	EnvName string
}

func (s *Service) ownedItem(kind, resourceID, envID, name, ownerID, state string, needsEnvAdmin int64, envName string) OwnedItem {
	label := kind
	if k, ok := rbac.LookupOwnedKind(kind); ok {
		label = k.Label
	}
	if envName == "" {
		envName = envID
	}
	return OwnedItem{
		OwnedResource: OwnedResource{
			Kind: kind, ResourceID: resourceID, EnvID: envID, Name: name,
			OwnerID: ownerID, State: state, NeedsEnvAdmin: needsEnvAdmin != 0,
		},
		Label: label, EnvName: envName,
	}
}

// ListOwned lists every automation a user owns, sorted by kind and ID. The
// caller has checked users.manage.
func (s *Service) ListOwned(ctx context.Context, ownerID string) ([]OwnedItem, error) {
	rows, err := s.Store.RQ.ListOwnedDetailByOwner(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("access: list owned by %s: %w", ownerID, err)
	}
	out := make([]OwnedItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.ownedItem(r.Kind, r.ResourceID, r.EnvID, r.Name, ownerID, r.State, r.NeedsEnvAdmin, r.EnvName))
	}
	return out, nil
}

// GetOwned reads one automation. One that is not recorded is ErrOwnedNotFound.
func (s *Service) GetOwned(ctx context.Context, kind, resourceID string) (OwnedItem, error) {
	r, err := s.Store.RQ.GetOwnedDetail(ctx, sqlc.GetOwnedDetailParams{Kind: kind, ResourceID: resourceID})
	if errors.Is(err, sql.ErrNoRows) {
		return OwnedItem{}, ErrOwnedNotFound
	}
	if err != nil {
		return OwnedItem{}, fmt.Errorf("access: read owned %s %s: %w", kind, resourceID, err)
	}
	return s.ownedItem(r.Kind, r.ResourceID, r.EnvID, r.Name, r.OwnerID, r.State, r.NeedsEnvAdmin, r.EnvName), nil
}

// EligibleOwners lists the users an automation could be handed to right now:
// active users, other than its owner, who pass the permission its kind needs on
// its environment (env Admin when it needs that), judged from their grants as
// they are at this moment. Sorted by username. TransferOwned checks again, so
// the answer is a convenience and never the authority.
func (s *Service) EligibleOwners(ctx context.Context, kind, resourceID string) ([]UserRef, error) {
	row, err := s.Store.RQ.GetOwnedResource(ctx, sqlc.GetOwnedResourceParams{Kind: kind, ResourceID: resourceID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOwnedNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("access: read owned %s %s: %w", kind, resourceID, err)
	}
	users, err := s.Store.RQ.ListActiveUsersForEligibility(ctx)
	if err != nil {
		return nil, fmt.Errorf("access: list users: %w", err)
	}
	out := []UserRef{}
	for _, u := range users {
		if u.ID == row.OwnerID {
			continue
		}
		o, err := s.readOwner(ctx, s.Store.RQ, u.ID)
		if err != nil {
			return nil, err
		}
		if s.desiredState(row, o) == OwnedActive {
			out = append(out, UserRef{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
		}
	}
	return out, nil
}

// TransferOwned hands an automation to another user (D-14). The new owner must
// be able to run it at this moment: the check is made inside the transaction
// that makes the change, so a grant removed after the list was read is seen. If
// they could not, the answer is ErrNotEligible and nothing changes. A paused
// automation is active again under its new owner, even one paused because its
// owner was deleted. The transfer is audited as owner.transferred. The caller
// has checked users.manage.
func (s *Service) TransferOwned(ctx context.Context, actor audit.Actor, kind, resourceID, newOwnerID string, opts ...Option) error {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := s.transferTx(ctx, tx, s.Store.Q.WithTx(tx), actor, kind, resourceID, newOwnerID, o); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// transferTx is TransferOwned inside the caller's write transaction, so a user's
// deletion can hand automations over in the same transaction as the tombstone.
func (s *Service) transferTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, actor audit.Actor, kind, resourceID, newOwnerID string, o options) error {
	row, err := q.GetOwnedResource(ctx, sqlc.GetOwnedResourceParams{Kind: kind, ResourceID: resourceID})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOwnedNotFound
	}
	if err != nil {
		return fmt.Errorf("access: read owned %s %s: %w", kind, resourceID, err)
	}
	if _, ok := rbac.LookupOwnedKind(kind); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownOwnedKind, kind)
	}
	if newOwnerID == row.OwnerID {
		return ErrNotEligible
	}
	next, err := s.readOwner(ctx, q, newOwnerID)
	if err != nil {
		return err
	}
	if s.desiredState(row, next) != OwnedActive {
		return ErrNotEligible
	}
	if err := q.TransferOwned(ctx, sqlc.TransferOwnedParams{
		OwnerID: newOwnerID, UpdatedAt: s.now().UnixNano(), Kind: kind, ResourceID: resourceID,
	}); err != nil {
		return fmt.Errorf("access: transfer owned %s %s: %w", kind, resourceID, err)
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionOwnerTransferred, TargetType: "automation", TargetID: resourceID, EnvID: row.EnvID,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"kind": kind, "resource": resourceID, "from": row.OwnerID, "to": newOwnerID},
	}); err != nil {
		return fmt.Errorf("access: audit owner transferred: %w", err)
	}
	if row.State != OwnedActive {
		row.OwnerID = newOwnerID
		return s.setOwnedStateTx(ctx, tx, q, row, OwnedActive, actor)
	}
	return nil
}
