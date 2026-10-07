// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
)

// phase3DetailKeys maps every Phase 3 action to the detail keys the plans that
// record it must use. Recording each with exactly those keys proves none of them
// trips the secret-word filter (RESEARCH Pitfall 12): a key such as "token" or
// "session_key" would be refused and the action silently never audited.
var phase3DetailKeys = map[string][]string{
	audit.ActionUserCreated:                {"username", "link_kind", "display_name"},
	audit.ActionUserUpdated:                {"username", "fields"},
	audit.ActionUserDisabled:               {"username", "ended", "paused"},
	audit.ActionUserEnabled:                {"username"},
	audit.ActionUserDeleted:                {"username", "ended", "paused", "reassigned"},
	audit.ActionUserInviteCreated:          {"username", "expires_at"},
	audit.ActionUserInviteRegenerated:      {"username", "expires_at"},
	audit.ActionUserInviteAccepted:         {"username", "link_kind"},
	audit.ActionUserPasswordResetIssued:    {"username", "ended", "expires_at"},
	audit.ActionUserPasswordResetCompleted: {"username"},
	audit.ActionUserAdminGranted:           {"username"},
	audit.ActionUserAdminRevoked:           {"username"},
	audit.ActionSessionRevoked:             {"username", "handle", "device"},
	audit.ActionUserSessionsRevoked:        {"username", "ended"},
	audit.ActionTeamCreated:                {"name"},
	audit.ActionTeamRenamed:                {"name", "old_name"},
	audit.ActionTeamDeleted:                {"name", "members", "impact_count", "impact"},
	audit.ActionTeamMemberAdded:            {"team", "username"},
	audit.ActionTeamMemberRemoved:          {"team", "username", "impact_count", "impact"},
	audit.ActionAccessGranted:              {"subject_type", "subject", "role", "scope", "extras"},
	audit.ActionAccessUpdated:              {"subject_type", "subject", "role", "scope", "extras"},
	audit.ActionAccessRevoked:              {"subject_type", "subject", "role", "scope", "impact_count", "impact"},
	audit.ActionOwnerTransferred:           {"kind", "resource", "from", "to"},
	audit.ActionAutomationPaused:           {"kind", "resource", "state", "owner"},
	audit.ActionBackupExported:             {"file", "size", "source"},
	audit.ActionBackupScheduledRun:         {"file", "size"},
	audit.ActionBackupFailed:               {"stage", "error"},
	audit.ActionBackupChecked:              {"format", "schema", "app_version"},
	audit.ActionBackupRestored:             {"source_instance", "backup_created", "app_version", "schema", "source"},
	audit.ActionBackupScheduleUpdated:      {"enabled", "frequency", "time_utc", "keep"},
	audit.ActionAuditReanchored:            {"source_instance", "backup_created", "old_kid", "old_head", "entries", "chain_verified"},
	audit.ActionKeyRotated:                 {"old_kid", "new_kid", "old_head", "entries", "attestation"},
}

func TestEveryPhase3ActionIsDefinedAndItsDetailKeysAreRecordable(t *testing.T) {
	if got := len(phase3DetailKeys); got != 32 {
		t.Fatalf("the table covers %d actions, want the 32 of the Phase 3 vocabulary", got)
	}
	f := newFixture(t)
	for action, keys := range phase3DetailKeys {
		details := make(map[string]string, len(keys))
		for _, k := range keys {
			details[k] = "x"
		}
		e := ent(action)
		e.Details = details
		if err := f.w.Record(context.Background(), e); err != nil {
			if errors.Is(err, audit.ErrSecretField) {
				t.Errorf("%s: a documented detail key trips the secret-word filter: %v", action, err)
			} else {
				t.Errorf("%s: Record: %v", action, err)
			}
		}
	}
	if got := f.verify(t); got.Broken != nil || got.Entries != 32 {
		t.Fatalf("verify = %+v", got)
	}
}

func TestPhase3ActionKeysHaveTheirDocumentedValues(t *testing.T) {
	for key, constant := range map[string]string{
		"user.created": audit.ActionUserCreated, "user.invite_regenerated": audit.ActionUserInviteRegenerated,
		"user.password_reset_completed": audit.ActionUserPasswordResetCompleted, "session.revoked": audit.ActionSessionRevoked,
		"user.sessions_revoked": audit.ActionUserSessionsRevoked, "team.member_removed": audit.ActionTeamMemberRemoved,
		"access.revoked": audit.ActionAccessRevoked, "owner.transferred": audit.ActionOwnerTransferred,
		"automation.paused": audit.ActionAutomationPaused, "backup.scheduled_run": audit.ActionBackupScheduledRun,
		"backup.schedule_updated": audit.ActionBackupScheduleUpdated, "audit.reanchored": audit.ActionAuditReanchored,
		"key.rotated": audit.ActionKeyRotated,
	} {
		if key != constant {
			t.Errorf("constant = %q, want %q", constant, key)
		}
	}
}
