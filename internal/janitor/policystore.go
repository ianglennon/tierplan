// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ErrPolicyVersion means the policy was saved by someone else since the caller read it, so nothing
// was written.
var ErrPolicyVersion = errors.New("janitor: the policy was changed since it was read")

// PolicyStore keeps the janitor's policy in SQLite at three levels (D-04): the instance defaults, each
// environment's overrides and each swarm node's. Every run path reads the policy through Effective.
type PolicyStore struct {
	St *store.Store
}

// Defaults are the instance defaults, the retention of the run history and the version of the row.
func (p PolicyStore) Defaults(ctx context.Context) (Policy, Retention, int64, error) {
	return readDefaults(ctx, p.St.RQ)
}

// DefaultsOn is Defaults read on the caller's transaction q, so that a save reads the defaults it replaces in
// the same transaction that writes the new ones.
func (p PolicyStore) DefaultsOn(ctx context.Context, q *sqlc.Queries) (Policy, Retention, int64, error) {
	return readDefaults(ctx, q)
}

func readDefaults(ctx context.Context, q *sqlc.Queries) (Policy, Retention, int64, error) {
	row, err := q.GetJanitorDefaults(ctx)
	if err != nil {
		return Policy{}, Retention{}, 0, fmt.Errorf("read the janitor defaults: %w", err)
	}
	pol := Policy{
		Enabled: row.Enabled != 0, Schedule: row.Schedule, SchedulePreset: row.SchedulePreset,
		KeepN: int(row.KeepN), Grace: time.Duration(row.GraceHours) * time.Hour,
		RemoveUnusedNetworks: row.RemoveUnusedNetworks != 0, RemoveStoppedContainers: row.RemoveStoppedContainers != 0,
	}
	return pol, Retention{Days: int(row.RetentionDays), Runs: int(row.RetentionRuns)}, row.Version, nil
}

// Retention is how long the run history is kept now: the one the instance defaults store (D-16). The runner,
// the startup prune and the status read it, so a saved change reaches all three.
func (p PolicyStore) Retention(ctx context.Context) (Retention, error) {
	_, r, _, err := p.Defaults(ctx)
	return r, err
}

// Followers is how many environments follow at least one instance default: those with no policy of their own
// and those whose policy leaves any field to the default.
func (p PolicyStore) Followers(ctx context.Context) (int, error) {
	n, err := p.St.RQ.CountJanitorFollowers(ctx)
	if err != nil {
		return 0, fmt.Errorf("count the environments that follow a janitor default: %w", err)
	}
	return int(n), nil
}

// RetentionPreview is how many runs, of how many environments, saving the retention r would delete now. It
// answers what SetDefaults would do, so a retention that does not lower the stored one deletes nothing and
// answers zero. r must be in range.
func (p PolicyStore) RetentionPreview(ctx context.Context, r Retention, now time.Time) (runs, environments int, err error) {
	if !r.InRange() {
		return 0, 0, fmt.Errorf("%w: a retention is %d to %d days and %d to %d runs", ErrOutOfRange, MinRetentionDays, MaxRetentionDays, MinRetentionRuns, MaxRetentionRuns)
	}
	cur, err := p.Retention(ctx)
	if err != nil {
		return 0, 0, err
	}
	if !r.lowers(cur) {
		return 0, 0, nil
	}
	row, err := p.St.RQ.CountJanitorRunsToPrune(ctx, sqlc.CountJanitorRunsToPruneParams{Cutoff: r.cutoff(now), Keep: int64(r.Runs)})
	if err != nil {
		return 0, 0, fmt.Errorf("count the janitor runs a retention would delete: %w", err)
	}
	return int(row.Runs), int(row.Environments), nil
}

// Env is an environment's overrides and the version they were read at. An environment with no row
// follows every default and has version 0.
func (p PolicyStore) Env(ctx context.Context, envID string) (Overrides, int64, error) {
	return readEnvPolicy(ctx, p.St.RQ, envID)
}

// EnvOn is Env read on the caller's transaction q, so that a save reads the overrides it replaces in the
// same transaction that writes the new ones.
func (p PolicyStore) EnvOn(ctx context.Context, q *sqlc.Queries, envID string) (Overrides, int64, error) {
	return readEnvPolicy(ctx, q, envID)
}

func readEnvPolicy(ctx context.Context, q *sqlc.Queries, envID string) (Overrides, int64, error) {
	row, err := q.GetJanitorEnvPolicy(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return Overrides{}, 0, nil
	}
	if err != nil {
		return Overrides{}, 0, fmt.Errorf("read the janitor policy of an environment: %w", err)
	}
	return Overrides{
		Enabled: boolPtr(row.Enabled), Schedule: stringPtr(row.Schedule), SchedulePreset: stringPtr(row.SchedulePreset),
		KeepN: intPtr(row.KeepN), GraceHours: intPtr(row.GraceHours),
		RemoveUnusedNetworks: boolPtr(row.RemoveUnusedNetworks), RemoveStoppedContainers: boolPtr(row.RemoveStoppedContainers),
	}, row.Version, nil
}

// Effective is the policy the janitor of an environment follows now: its overrides over the defaults.
func (p PolicyStore) Effective(ctx context.Context, envID string) (Policy, error) {
	def, _, _, err := p.Defaults(ctx)
	if err != nil {
		return Policy{}, err
	}
	env, _, err := p.Env(ctx, envID)
	if err != nil {
		return Policy{}, err
	}
	return Effective(def, env), nil
}

// Nodes are the overrides of the swarm nodes of an environment, by swarm node ID. A node with no
// entry follows its environment.
func (p PolicyStore) Nodes(ctx context.Context, envID string) (map[string]NodeOverrides, error) {
	rows, err := p.St.RQ.ListJanitorNodePolicies(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the janitor policy of the nodes of an environment: %w", err)
	}
	out := make(map[string]NodeOverrides, len(rows))
	for _, r := range rows {
		out[r.NodeID] = NodeOverrides{Enabled: boolPtr(r.Enabled), KeepN: intPtr(r.KeepN)}
	}
	return out, nil
}

// SetEnv saves the overrides of an environment on the caller's transaction q, which also records the
// audit row. version is the one Env answered when the caller read the policy: a save made since is
// ErrPolicyVersion and writes nothing. Saving what is already stored writes nothing and answers
// changed false. Every value passes Validate first. A nil field is stored as NULL, so it follows the
// default again.
func (p PolicyStore) SetEnv(ctx context.Context, q *sqlc.Queries, envID string, o Overrides, version int64, by string, now time.Time) (changed bool, err error) {
	if err := Validate(o); err != nil {
		return false, err
	}
	cur, curVersion, err := readEnvPolicy(ctx, q, envID)
	if err != nil {
		return false, err
	}
	if curVersion != version {
		return false, ErrPolicyVersion
	}
	want := toRow(o)
	if toRow(cur) == want {
		return false, nil
	}
	at := now.UTC().UnixNano()
	if curVersion == 0 {
		n, err := q.InsertJanitorEnvPolicy(ctx, sqlc.InsertJanitorEnvPolicyParams{
			EnvID: envID, Enabled: want.enabled, Schedule: want.schedule, SchedulePreset: want.preset, KeepN: want.keepN,
			GraceHours: want.grace, RemoveUnusedNetworks: want.networks, RemoveStoppedContainers: want.containers,
			UpdatedBy: by, UpdatedAt: at,
		})
		if err != nil {
			return false, fmt.Errorf("save the janitor policy of an environment: %w", err)
		}
		if n == 0 {
			return false, ErrPolicyVersion
		}
		return true, nil
	}
	n, err := q.UpdateJanitorEnvPolicy(ctx, sqlc.UpdateJanitorEnvPolicyParams{
		Enabled: want.enabled, Schedule: want.schedule, SchedulePreset: want.preset, KeepN: want.keepN,
		GraceHours: want.grace, RemoveUnusedNetworks: want.networks, RemoveStoppedContainers: want.containers,
		UpdatedBy: by, UpdatedAt: at, EnvID: envID, Version: version,
	})
	if err != nil {
		return false, fmt.Errorf("save the janitor policy of an environment: %w", err)
	}
	if n == 0 {
		return false, ErrPolicyVersion
	}
	return true, nil
}

// SetDefaults saves the instance defaults and the retention of the run history on the caller's transaction q,
// which also records the audit row. version is the one Defaults answered when the caller read them: a save made
// since is ErrPolicyVersion and writes nothing. Saving what is already stored writes nothing and answers changed
// false. Every value passes the same checks an environment's do, and the retention must be in range.
//
// A retention that lowers the stored one deletes, on the same transaction, the runs it no longer keeps in every
// environment (never one that is waiting or running, and a run's items and evidence go with it); removedRuns says
// how many. A retention that does not lower it deletes nothing.
func (p PolicyStore) SetDefaults(ctx context.Context, q *sqlc.Queries, pol Policy, r Retention, version int64, by string, now time.Time) (removedRuns int64, changed bool, err error) {
	if err := validateDefaults(pol, r); err != nil {
		return 0, false, err
	}
	cur, curRet, curVersion, err := readDefaults(ctx, q)
	if err != nil {
		return 0, false, err
	}
	if curVersion != version {
		return 0, false, ErrPolicyVersion
	}
	if cur == pol && curRet == r {
		return 0, false, nil
	}
	n, err := q.UpdateJanitorDefaults(ctx, sqlc.UpdateJanitorDefaultsParams{
		Enabled: boolInt(pol.Enabled), Schedule: pol.Schedule, SchedulePreset: pol.SchedulePreset, KeepN: int64(pol.KeepN),
		GraceHours: int64(pol.Grace / time.Hour), RemoveUnusedNetworks: boolInt(pol.RemoveUnusedNetworks),
		RemoveStoppedContainers: boolInt(pol.RemoveStoppedContainers), RetentionDays: int64(r.Days), RetentionRuns: int64(r.Runs),
		UpdatedBy: by, UpdatedAt: now.UTC().UnixNano(), Version: version,
	})
	if err != nil {
		return 0, false, fmt.Errorf("save the janitor defaults: %w", err)
	}
	if n == 0 {
		return 0, false, ErrPolicyVersion
	}
	if r.lowers(curRet) {
		removed, err := q.PruneAllJanitorRuns(ctx, sqlc.PruneAllJanitorRunsParams{Cutoff: r.cutoff(now), Keep: int64(r.Runs)})
		if err != nil {
			return 0, false, fmt.Errorf("prune the janitor runs the new retention does not keep: %w", err)
		}
		removedRuns = removed
	}
	return removedRuns, true, nil
}

// validateDefaults checks a full policy and retention: every field is set, so there is nothing to follow.
func validateDefaults(pol Policy, r Retention) error {
	if pol.Grace%time.Hour != 0 {
		return fmt.Errorf("%w: the grace period is whole hours", ErrOutOfRange)
	}
	keep, grace := pol.KeepN, int(pol.Grace/time.Hour)
	if err := Validate(Overrides{KeepN: &keep, GraceHours: &grace, Schedule: &pol.Schedule, SchedulePreset: &pol.SchedulePreset}); err != nil {
		return err
	}
	if !r.InRange() {
		return fmt.Errorf("%w: a retention is %d to %d days and %d to %d runs", ErrOutOfRange, MinRetentionDays, MaxRetentionDays, MinRetentionRuns, MaxRetentionRuns)
	}
	return nil
}

// envRow is Overrides as the columns store them, comparable so that "nothing changed" is one ==.
type envRow struct {
	enabled, keepN, grace, networks, containers sql.NullInt64
	schedule, preset                            sql.NullString
}

func toRow(o Overrides) envRow {
	return envRow{
		enabled: nullBool(o.Enabled), keepN: nullInt(o.KeepN), grace: nullInt(o.GraceHours),
		networks: nullBool(o.RemoveUnusedNetworks), containers: nullBool(o.RemoveStoppedContainers),
		schedule: nullString(o.Schedule), preset: nullString(o.SchedulePreset),
	}
}

func nullBool(b *bool) sql.NullInt64 {
	if b == nil {
		return sql.NullInt64{}
	}
	if *b {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Valid: true}
}

func nullInt(i *int) sql.NullInt64 {
	if i == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*i), Valid: true}
}

func nullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func boolPtr(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	i := int(n.Int64)
	return &i
}

func stringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}
