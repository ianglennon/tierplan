// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/vault"
)

// The saved backup schedule and its sealed passphrase (plan 03-27, D-32).

var adminActor = audit.Actor{Type: audit.ActorUser, ID: "u1", Name: "alice"}

func ptr[T any](v T) *T { return &v }

func dailyAt3(pass string) backup.ScheduleInput {
	return backup.ScheduleInput{Enabled: true, Frequency: "daily", TimeUTC: "03:00", Keep: 7, Passphrase: ptr(pass)}
}

func (f *exportFixture) saveSchedule(t *testing.T, in backup.ScheduleInput) (backup.Schedule, error) {
	t.Helper()
	return backup.SaveSchedule(context.Background(), f.st, f.live, f.aud, adminActor, in)
}

func TestAScheduleThatWasNeverSavedIsOffWithTheDefaults(t *testing.T) {
	f := newExportFixture(t)
	s, err := backup.LoadSchedule(context.Background(), f.st)
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled || s.Frequency != "daily" || s.TimeUTC != "03:00" || s.Keep != 7 || s.PassphraseSaved {
		t.Fatalf("schedule = %+v, want off, daily at 03:00, keep 7, no passphrase", s)
	}
}

func TestSavingTheScheduleSealsThePassphraseUnderItsOwnContextAndAuditsWithoutIt(t *testing.T) {
	f := newExportFixture(t)
	saved, err := f.saveSchedule(t, dailyAt3(exportPass))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Enabled || !saved.PassphraseSaved || saved.Keep != 7 {
		t.Fatalf("saved = %+v", saved)
	}
	loaded, err := backup.LoadSchedule(context.Background(), f.st)
	if err != nil || loaded.TimeUTC != "03:00" || !loaded.PassphraseSaved || !loaded.Enabled {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}

	var sealed string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule WHERE id = 1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || strings.Contains(sealed, exportPass) {
		t.Fatalf("the stored passphrase is %q", sealed)
	}
	plain, err := f.live.Open(vault.ContextBackupSchedule, sealed)
	if err != nil || string(plain) != exportPass {
		t.Fatalf("the passphrase does not open under its own context: %q, %v", plain, err)
	}
	if _, err := f.live.Open(vault.ContextAgentCA+"env-1", sealed); err == nil {
		t.Fatal("the passphrase opened under another context")
	}

	var actor, details string
	if err := f.st.Read.QueryRow(`SELECT actor_id, details FROM audit_log WHERE action = 'backup.schedule_updated'`).Scan(&actor, &details); err != nil {
		t.Fatalf("no backup.schedule_updated entry: %v", err)
	}
	if actor != "u1" || strings.Contains(details, exportPass) || !strings.Contains(details, `"time_utc":"03:00"`) || !strings.Contains(details, `"keep":"7"`) {
		t.Fatalf("audit actor %q, details %s", actor, details)
	}
}

func TestAWeeklyScheduleNamesItsDayAndIsKept(t *testing.T) {
	f := newExportFixture(t)
	in := dailyAt3(exportPass)
	in.Frequency, in.Weekday, in.TimeUTC, in.Keep = "weekly", 3, "23:45", 30
	saved, err := f.saveSchedule(t, in)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Frequency != "weekly" || saved.Weekday != 3 || saved.TimeUTC != "23:45" || saved.Keep != 30 {
		t.Fatalf("saved = %+v", saved)
	}
	loaded, err := backup.LoadSchedule(context.Background(), f.st)
	if err != nil || loaded.Frequency != "weekly" || loaded.Weekday != 3 {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
}

func TestAScheduleOutOfRangeIsRefusedAndNothingIsSaved(t *testing.T) {
	f := newExportFixture(t)
	for name, mutate := range map[string]func(*backup.ScheduleInput){
		"hour 25":            func(in *backup.ScheduleInput) { in.TimeUTC = "25:00" },
		"minute 60":          func(in *backup.ScheduleInput) { in.TimeUTC = "03:60" },
		"no leading zero":    func(in *backup.ScheduleInput) { in.TimeUTC = "3:00" },
		"seconds":            func(in *backup.ScheduleInput) { in.TimeUTC = "03:00:00" },
		"empty time":         func(in *backup.ScheduleInput) { in.TimeUTC = "" },
		"keep 0":             func(in *backup.ScheduleInput) { in.Keep = 0 },
		"keep 366":           func(in *backup.ScheduleInput) { in.Keep = 366 },
		"hourly":             func(in *backup.ScheduleInput) { in.Frequency = "hourly" },
		"weekday 7":          func(in *backup.ScheduleInput) { in.Frequency, in.Weekday = "weekly", 7 },
		"weekday -1":         func(in *backup.ScheduleInput) { in.Frequency, in.Weekday = "weekly", -1 },
		"empty frequency":    func(in *backup.ScheduleInput) { in.Frequency = "" },
		"weekday on a daily": func(in *backup.ScheduleInput) { in.Weekday = 9 },
	} {
		in := dailyAt3(exportPass)
		mutate(&in)
		if _, err := f.saveSchedule(t, in); !errors.Is(err, backup.ErrInvalidSchedule) {
			t.Errorf("%s: err = %v, want ErrInvalidSchedule", name, err)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM backup_schedule`).Scan(&n); err != nil || n != 0 {
		t.Errorf("backup_schedule rows = %d, %v after only refused saves", n, err)
	}
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'backup.schedule_updated'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("backup.schedule_updated entries = %d, %v after only refused saves", n, err)
	}
}

func TestSavingWithNoPassphraseKeepsTheSavedOneAndTurningItOnWithoutOneIsRefused(t *testing.T) {
	f := newExportFixture(t)

	off := dailyAt3("")
	off.Enabled, off.Passphrase = false, nil
	if _, err := f.saveSchedule(t, off); err != nil {
		t.Fatalf("saving a schedule that is off with no passphrase: %v", err)
	}
	on := off
	on.Enabled = true
	if _, err := f.saveSchedule(t, on); !errors.Is(err, backup.ErrPassphraseRequired) {
		t.Fatalf("turning it on with no passphrase ever saved: %v, want ErrPassphraseRequired", err)
	}
	empty := ptr("")
	on.Passphrase = empty
	if _, err := f.saveSchedule(t, on); !errors.Is(err, backup.ErrPassphraseRequired) {
		t.Fatalf("turning it on with an empty passphrase: %v, want ErrPassphraseRequired", err)
	}

	if _, err := f.saveSchedule(t, dailyAt3(exportPass)); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	keep := dailyAt3("")
	keep.Passphrase, keep.Keep = nil, 14
	saved, err := f.saveSchedule(t, keep)
	if err != nil || !saved.PassphraseSaved || saved.Keep != 14 {
		t.Fatalf("saving without a passphrase: %+v, %v", saved, err)
	}
	keep.Passphrase = empty
	if _, err := f.saveSchedule(t, keep); err != nil {
		t.Fatalf("saving with an empty passphrase: %v", err)
	}
	var after string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("saving without a passphrase changed the saved one")
	}
	if plain, err := f.live.Open(vault.ContextBackupSchedule, after); err != nil || string(plain) != exportPass {
		t.Errorf("the saved passphrase = %q, %v", plain, err)
	}

	if _, err := f.saveSchedule(t, dailyAt3("fifteen chars!!")); !errors.Is(err, backup.ErrPassphraseTooShort) {
		t.Errorf("a 16-character-minus-one passphrase: %v, want ErrPassphraseTooShort", err)
	}
	// A refused new passphrase leaves the saved one as it was.
	var still string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule`).Scan(&still); err != nil || still != before {
		t.Errorf("a refused save changed the saved passphrase: %v", err)
	}
}
