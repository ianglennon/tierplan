// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"context"
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
