// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// ErrVIPNotFound is an environment with no virtual IP row.
var ErrVIPNotFound = errors.New("ingressstore: the environment has no virtual IP")

// VIPMovesKept is how many moves of one environment the store keeps. InsertVIPMove prunes the oldest
// beyond it in the same transaction (D-23).
const VIPMovesKept = 500

// vipDefaultPage is the page size ListVIPMoves uses when the caller asks for none.
const vipDefaultPage = 50

// VIPConfig is an environment's virtual IP: the operator's choice (D-24). Docker is the source of
// truth for what runs; this is only the desired configuration.
//
// KeySealed is the VRRP key as vault.Seal produced it, under vault.ContextIngressVIPKey plus the
// environment id. The caller seals it before PutVIP and opens it where the secret is built; this
// store never opens it, never logs it and never returns it as anything but the sealed text.
type VIPConfig struct {
	EnvID string
	// Address is the IPv4 address without a prefix length.
	Address netip.Addr
	// PrefixLen is the prefix length of the network the address sits in, 1 to 32.
	PrefixLen int
	// RouterID is the VRRP virtual router id, 1 to 255.
	RouterID int
	// RouterDerived is true when the server derived RouterID, false when the admin chose it.
	RouterDerived bool
	// KeySealed is the sealed key. See the type's comment.
	KeySealed string
	// KeyGeneration is 1 for the first key and one higher per rotation.
	KeyGeneration int
	// Enabled is false while a switch-off is in progress: the ensure loop finishes the removal.
	Enabled bool
	// CreatedAt is when the row was made; PutVIP stamps now when it is the zero time. A replace keeps
	// the stored value.
	CreatedAt time.Time
	// UpdatedAt is stamped by every write that changes the configuration.
	UpdatedAt time.Time
	// EnsureAt is when the last ensure pass ended; the zero time when none has.
	EnsureAt time.Time
	// EnsureError is the class of the last pass's failure, "" when it succeeded. Never engine text.
	EnsureError string
}

// Prefix is the address with its prefix length: the network's form of the virtual IP. It is the
// zero Prefix when the address or the length is not valid.
func (c VIPConfig) Prefix() netip.Prefix {
	return netip.PrefixFrom(c.Address, c.PrefixLen)
}

// VIPMove is one move of the address from one node to another, as the server saw it (D-23). An empty
// node id or name means no node: the address had no holder before, or has none after.
type VIPMove struct {
	ID           int64
	EnvID        string
	At           time.Time
	FromNodeID   string
	FromNodeName string
	ToNodeID     string
	ToNodeName   string
	// Reason is one of holder_failed, probe_failing, drain_handoff, disabled and unknown.
	Reason string
}

// VIPMoveCursor is the place after which the next page of moves starts: the instant and the insertion
// id of the last move of the previous page. The id breaks ties between equal instants, so a page
// boundary inside a run of equal instants repeats and skips nothing.
type VIPMoveCursor struct {
	At time.Time
	ID int64
}

// vipOf converts a row. A stored address that does not parse is a corrupt row and an error.
func vipOf(r sqlc.IngressVip) (VIPConfig, error) {
	addr, err := netip.ParseAddr(r.Address)
	if err != nil {
		return VIPConfig{}, fmt.Errorf("ingressstore: the virtual IP of environment %q holds an address that does not parse: %w", r.EnvID, err)
	}
	return VIPConfig{
		EnvID: r.EnvID, Address: addr, PrefixLen: int(r.PrefixLen), RouterID: int(r.RouterID),
		RouterDerived: r.RouterDerived != 0, KeySealed: r.KeySealed, KeyGeneration: int(r.KeyGeneration),
		Enabled: r.Enabled != 0, CreatedAt: fromNanos(r.CreatedAt), UpdatedAt: fromNanos(r.UpdatedAt),
		EnsureAt: fromNanos(r.EnsureAt), EnsureError: r.EnsureError,
	}, nil
}

// vipMoveOf converts a row.
func vipMoveOf(r sqlc.IngressVipMove) VIPMove {
	return VIPMove{
		ID: r.ID, EnvID: r.EnvID, At: fromNanos(r.At), FromNodeID: r.FromNodeID, FromNodeName: r.FromNodeName,
		ToNodeID: r.ToNodeID, ToNodeName: r.ToNodeName, Reason: r.Reason,
	}
}

// PutVIP creates the environment's virtual IP or replaces its configuration. The ensure state
// (EnsureAt, EnsureError) is the ensure loop's and is kept; CreatedAt of an existing row is kept.
// cfg.KeySealed must already be sealed.
func (s *Store) PutVIP(ctx context.Context, cfg VIPConfig) error {
	err := s.st.Q.PutVIP(ctx, s.putVIPParams(cfg))
	if err != nil {
		return fmt.Errorf("ingressstore: write the virtual IP of environment %q: %w", cfg.EnvID, err)
	}
	return nil
}

// PutVIPWithHook is PutVIP with hook run in the same transaction, after the row is written: an error of
// the hook rolls the row back. The enable writes its audit row here, so the virtual IP never exists without
// it. A nil hook is PutVIP.
func (s *Store) PutVIPWithHook(ctx context.Context, cfg VIPConfig, hook DeployHook) error {
	return s.writeQuiet(ctx, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.PutVIP(ctx, s.putVIPParams(cfg)); err != nil {
			return false, fmt.Errorf("ingressstore: write the virtual IP of environment %q: %w", cfg.EnvID, err)
		}
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// putVIPParams is the row PutVIP writes: updated_at is now and created_at is cfg's, or now when it is zero.
func (s *Store) putVIPParams(cfg VIPConfig) sqlc.PutVIPParams {
	now := s.now().UTC()
	created := cfg.CreatedAt
	if created.IsZero() {
		created = now
	}
	return sqlc.PutVIPParams{
		EnvID: cfg.EnvID, Address: cfg.Address.String(), PrefixLen: int64(cfg.PrefixLen), RouterID: int64(cfg.RouterID),
		RouterDerived: boolInt(cfg.RouterDerived), KeySealed: cfg.KeySealed, KeyGeneration: int64(cfg.KeyGeneration),
		Enabled: boolInt(cfg.Enabled), CreatedAt: toNanos(created), UpdatedAt: toNanos(now),
	}
}

// SealVIPKey seals a VRRP key of the environment for VIPConfig.KeySealed, under the context the column is
// registered with (vault.ContextIngressVIPKey plus the environment ID), so a re-wrap opens it again. It is the
// one way the key is sealed; the store never opens it.
func (s *Store) SealVIPKey(envID string, raw []byte) (string, error) {
	if s.v == nil {
		return "", errors.New("ingressstore: no vault to seal the virtual IP key with")
	}
	sealed, err := s.v.Seal(vault.ContextIngressVIPKey+envID, raw)
	if err != nil {
		return "", fmt.Errorf("ingressstore: seal the virtual IP key of environment %q: %w", envID, err)
	}
	return sealed, nil
}

// GetVIP reads an environment's virtual IP. An environment with none is ErrVIPNotFound.
func (s *Store) GetVIP(ctx context.Context, envID string) (VIPConfig, error) {
	row, err := s.st.RQ.GetVIP(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return VIPConfig{}, ErrVIPNotFound
	}
	if err != nil {
		return VIPConfig{}, fmt.Errorf("ingressstore: read the virtual IP of environment %q: %w", envID, err)
	}
	return vipOf(row)
}

// ListVIPs lists every virtual IP of the install, ordered by environment id.
func (s *Store) ListVIPs(ctx context.Context) ([]VIPConfig, error) {
	rows, err := s.st.RQ.ListVIPs(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the virtual IPs: %w", err)
	}
	out := make([]VIPConfig, 0, len(rows))
	for _, r := range rows {
		c, err := vipOf(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// SetVIPEnabled sets the switch. false means a switch-off is in progress and the ensure loop finishes
// the removal. A missing row is ErrVIPNotFound.
func (s *Store) SetVIPEnabled(ctx context.Context, envID string, enabled bool) error {
	n, err := s.st.Q.SetVIPEnabled(ctx, sqlc.SetVIPEnabledParams{Enabled: boolInt(enabled), UpdatedAt: toNanos(s.now().UTC()), EnvID: envID})
	if err != nil {
		return fmt.Errorf("ingressstore: set the virtual IP switch of environment %q: %w", envID, err)
	}
	if n == 0 {
		return ErrVIPNotFound
	}
	return nil
}

// UpdateVIPAddress changes the address, its prefix length and the router id, with whether the router
// id was derived. The key and its generation are untouched. A missing row is ErrVIPNotFound.
func (s *Store) UpdateVIPAddress(ctx context.Context, envID string, addr netip.Addr, prefixLen, routerID int, routerDerived bool) error {
	n, err := s.st.Q.UpdateVIPAddress(ctx, sqlc.UpdateVIPAddressParams{
		Address: addr.String(), PrefixLen: int64(prefixLen), RouterID: int64(routerID),
		RouterDerived: boolInt(routerDerived), UpdatedAt: toNanos(s.now().UTC()), EnvID: envID,
	})
	if err != nil {
		return fmt.Errorf("ingressstore: update the virtual IP address of environment %q: %w", envID, err)
	}
	if n == 0 {
		return ErrVIPNotFound
	}
	return nil
}

// DeleteVIP removes the environment's virtual IP row and reports whether there was one. The move
// history stays: it goes with the environment, not with the switch.
func (s *Store) DeleteVIP(ctx context.Context, envID string) (bool, error) {
	n, err := s.st.Q.DeleteVIP(ctx, envID)
	if err != nil {
		return false, fmt.Errorf("ingressstore: delete the virtual IP of environment %q: %w", envID, err)
	}
	return n > 0, nil
}

// UpdateVIPAddressWithHook is UpdateVIPAddress with hook run in the same transaction, after the row is written: an error of
// the hook rolls the change back. The change job writes its audit row here, so the address never changes without it. A nil
// hook is UpdateVIPAddress.
func (s *Store) UpdateVIPAddressWithHook(ctx context.Context, envID string, addr netip.Addr, prefixLen, routerID int, routerDerived bool, hook DeployHook) error {
	return s.writeQuiet(ctx, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		n, err := q.UpdateVIPAddress(ctx, sqlc.UpdateVIPAddressParams{
			Address: addr.String(), PrefixLen: int64(prefixLen), RouterID: int64(routerID),
			RouterDerived: boolInt(routerDerived), UpdatedAt: toNanos(s.now().UTC()), EnvID: envID,
		})
		if err != nil {
			return false, fmt.Errorf("ingressstore: update the virtual IP address of environment %q: %w", envID, err)
		}
		if n == 0 {
			return false, ErrVIPNotFound
		}
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// DeleteVIPWithHook is DeleteVIP with hook run in the same transaction, after the row is deleted and only when there was one:
// an error of the hook rolls the delete back. The switch-off writes its audit row here, so the row never goes without it. A nil
// hook is DeleteVIP.
func (s *Store) DeleteVIPWithHook(ctx context.Context, envID string, hook DeployHook) (bool, error) {
	deleted := false
	err := s.writeQuiet(ctx, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		n, err := q.DeleteVIP(ctx, envID)
		if err != nil {
			return false, fmt.Errorf("ingressstore: delete the virtual IP of environment %q: %w", envID, err)
		}
		if n == 0 {
			return false, nil
		}
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		deleted = true
		return true, nil
	})
	return deleted, err
}

// RecordVIPEnsure stores the result of one ensure pass: when it ended and the class of its failure, ""
// for success. errClass must be a short class, never engine text, which can carry a secret. It
// changes no configuration, so UpdatedAt stays. A missing row is ErrVIPNotFound.
func (s *Store) RecordVIPEnsure(ctx context.Context, envID string, at time.Time, errClass string) error {
	n, err := s.st.Q.RecordVIPEnsure(ctx, sqlc.RecordVIPEnsureParams{EnsureAt: toNanos(at), EnsureError: errClass, EnvID: envID})
	if err != nil {
		return fmt.Errorf("ingressstore: record the virtual IP ensure pass of environment %q: %w", envID, err)
	}
	if n == 0 {
		return ErrVIPNotFound
	}
	return nil
}

// InsertVIPMove records one move and prunes the environment's history to the newest VIPMovesKept
// rows, in one transaction. m.ID is ignored: the store assigns it. Two moves at the same instant are
// two rows.
func (s *Store) InsertVIPMove(ctx context.Context, m VIPMove) error {
	return s.writeQuiet(ctx, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.InsertVIPMove(ctx, sqlc.InsertVIPMoveParams{
			EnvID: m.EnvID, At: toNanos(m.At), FromNodeID: m.FromNodeID, FromNodeName: m.FromNodeName,
			ToNodeID: m.ToNodeID, ToNodeName: m.ToNodeName, Reason: m.Reason,
		}); err != nil {
			return false, fmt.Errorf("ingressstore: record a virtual IP move of environment %q: %w", m.EnvID, err)
		}
		if err := q.PruneVIPMoves(ctx, sqlc.PruneVIPMovesParams{EnvID: m.EnvID, Keep: VIPMovesKept}); err != nil {
			return false, fmt.Errorf("ingressstore: prune the virtual IP moves of environment %q: %w", m.EnvID, err)
		}
		return true, nil
	})
}

// ListVIPMoves reads one page of an environment's moves, newest first by time, equal instants by
// insertion id descending. before is nil for the first page, otherwise the cursor a previous call
// returned. The returned slice is never nil. The returned cursor is nil when there is no further page,
// including when the environment has no moves. limit below 1 means the default page.
func (s *Store) ListVIPMoves(ctx context.Context, envID string, before *VIPMoveCursor, limit int) ([]VIPMove, *VIPMoveCursor, error) {
	if limit < 1 {
		limit = vipDefaultPage
	}
	beforeAt, beforeID := int64(math.MaxInt64), int64(math.MaxInt64)
	if before != nil {
		beforeAt, beforeID = toNanos(before.At), before.ID
	}
	// One row more than the page says whether there is a further page.
	rows, err := s.st.RQ.ListVIPMoves(ctx, sqlc.ListVIPMovesParams{
		EnvID: envID, BeforeAt: beforeAt, BeforeID: beforeID, RowLimit: int64(limit) + 1,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ingressstore: list the virtual IP moves of environment %q: %w", envID, err)
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]VIPMove, 0, len(rows))
	for _, r := range rows {
		out = append(out, vipMoveOf(r))
	}
	if !more {
		return out, nil, nil
	}
	last := out[len(out)-1]
	return out, &VIPMoveCursor{At: last.At, ID: last.ID}, nil
}

// LatestVIPMove reads the newest move of an environment; ok is false when it has none.
func (s *Store) LatestVIPMove(ctx context.Context, envID string) (VIPMove, bool, error) {
	row, err := s.st.RQ.LatestVIPMove(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return VIPMove{}, false, nil
	}
	if err != nil {
		return VIPMove{}, false, fmt.Errorf("ingressstore: read the latest virtual IP move of environment %q: %w", envID, err)
	}
	return vipMoveOf(row), true, nil
}

// VIPMovesSince lists the moves of an environment strictly after since, oldest first. The slice is
// never nil.
func (s *Store) VIPMovesSince(ctx context.Context, envID string, since time.Time) ([]VIPMove, error) {
	rows, err := s.st.RQ.VIPMovesSince(ctx, sqlc.VIPMovesSinceParams{EnvID: envID, At: toNanos(since)})
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the virtual IP moves of environment %q since %s: %w", envID, since.UTC().Format(time.RFC3339), err)
	}
	out := make([]VIPMove, 0, len(rows))
	for _, r := range rows {
		out = append(out, vipMoveOf(r))
	}
	return out, nil
}
