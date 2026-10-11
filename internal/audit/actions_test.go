// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	audit.ActionAutomationResumed:          {"kind", "resource", "state", "owner"},
	audit.ActionBackupExported:             {"file", "size", "source"},
	audit.ActionBackupScheduledRun:         {"file", "size"},
	audit.ActionBackupFailed:               {"stage", "error"},
	audit.ActionBackupChecked:              {"format", "schema", "app_version"},
	audit.ActionBackupRestored:             {"source_instance", "backup_created", "app_version", "schema", "source"},
	audit.ActionBackupScheduleUpdated:      {"enabled", "frequency", "time_utc", "keep"},
	audit.ActionBackupUploadDiscarded:      {"source"},
	audit.ActionAuditReanchored:            {"source_instance", "backup_created", "old_kid", "old_head", "entries", "chain_verified"},
	audit.ActionKeyRotated:                 {"old_kid", "new_kid", "old_head", "entries", "attestation"},
}

func TestEveryPhase3ActionIsDefinedAndItsDetailKeysAreRecordable(t *testing.T) {
	if got := len(phase3DetailKeys); got != 34 {
		t.Fatalf("the table covers %d actions, want the 34 of the Phase 3 vocabulary", got)
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
	if got := f.verify(t); got.Broken != nil || got.Entries != 34 {
		t.Fatalf("verify = %+v", got)
	}
}

// phase4DetailKeys maps every Phase 4 action to the detail keys the plans that
// record it must use, for the same reason as phase3DetailKeys. The union of keys
// is shared by the container actions: container, node and code always; bulk, stage,
// own, timeout and signal when the request carried them.
var phase4DetailKeys = map[string][]string{
	audit.ActionContainerStarted: {"container", "node", "code", "bulk", "stage", "own", "timeout", "signal"},
	audit.ActionContainerStopped: {"container", "node", "code", "bulk", "stage", "own", "timeout", "signal"},
	// restart and kill take the same timeout and signal as a stop; remove takes
	// force and volumes (plan 04-05).
	audit.ActionContainerRestarted: {"container", "node", "code", "bulk", "stage", "own", "timeout", "signal"},
	audit.ActionContainerKilled:    {"container", "node", "code", "bulk", "stage", "own", "signal"},
	audit.ActionContainerPaused:    {"container", "node", "code", "bulk", "stage", "own"},
	audit.ActionContainerUnpaused:  {"container", "node", "code", "bulk", "stage", "own"},
	audit.ActionContainerRemoved:   {"container", "node", "code", "bulk", "stage", "own", "force", "volumes"},
	// A network create records what was asked and where it landed; the remove of plan
	// 04-13 records the name, node and scope, and the code of a refusal.
	audit.ActionNetworkCreated: {"name", "node", "driver", "scope", "subnet", "gateway", "internal", "attachable", "code"},
	audit.ActionNetworkRemoved: {"name", "node", "driver", "scope", "code"},
	// Connecting a container records the network, the container, its node and, when asked
	// for, the alias and addresses; disconnecting the first three.
	audit.ActionNetworkConnected:    {"network", "container", "node", "alias", "ipv4", "ipv6", "code"},
	audit.ActionNetworkDisconnected: {"network", "container", "node", "code"},
	// A volume create records the name, node, driver and the driver options with any
	// password-like part masked (redact.AuditOptions); the remove of plan 04-17 records
	// the name, node, whether it was part of a bulk delete, and the code.
	audit.ActionVolumeCreated: {"name", "node", "driver", "options", "code"},
	audit.ActionVolumeRemoved: {"name", "node", "driver", "bulk", "code"},
	// Opening a log stream records the container, its node, the viewer's random ID (sid)
	// and what was asked for; never any line of the log (plan 04-19, D-23, D-24).
	audit.ActionLogsViewed: {"container", "node", "sid", "since", "tail"},
	// A browser shell records that a session began and how it ended, never what was
	// typed or shown (plan 04-22, D-28): the container, its node, the session's ID
	// (sid), the command as chosen, the user and working directory (empty for the
	// container's own), then at the end the time it lasted, the exit code or that the
	// process was still running, and why it ended. owner names whose session an admin
	// ended (plan 04-25).
	audit.ActionShellOpened: {"container", "node", "sid", "command", "user", "workdir"},
	audit.ActionShellClosed: {"container", "node", "sid", "duration_ms", "exit_code", "still_running", "reason", "owner"},
	// An administrator ending someone else's shell is its own row, with the admin as the
	// actor and the session's owner named (plan 04-25, D-37).
	audit.ActionShellEnded: {"container", "node", "sid", "owner"},
}

// phase5DetailKeys maps every Phase 5 action to the detail keys the plans that record
// it must use, for the same reason as phase3DetailKeys. A pull records one row per node:
// the normalised reference, the digest it resolved to, the node, which credential was used
// (anonymous until a credential is chosen), the code of a refusal, the layer a failure
// stopped on and the job ID as bulk (plan 05-01, D-31).
var phase5DetailKeys = map[string][]string{
	audit.ActionImagePulled: {"ref", "digest", "node", "credential", "layer", "code", "bulk"},
}

func TestEveryPhase5ActionIsDefinedAndItsDetailKeysAreRecordable(t *testing.T) {
	f := newFixture(t)
	for action, keys := range phase5DetailKeys {
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
	if got := f.verify(t); got.Broken != nil || got.Entries != len(phase5DetailKeys) {
		t.Fatalf("verify = %+v", got)
	}
	for key, constant := range map[string]string{"image.pulled": audit.ActionImagePulled} {
		if key != constant {
			t.Errorf("constant = %q, want %q", constant, key)
		}
	}
}

func TestEveryPhase4ActionIsDefinedAndItsDetailKeysAreRecordable(t *testing.T) {
	f := newFixture(t)
	for action, keys := range phase4DetailKeys {
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
	if got := f.verify(t); got.Broken != nil || got.Entries != len(phase4DetailKeys) {
		t.Fatalf("verify = %+v", got)
	}
	for key, constant := range map[string]string{
		"container.started": audit.ActionContainerStarted, "container.stopped": audit.ActionContainerStopped,
		"container.restarted": audit.ActionContainerRestarted, "container.killed": audit.ActionContainerKilled,
		"container.paused": audit.ActionContainerPaused, "container.unpaused": audit.ActionContainerUnpaused,
		"container.removed": audit.ActionContainerRemoved,
		"network.created":   audit.ActionNetworkCreated, "network.removed": audit.ActionNetworkRemoved,
		"network.connected": audit.ActionNetworkConnected, "network.disconnected": audit.ActionNetworkDisconnected,
		"volume.created": audit.ActionVolumeCreated, "volume.removed": audit.ActionVolumeRemoved,
		"logs.viewed": audit.ActionLogsViewed, "logs.downloaded": audit.ActionLogsDownloaded,
		"shell.opened": audit.ActionShellOpened, "shell.closed": audit.ActionShellClosed, "shell.ended": audit.ActionShellEnded,
	} {
		if key != constant {
			t.Errorf("constant = %q, want %q", constant, key)
		}
	}
}

func TestPhase3ActionKeysHaveTheirDocumentedValues(t *testing.T) {
	for key, constant := range map[string]string{
		"user.created": audit.ActionUserCreated, "user.invite_regenerated": audit.ActionUserInviteRegenerated,
		"user.password_reset_completed": audit.ActionUserPasswordResetCompleted, "session.revoked": audit.ActionSessionRevoked,
		"user.sessions_revoked": audit.ActionUserSessionsRevoked, "team.member_removed": audit.ActionTeamMemberRemoved,
		"access.revoked": audit.ActionAccessRevoked, "owner.transferred": audit.ActionOwnerTransferred,
		"automation.paused": audit.ActionAutomationPaused, "automation.resumed": audit.ActionAutomationResumed, "backup.scheduled_run": audit.ActionBackupScheduledRun,
		"backup.schedule_updated": audit.ActionBackupScheduleUpdated, "audit.reanchored": audit.ActionAuditReanchored,
		"key.rotated": audit.ActionKeyRotated,
	} {
		if key != constant {
			t.Errorf("constant = %q, want %q", constant, key)
		}
	}
}

// definedActions lists the value of every Action* string constant of audit.go.
func definedActions(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "audit.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, _ := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, v)
			}
		}
	}
	return out
}

// Every action the backend writes has a label on the audit page and a place in
// its Action filter. A key the page does not know still renders, as the raw key,
// but a delegated admin reading the log should never be handed one (UI-SPEC,
// Audit log changes). The page's list is a TypeScript file, so this reads it.
func TestEveryDefinedActionIsLabelledAndFilterableOnTheAuditPage(t *testing.T) {
	actions := definedActions(t)
	if len(actions) < 60 {
		t.Fatalf("found %d Action constants, expected the whole vocabulary: the parse is wrong", len(actions))
	}

	ts, err := os.ReadFile("../../web/src/features/audit/filters.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)KNOWN_ACTIONS: readonly string\[\] = \[(.*?)\];`).FindSubmatch(ts)
	if block == nil {
		t.Fatal("KNOWN_ACTIONS not found in filters.ts")
	}
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_.]+)"`).FindAllSubmatch(block[1], -1) {
		known[string(m[1])] = true
	}

	raw, err := os.ReadFile("../../web/src/i18n/en/audit.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Audit struct {
			Actions map[string]map[string]string `json:"actions"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	for _, action := range actions {
		area, name, _ := strings.Cut(action, ".")
		if !known[action] {
			t.Errorf("%s is not in KNOWN_ACTIONS (web/src/features/audit/filters.ts)", action)
		}
		if doc.Audit.Actions[area][name] == "" {
			t.Errorf("%s has no label in web/src/i18n/en/audit.json", action)
		}
	}
}
