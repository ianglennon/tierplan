// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/schedule"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// When an environment is checked for image updates (IMG-04, D-01). The policy has two levels, like the
// janitor's (Phase 11, D-02): the instance defaults, and each environment's overrides, where a nil field
// follows the default and is never a copy of it.

const (
	// MinInterval is the shortest gap between two scheduled checks a schedule may have. A registry is asked
	// politely, so a schedule that fires more often than every hour is refused.
	MinInterval = time.Hour
	// DefaultSchedule is the cron expression of the default: every 6 hours, at 00:00, 06:00, 12:00 and 18:00
	// in the server's time zone, moved a little per environment so they do not all start together.
	DefaultSchedule = "0 */6 * * *"
	// DefaultSchedulePreset names DefaultSchedule in the UI.
	DefaultSchedulePreset = schedule.PresetEvery6h
	// DefaultStaleAfter is how old a successful check may be before it reads stale in an environment whose
	// schedule is off.
	DefaultStaleAfter = defaultStaleAfter
	// StaleIntervals is how many gaps of its schedule an environment's last check may be old before an
	// unchanged digest reads check_stale: a check or two may be missed without the page saying so.
	StaleIntervals = 3
)

// ErrVersionChanged means the policy was saved by someone else since the caller read it, so nothing was
// written.
var ErrVersionChanged = errors.New("updates: the update check policy was changed since it was read")

// ErrInvalidSchedule and ErrTooFrequent are the errors of a schedule that cannot be used, as the shared
// schedule package answers them.
var (
	ErrInvalidSchedule = schedule.ErrInvalidSchedule
	ErrTooFrequent     = schedule.ErrTooFrequent
)

// Policy is when an environment is checked for image updates.
type Policy struct {
	// Enabled says whether the schedule checks the environment. A person can still press Check now.
	Enabled bool
	// Schedule is a cron expression; Preset names it for the UI ("every_6h", "custom").
	Schedule, Preset string
}

// DefaultPolicy is the policy of an environment nobody has configured: checked every 6 hours.
func DefaultPolicy() Policy {
	return Policy{Enabled: true, Schedule: DefaultSchedule, Preset: DefaultSchedulePreset}
}

// Overrides are the fields an environment sets itself. A nil field follows the instance default.
type Overrides struct {
	Enabled          *bool
	Schedule, Preset *string
}

// Effective is the policy of an environment: each field of its overrides, or the default where the
// override is nil. A schedule override brings its preset with it ("custom" when it names none); a preset
// without a schedule is ignored, so the two always describe one schedule.
func Effective(defaults Policy, env Overrides) Policy {
	p := defaults
	if env.Enabled != nil {
		p.Enabled = *env.Enabled
	}
	if env.Schedule != nil {
		p.Schedule = *env.Schedule
		p.Preset = schedule.PresetCustom
		if env.Preset != nil {
			p.Preset = *env.Preset
		}
	}
	return p
}

func validPreset(name string) bool {
	switch name {
	case schedule.PresetHourly, schedule.PresetEvery6h, schedule.PresetDaily, schedule.PresetWeekly, schedule.PresetCustom:
		return true
	}
	return false
}

// Validate checks the values an environment would save: the schedule through schedule.ParseSchedule with
// the one hour floor (ErrInvalidSchedule, ErrTooFrequent), and a preset that names a schedule. A nil field is
// not checked: it follows the default.
func Validate(o Overrides) error {
	if o.Schedule != nil {
		if _, err := schedule.ParseSchedule(*o.Schedule, MinInterval); err != nil {
			return err
		}
	}
	if o.Preset != nil {
		if !validPreset(*o.Preset) {
			return fmt.Errorf("%w: %q is not a schedule preset", ErrInvalidSchedule, *o.Preset)
		}
		if o.Schedule == nil {
			return fmt.Errorf("%w: a preset needs the schedule it names", ErrInvalidSchedule)
		}
	}
	return nil
}

// StaleAfterOf is how old a successful check of an environment may be before an unchanged digest reads
// check_stale: StaleIntervals times the gap between the next two firings of its schedule, and
// DefaultStaleAfter when its schedule is off or cannot be read. now and loc place the gap, which is
// not the same every day across a clock change.
func StaleAfterOf(p Policy, envID string, now time.Time, loc *time.Location) time.Duration {
	if !p.Enabled {
		return DefaultStaleAfter
	}
	next, err := schedule.NextRuns(p.Schedule, envID, now, loc, 2, MinInterval)
	if err != nil || len(next) < 2 {
		return DefaultStaleAfter
	}
	return StaleIntervals * next[1].Sub(next[0])
}

// PolicyStore keeps the update check policy in SQLite at two levels: the instance defaults and each
// environment's overrides. Q runs on the write connection W, like SQLStore.
type PolicyStore struct {
	Q *sqlc.Queries
	W *sql.DB
}

// Defaults are the instance defaults and the version of the row.
func (p PolicyStore) Defaults(ctx context.Context) (Policy, int64, error) {
	return readDefaults(ctx, p.Q)
}

// DefaultsOn is Defaults read on the caller's transaction q, so that a save reads the defaults it replaces
// in the same transaction that writes the new ones.
func (p PolicyStore) DefaultsOn(ctx context.Context, q *sqlc.Queries) (Policy, int64, error) {
	return readDefaults(ctx, q)
}

func readDefaults(ctx context.Context, q *sqlc.Queries) (Policy, int64, error) {
	row, err := q.GetImageUpdateDefaults(ctx)
	if err != nil {
		return Policy{}, 0, fmt.Errorf("read the image update check defaults: %w", err)
	}
	return Policy{Enabled: row.Enabled != 0, Schedule: row.Schedule, Preset: row.SchedulePreset}, row.Version, nil
}

// Env is an environment's overrides and the version they were read at. An environment with no row follows
// every default and has version 0.
func (p PolicyStore) Env(ctx context.Context, envID string) (Overrides, int64, error) {
	return readEnv(ctx, p.Q, envID)
}

// EnvOn is Env read on the caller's transaction q.
func (p PolicyStore) EnvOn(ctx context.Context, q *sqlc.Queries, envID string) (Overrides, int64, error) {
	return readEnv(ctx, q, envID)
}

func readEnv(ctx context.Context, q *sqlc.Queries, envID string) (Overrides, int64, error) {
	row, err := q.GetImageUpdateEnvPolicy(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return Overrides{}, 0, nil
	}
	if err != nil {
		return Overrides{}, 0, fmt.Errorf("read the image update check policy of an environment: %w", err)
	}
	return Overrides{Enabled: boolPtr(row.Enabled), Schedule: stringPtr(row.Schedule), Preset: stringPtr(row.SchedulePreset)}, row.Version, nil
}

// Effective is the policy an environment follows now: its overrides over the defaults.
func (p PolicyStore) Effective(ctx context.Context, envID string) (Policy, error) {
	def, _, err := p.Defaults(ctx)
	if err != nil {
		return Policy{}, err
	}
	env, _, err := p.Env(ctx, envID)
	if err != nil {
		return Policy{}, err
	}
	return Effective(def, env), nil
}

// Followers is how many environments follow at least one instance default: those with no policy of their
// own and those whose policy leaves a field to the default.
func (p PolicyStore) Followers(ctx context.Context) (int, error) {
	n, err := p.Q.CountImageUpdateFollowers(ctx)
	if err != nil {
		return 0, fmt.Errorf("count the environments that follow an update check default: %w", err)
	}
	return int(n), nil
}

// SetEnv saves the overrides of an environment on the caller's transaction q, which also records the audit
// row. version is the one Env answered when the caller read the policy: a save made since is
// ErrVersionChanged and writes nothing. Saving what is already stored writes nothing and answers changed
// false. Every value passes Validate first. A nil field is stored as NULL, so it follows the default again.
func (p PolicyStore) SetEnv(ctx context.Context, q *sqlc.Queries, envID string, o Overrides, version int64, by string, now time.Time) (changed bool, err error) {
	if err := Validate(o); err != nil {
		return false, err
	}
	cur, curVersion, err := readEnv(ctx, q, envID)
	if err != nil {
		return false, err
	}
	if curVersion != version {
		return false, ErrVersionChanged
	}
	want := toRow(o)
	if toRow(cur) == want {
		return false, nil
	}
	at := now.UTC().UnixNano()
	if curVersion == 0 {
		n, err := q.InsertImageUpdateEnvPolicy(ctx, sqlc.InsertImageUpdateEnvPolicyParams{
			EnvID: envID, Enabled: want.enabled, Schedule: want.schedule, SchedulePreset: want.preset, UpdatedBy: by, UpdatedAt: at,
		})
		if err != nil {
			return false, fmt.Errorf("save the image update check policy of an environment: %w", err)
		}
		if n == 0 {
			return false, ErrVersionChanged
		}
		return true, nil
	}
	n, err := q.UpdateImageUpdateEnvPolicy(ctx, sqlc.UpdateImageUpdateEnvPolicyParams{
		Enabled: want.enabled, Schedule: want.schedule, SchedulePreset: want.preset, UpdatedBy: by, UpdatedAt: at, EnvID: envID, Version: version,
	})
	if err != nil {
		return false, fmt.Errorf("save the image update check policy of an environment: %w", err)
	}
	if n == 0 {
		return false, ErrVersionChanged
	}
	return true, nil
}

// SetDefaults saves the instance defaults on the caller's transaction q, which also records the audit row.
// version is the one Defaults answered when the caller read them: a save made since is ErrVersionChanged and
// writes nothing. Saving what is already stored writes nothing and answers changed false. Every value
// passes the checks an environment's do.
func (p PolicyStore) SetDefaults(ctx context.Context, q *sqlc.Queries, pol Policy, version int64, by string, now time.Time) (changed bool, err error) {
	if err := Validate(Overrides{Schedule: &pol.Schedule, Preset: &pol.Preset}); err != nil {
		return false, err
	}
	cur, curVersion, err := readDefaults(ctx, q)
	if err != nil {
		return false, err
	}
	if curVersion != version {
		return false, ErrVersionChanged
	}
	if cur == pol {
		return false, nil
	}
	n, err := q.UpdateImageUpdateDefaults(ctx, sqlc.UpdateImageUpdateDefaultsParams{
		Enabled: boolInt(pol.Enabled), Schedule: pol.Schedule, SchedulePreset: pol.Preset,
		UpdatedBy: by, UpdatedAt: now.UTC().UnixNano(), Version: version,
	})
	if err != nil {
		return false, fmt.Errorf("save the image update check defaults: %w", err)
	}
	if n == 0 {
		return false, ErrVersionChanged
	}
	return true, nil
}

// envRow is Overrides as the columns store them, comparable so that "nothing changed" is one ==.
type envRow struct {
	enabled          sql.NullInt64
	schedule, preset sql.NullString
}

func toRow(o Overrides) envRow {
	r := envRow{}
	if o.Enabled != nil {
		r.enabled = sql.NullInt64{Int64: boolInt(*o.Enabled), Valid: true}
	}
	if o.Schedule != nil {
		r.schedule = sql.NullString{String: *o.Schedule, Valid: true}
		// The two are set together: a schedule without a preset is custom.
		preset := schedule.PresetCustom
		if o.Preset != nil {
			preset = *o.Preset
		}
		r.preset = sql.NullString{String: preset, Valid: true}
	}
	return r
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func boolPtr(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}

func stringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}
