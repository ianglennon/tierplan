// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// The frequencies a schedule has.
const (
	FrequencyDaily  = "daily"
	FrequencyWeekly = "weekly"
)

// Defaults of a schedule that was never saved (D-32).
const (
	DefaultKeep    = 7
	DefaultTimeUTC = "03:00"
	// MaxKeep is the most backups a schedule keeps.
	MaxKeep = 365
)

// Errors of SaveSchedule.
var (
	// ErrInvalidSchedule means a field is out of range: the frequency, the weekday,
	// the time of day or the number of backups to keep.
	ErrInvalidSchedule = errors.New("the backup schedule is not valid")
	// ErrPassphraseRequired means the schedule would be on with no passphrase to
	// lock its backups, now or saved earlier.
	ErrPassphraseRequired = errors.New("a scheduled backup needs a passphrase")
)

// targetSchedule is the audit target of a change to the schedule.
const targetSchedule = "backup_schedule"

// Schedule is the saved backup schedule. The passphrase is never part of it: only
// whether one is saved.
type Schedule struct {
	Enabled bool
	// Frequency is "daily", or "weekly" on Weekday.
	Frequency string
	// Weekday is 0 (Sunday) to 6, used by a weekly schedule only.
	Weekday int
	// TimeUTC is the time of day as 24-hour "HH:MM", in UTC.
	TimeUTC         string
	Keep            int
	PassphraseSaved bool
	UpdatedAt       time.Time
}

// ScheduleInput is a change to the schedule. A nil or empty Passphrase keeps the
// one already saved.
type ScheduleInput struct {
	Enabled    bool
	Frequency  string
	Weekday    int
	TimeUTC    string
	Keep       int
	Passphrase *string
}

// defaultSchedule is what a schedule that was never saved reads as.
func defaultSchedule() Schedule {
	return Schedule{Frequency: FrequencyDaily, TimeUTC: DefaultTimeUTC, Keep: DefaultKeep}
}

func scheduleOf(row sqlc.BackupSchedule) Schedule {
	s := Schedule{
		Enabled:         row.Enabled != 0,
		Frequency:       row.Frequency,
		Weekday:         int(row.Weekday),
		TimeUTC:         row.TimeUtc,
		Keep:            int(row.Keep),
		PassphraseSaved: row.PassphraseSealed != "",
	}
	if row.UpdatedAt != 0 {
		s.UpdatedAt = time.Unix(0, row.UpdatedAt).UTC()
	}
	return s
}

// LoadSchedule reads the saved schedule, or the defaults (off) when it was never
// saved.
func LoadSchedule(ctx context.Context, st *store.Store) (Schedule, error) {
	row, err := st.RQ.GetBackupSchedule(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultSchedule(), nil
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("read the backup schedule: %w", err)
	}
	return scheduleOf(row), nil
}

// parseClock reads "HH:MM" (24-hour, zero-padded) as an hour and a minute.
func parseClock(s string) (hour, minute int, ok bool) {
	h, m, found := strings.Cut(s, ":")
	if !found || len(h) != 2 || len(m) != 2 {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

func (in ScheduleInput) validate() error {
	if in.Frequency != FrequencyDaily && in.Frequency != FrequencyWeekly {
		return fmt.Errorf("%w: frequency %q is not daily or weekly", ErrInvalidSchedule, in.Frequency)
	}
	if in.Weekday < 0 || in.Weekday > 6 {
		return fmt.Errorf("%w: weekday %d is not between 0 (Sunday) and 6", ErrInvalidSchedule, in.Weekday)
	}
	if _, _, ok := parseClock(in.TimeUTC); !ok {
		return fmt.Errorf("%w: time %q is not HH:MM between 00:00 and 23:59", ErrInvalidSchedule, in.TimeUTC)
	}
	if in.Keep < 1 || in.Keep > MaxKeep {
		return fmt.Errorf("%w: keep %d is not between 1 and %d", ErrInvalidSchedule, in.Keep, MaxKeep)
	}
	return nil
}

// SaveSchedule validates and stores the schedule and audits the change. A new
// passphrase is sealed under the vault with its own context, so it is re-wrapped by
// backups, restores and key rotation with every other sealed column; it is never
// written anywhere else. With no new passphrase the saved one stays. It returns
// ErrInvalidSchedule, ErrPassphraseTooShort (a new passphrase under the minimum) or
// ErrPassphraseRequired (on, with no passphrase at all).
func SaveSchedule(ctx context.Context, st *store.Store, v *vault.Vault, w *audit.Writer, actor audit.Actor, in ScheduleInput) (Schedule, error) {
	if err := in.validate(); err != nil {
		return Schedule{}, err
	}
	newPassphrase := in.Passphrase != nil && *in.Passphrase != ""
	if newPassphrase && utf8.RuneCountInString(*in.Passphrase) < MinPassphraseLen {
		return Schedule{}, ErrPassphraseTooShort
	}

	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, fmt.Errorf("save the backup schedule: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := st.Q.WithTx(tx)

	sealed := ""
	switch row, err := q.GetBackupSchedule(ctx); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Schedule{}, fmt.Errorf("read the saved backup schedule: %w", err)
	default:
		sealed = row.PassphraseSealed
	}
	if newPassphrase {
		if sealed, err = v.Seal(vault.ContextBackupSchedule, []byte(*in.Passphrase)); err != nil {
			return Schedule{}, fmt.Errorf("seal the schedule passphrase: %w", err)
		}
	}
	if in.Enabled && sealed == "" {
		return Schedule{}, ErrPassphraseRequired
	}

	enabled := int64(0)
	if in.Enabled {
		enabled = 1
	}
	now := time.Now().UTC()
	if err := q.UpsertBackupSchedule(ctx, sqlc.UpsertBackupScheduleParams{
		Enabled: enabled, Frequency: in.Frequency, Weekday: int64(in.Weekday), TimeUtc: in.TimeUTC,
		Keep: int64(in.Keep), PassphraseSealed: sealed, UpdatedAt: now.UnixNano(), UpdatedBy: actor.ID,
	}); err != nil {
		return Schedule{}, fmt.Errorf("save the backup schedule: %w", err)
	}
	if err := w.RecordTx(ctx, tx, audit.Entry{
		Actor:      actor,
		Action:     audit.ActionBackupScheduleUpdated,
		TargetType: targetSchedule,
		TargetID:   targetSchedule,
		Result:     audit.ResultSuccess,
		Details: map[string]string{
			"enabled": strconv.FormatBool(in.Enabled), "frequency": in.Frequency, "time_utc": in.TimeUTC, "keep": strconv.Itoa(in.Keep),
		},
	}); err != nil {
		return Schedule{}, fmt.Errorf("audit the backup schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Schedule{}, fmt.Errorf("save the backup schedule: %w", err)
	}
	return Schedule{
		Enabled: in.Enabled, Frequency: in.Frequency, Weekday: in.Weekday, TimeUTC: in.TimeUTC,
		Keep: in.Keep, PassphraseSaved: sealed != "", UpdatedAt: now,
	}, nil
}

// schedulePassphrase opens the saved passphrase. The plaintext lives only in the
// returned slice, which the caller clears when the backup is done.
func schedulePassphrase(ctx context.Context, st *store.Store, v *vault.Vault) ([]byte, error) {
	row, err := st.RQ.GetBackupSchedule(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the schedule passphrase: %w", err)
	}
	if row.PassphraseSealed == "" {
		return nil, ErrPassphraseRequired
	}
	plain, err := v.Open(vault.ContextBackupSchedule, row.PassphraseSealed)
	if err != nil {
		return nil, fmt.Errorf("open the schedule passphrase: %w", err)
	}
	return plain, nil
}
