// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// DeployState is what the deployer needs of one environment's ingress row: the switch, the access-log
// choice, the restore pause and the result of the last ensure pass. An environment with no row is the
// zero DeployState: everything off, nothing ensured.
type DeployState struct {
	Enabled    bool
	AccessLogs bool
	// Paused is set by a restore (D-42): the ensure loop and the pending wipes only observe until an
	// admin resumes.
	Paused bool
	// EnsureAt is when the last ensure pass over the environment ended; the zero time when none has.
	EnsureAt time.Time
	// EnsureError is the class of the last pass's failure, "" when it succeeded. Never engine text.
	EnsureError string
}

// PendingWipe is a certificate cache volume on a node that could not be reached when the ingress was
// removed. The keys in it stay on that node until the wipe is done (D-27).
type PendingWipe struct {
	EnvID       string
	SwarmNodeID string
	Volume      string
	CreatedAt   time.Time
}

// LiveRoute is one route of a claim of the environment, whether or not its stack is running (D-25).
type LiveRoute struct {
	Hostname string
	// PathPrefix is the path the route matches; a stored empty prefix is read as "/".
	PathPrefix string
	// StackID and StackName name the stack that holds the claim; both are empty for a system claim.
	StackID   string
	StackName string
}

// DeployHook runs inside the transaction of a switch change, after the row is written and only when
// the value actually moved. An error rolls the change back: the audit row of a switch is written
// here, so a switch never moves without it.
type DeployHook func(ctx context.Context, tx *sql.Tx) error

// boolInt is the stored form of a switch.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// switchOp writes one switch column. current reads the value now (ok false when the environment has
// no row), set writes the new value and reports whether the row changed.
type switchOp struct {
	current func(sqlc.GetIngressDeployStateRow) bool
	set     func(ctx context.Context, q *sqlc.Queries, on bool) (int64, error)
}

// writeSwitch moves one switch of an environment. An environment with no row that is asked for "off"
// is left without one: nothing moved, so no row, no version and no notification. A change raises
// config_version and notifies once (through write); a repeat changes nothing. withVersion false is
// for a switch that changes no route: the row is written and nothing else happens.
func (s *Store) writeSwitch(ctx context.Context, envID string, on bool, actor string, op switchOp, hook DeployHook, withVersion bool) (bool, error) {
	now := s.now().UTC()
	changed := false
	body := func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		cur, err := q.GetIngressDeployState(ctx, envID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !on {
				return false, nil
			}
		case err != nil:
			return false, fmt.Errorf("ingressstore: read the ingress state of environment %q: %w", envID, err)
		default:
			if op.current(cur) == on {
				return false, nil
			}
		}
		n, err := op.set(ctx, q, on)
		if err != nil {
			return false, fmt.Errorf("ingressstore: write the ingress switch of environment %q: %w", envID, err)
		}
		if n == 0 {
			return false, nil
		}
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		changed = true
		return true, nil
	}
	if withVersion {
		if err := s.write(ctx, envID, actor, now, body); err != nil {
			return false, err
		}
		return changed, nil
	}
	if err := s.writeQuiet(ctx, body); err != nil {
		return false, err
	}
	return changed, nil
}

// writeQuiet runs fn in one write transaction and commits it. It never raises config_version and
// never notifies: for state that changes no route.
func (s *Store) writeQuiet(ctx context.Context, fn func(tx *sql.Tx, q *sqlc.Queries) (bool, error)) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ingressstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if _, err := fn(tx, s.st.Q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ingressstore: commit: %w", err)
	}
	return nil
}

// Notify tells the store's notifier that the environment changed, for a change the store did not make
// itself (a stack deleted, whose claims went with its row). It is nil-safe: a store built without a
// notifier tells nobody.
func (s *Store) Notify(envID string) {
	s.notifier.Notify(envID)
}

// SetEnabled switches an environment's ingress on or off. The first switch creates the environment's
// row. A change raises config_version once, notifies once and reports true; asking for the value it
// already has changes nothing and reports false. hook, which may be nil, runs in the transaction of a
// change.
func (s *Store) SetEnabled(ctx context.Context, envID string, on bool, actor string, hook DeployHook) (bool, error) {
	return s.writeSwitch(ctx, envID, on, actor, switchOp{
		current: func(r sqlc.GetIngressDeployStateRow) bool { return r.Enabled != 0 },
		set: func(ctx context.Context, q *sqlc.Queries, on bool) (int64, error) {
			return q.UpsertIngressEnabled(ctx, sqlc.UpsertIngressEnabledParams{EnvID: envID, Enabled: boolInt(on)})
		},
	}, hook, true)
}

// SetAccessLogs switches the proxy's access log on or off. It changes how the proxy service is
// declared, so it raises config_version like the other switch (D-30). The same change rules as
// SetEnabled.
func (s *Store) SetAccessLogs(ctx context.Context, envID string, on bool, actor string, hook DeployHook) (bool, error) {
	return s.writeSwitch(ctx, envID, on, actor, switchOp{
		current: func(r sqlc.GetIngressDeployStateRow) bool { return r.AccessLogs != 0 },
		set: func(ctx context.Context, q *sqlc.Queries, on bool) (int64, error) {
			return q.SetIngressAccessLogs(ctx, sqlc.SetIngressAccessLogsParams{EnvID: envID, AccessLogs: boolInt(on)})
		},
	}, hook, true)
}

// SetPaused sets or clears the restore pause (D-42). It changes no route, so config_version does not
// move and nobody is notified; it reports whether the value moved.
func (s *Store) SetPaused(ctx context.Context, envID string, paused bool, actor string, hook DeployHook) (bool, error) {
	return s.writeSwitch(ctx, envID, paused, actor, switchOp{
		current: func(r sqlc.GetIngressDeployStateRow) bool { return r.Paused != 0 },
		set: func(ctx context.Context, q *sqlc.Queries, on bool) (int64, error) {
			return q.SetIngressPaused(ctx, sqlc.SetIngressPausedParams{EnvID: envID, Paused: boolInt(on)})
		},
	}, hook, false)
}

// RecordEnsure stores the result of one ensure pass: when it ended and the class of its failure, ""
// for success. It is a plain write: no version, no notification. errClass must be a short class, not
// engine text, which can carry a secret.
func (s *Store) RecordEnsure(ctx context.Context, envID string, at time.Time, errClass string) error {
	return s.writeQuiet(ctx, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.RecordIngressEnsure(ctx, sqlc.RecordIngressEnsureParams{
			EnvID: envID, EnsureAt: toNanos(at), EnsureError: errClass,
		}); err != nil {
			return false, fmt.Errorf("ingressstore: record the ensure pass of environment %q: %w", envID, err)
		}
		return false, nil
	})
}

// DeployState reads an environment's deploy state. An environment with no ingress row gives the zero
// DeployState and no error.
func (s *Store) DeployState(ctx context.Context, envID string) (DeployState, error) {
	row, err := s.st.RQ.GetIngressDeployState(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployState{}, nil
	}
	if err != nil {
		return DeployState{}, fmt.Errorf("ingressstore: read the deploy state of environment %q: %w", envID, err)
	}
	return DeployState{
		Enabled: row.Enabled != 0, AccessLogs: row.AccessLogs != 0, Paused: row.Paused != 0,
		EnsureAt: fromNanos(row.EnsureAt), EnsureError: row.EnsureError,
	}, nil
}

// EnabledEnvs lists the ids of the environments whose ingress is switched on, ordered by id.
func (s *Store) EnabledEnvs(ctx context.Context) ([]string, error) {
	ids, err := s.st.RQ.ListEnabledIngressEnvs(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the environments with ingress on: %w", err)
	}
	return ids, nil
}

// AddPendingWipe queues the wipe of a node's certificate cache volume. Queuing the same wipe again
// changes nothing. tx, when not nil, is the transaction the caller is writing in, so the wipe is
// queued with the audit row that names it; nil writes on its own.
func (s *Store) AddPendingWipe(ctx context.Context, tx *sql.Tx, envID, swarmNodeID, volume string, at time.Time) error {
	q := s.st.Q
	if tx != nil {
		q = q.WithTx(tx)
	}
	if err := q.InsertIngressPendingWipe(ctx, sqlc.InsertIngressPendingWipeParams{
		EnvID: envID, SwarmNodeID: swarmNodeID, Volume: volume, CreatedAt: toNanos(at),
	}); err != nil {
		return fmt.Errorf("ingressstore: queue the wipe of %q on node %q: %w", volume, swarmNodeID, err)
	}
	return nil
}

// QueueWipe queues the wipe of a node's certificate cache volume and runs hook, which may be nil, in the same transaction, so
// the audit row that names the queued wipe is never written without it. Queuing the same wipe again changes nothing.
func (s *Store) QueueWipe(ctx context.Context, envID, swarmNodeID, volume string, at time.Time, hook DeployHook) error {
	return s.writeQuiet(ctx, func(tx *sql.Tx, _ *sqlc.Queries) (bool, error) {
		if err := s.AddPendingWipe(ctx, tx, envID, swarmNodeID, volume, at); err != nil {
			return false, err
		}
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// CompleteWipe removes a queued wipe once it is done. hook, which may be nil, runs in the same transaction, and only when
// there was a queued wipe to remove: the audit row of a done wipe is never written for one that was not queued. It reports
// whether there was one.
func (s *Store) CompleteWipe(ctx context.Context, envID, swarmNodeID, volume string, hook DeployHook) (bool, error) {
	cleared := false
	err := s.writeQuiet(ctx, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		n, err := q.DeleteIngressPendingWipe(ctx, sqlc.DeleteIngressPendingWipeParams{EnvID: envID, SwarmNodeID: swarmNodeID, Volume: volume})
		if err != nil {
			return false, fmt.Errorf("ingressstore: clear the wipe of %q on node %q: %w", volume, swarmNodeID, err)
		}
		if n == 0 {
			return false, nil
		}
		cleared = true
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	return cleared, err
}

// pendingWipeOf converts a row.
func pendingWipeOf(r sqlc.IngressPendingWipe) PendingWipe {
	return PendingWipe{EnvID: r.EnvID, SwarmNodeID: r.SwarmNodeID, Volume: r.Volume, CreatedAt: fromNanos(r.CreatedAt)}
}

// PendingWipes lists one environment's queued wipes, oldest first.
func (s *Store) PendingWipes(ctx context.Context, envID string) ([]PendingWipe, error) {
	rows, err := s.st.RQ.ListIngressPendingWipes(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the pending wipes of environment %q: %w", envID, err)
	}
	out := make([]PendingWipe, 0, len(rows))
	for _, r := range rows {
		out = append(out, pendingWipeOf(r))
	}
	return out, nil
}

// AllPendingWipes lists every queued wipe of the install, by environment then oldest first.
func (s *Store) AllPendingWipes(ctx context.Context) ([]PendingWipe, error) {
	rows, err := s.st.RQ.ListAllIngressPendingWipes(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the pending wipes: %w", err)
	}
	out := make([]PendingWipe, 0, len(rows))
	for _, r := range rows {
		out = append(out, pendingWipeOf(r))
	}
	return out, nil
}

// ClearPendingWipe removes a queued wipe once it is done and reports whether there was one.
func (s *Store) ClearPendingWipe(ctx context.Context, envID, swarmNodeID, volume string) (bool, error) {
	n, err := s.st.Q.DeleteIngressPendingWipe(ctx, sqlc.DeleteIngressPendingWipeParams{
		EnvID: envID, SwarmNodeID: swarmNodeID, Volume: volume,
	})
	if err != nil {
		return false, fmt.Errorf("ingressstore: clear the wipe of %q on node %q: %w", volume, swarmNodeID, err)
	}
	return n > 0, nil
}

// LiveRoutes lists every route of every claim of the environment, stopped stacks included, ordered by
// hostname then path prefix. A claim of another environment is never listed.
func (s *Store) LiveRoutes(ctx context.Context, envID string) ([]LiveRoute, error) {
	rows, err := s.st.RQ.ListIngressLiveRoutes(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the routes of environment %q: %w", envID, err)
	}
	out := make([]LiveRoute, 0, len(rows))
	for _, r := range rows {
		prefix := r.PathPrefix
		if prefix == "" {
			prefix = "/"
		}
		out = append(out, LiveRoute{Hostname: r.Hostname, PathPrefix: prefix, StackID: r.StackID, StackName: r.StackName})
	}
	return out, nil
}
