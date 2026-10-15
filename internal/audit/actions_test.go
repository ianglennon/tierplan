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
	// 04-13 records the name, node and scope, and the code of a refusal. A network, secret or config
	// made or removed by a service apply also records its source, service_apply or
	// service_apply_cleanup (plan 06-26).
	audit.ActionNetworkCreated: {"name", "node", "driver", "scope", "subnet", "gateway", "internal", "attachable", "source", "code"},
	audit.ActionNetworkRemoved: {"name", "node", "driver", "scope", "source", "code"},
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
	// and what was asked for; never any line of the log (plan 04-19, D-23, D-24). A service's
	// log records the service in place of the container, and the task and node filters
	// (plan 06-29, D-35).
	audit.ActionLogsViewed: {"container", "service", "node", "task", "sid", "since", "tail"},
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
	// A cancel of a pull by someone other than the person who started it is its own row, with the
	// canceller as the actor, the job ID as bulk and the starter named (WR-B04).
	audit.ActionImagePullCancelled: {"ref", "bulk", "started_by"},
	// A remove or an untag records one row per node: the image's name as it was shown, the node,
	// the code of a refusal and the bulk ID (plan 05-16, D-31).
	audit.ActionImageRemoved:  {"ref", "node", "code", "bulk"},
	audit.ActionImageUntagged: {"ref", "node", "code", "bulk"},
	// A credential row names what was stored and never the secret or any part of it: the
	// name, the layer, the host and prefix, the type, the username, whether it is the Docker
	// Hub fallback and the expiry day (plan 05-05, D-31).
	audit.ActionRegistryCredentialCreated: {"name", "layer", "host", "prefix", "type", "username", "hub_fallback", "expires"},
	// An update adds the names of the fields that changed, comma-joined, the secret only as
	// the word secret (plan 05-07). A change of host also records where it pointed before and now.
	audit.ActionRegistryCredentialUpdated: {"name", "layer", "host", "prefix", "type", "username", "hub_fallback", "expires", "changed", "host_from", "host_to"},
	audit.ActionRegistryCredentialDeleted: {"name", "layer", "host", "prefix", "type", "username", "hub_fallback", "expires"},
	// A test records where it ran from (via: the server, or a node), the registry and
	// type, each step's result, the code of the first failure, the repository tried and, from
	// an environment, the node. Never the secret (plan 05-11, D-12, D-31).
	audit.ActionRegistryCredentialTested: {"name", "layer", "host", "prefix", "type", "reachable", "signed_in", "can_pull", "code", "via", "repository", "node"},
}

// phase6DetailKeys maps every Phase 6 action to the detail keys the plans that record it must
// use, for the same reason as phase3DetailKeys. A scale records one row per service: its name, the new
// replica count, the count it had, the code of a refusal, the bulk ID of a selection scaled at
// once and the node the manager answered on (plan 06-01, RES-02). A force update records the image
// the service was left with, its digest, whether it was pinned, whether it was a retry and the
// code; a rollback records the version it was asked at and the code (plan 06-08, RES-02). A remove records the name, the stack the service belonged to, whether it was
// Bayplan's agent service, whether its name was typed to confirm, and the code (plan 06-10, RES-02).
var phase6DetailKeys = map[string][]string{
	audit.ActionServiceScaled:       {"name", "replicas", "previous", "code", "bulk", "node"},
	audit.ActionServiceForceUpdated: {"name", "image", "digest", "retry", "pinned", "code"},
	audit.ActionServiceRolledBack:   {"name", "version", "code"},
	audit.ActionServiceRemoved:      {"name", "stack", "agent", "confirmed", "code"},
	// An edit records the version it was applied at, the image the service was left with and its digest,
	// whether the spec changed, how many host-escalation findings it had and the code; never a value of
	// the spec (plan 06-18, RES-03).
	audit.ActionServiceUpdated: {"name", "version", "image", "digest", "changed", "findings", "code"},
	// A create records the name, the image the service was made with and its digest, how many objects
	// were made for it and the code; never a value of the spec (plan 06-27, RES-03).
	audit.ActionServiceCreated: {"name", "image", "digest", "objects", "code"},
	// An availability or role change records the node's name, the value it had and the one asked for,
	// whether its name was typed to confirm, the warnings the change carried (comma separated) and the
	// code (plan 06-15, RES-05).
	audit.ActionNodeAvailabilityChanged: {"name", "from", "to", "confirmed", "warnings", "code"},
	audit.ActionNodeRoleChanged:         {"name", "from", "to", "confirmed", "warnings", "code"},
	// A label change records the node's name, how many labels were added, changed and removed, how
	// many services have a placement constraint the change affects and the code (plan 06-16, RES-05).
	audit.ActionNodeLabelsChanged: {"name", "added", "changed", "removed", "affected", "code"},
	// A node remove records the node's host name, the agent that was revoked with it (empty when no
	// agent ran on the node) and the code (plan 06-17, RES-05).
	audit.ActionNodeRemoved: {"hostname", "agent", "code"},
	// A secret create records the name, how many labels it has, the size of the value in bytes, where
	// the request came from (typed, file, rotate, apply or api) and the code; never the value. A
	// remove records the name and the code (plan 06-22, RES-08).
	audit.ActionSecretCreated: {"name", "labels", "size", "source", "code"},
	audit.ActionSecretRemoved: {"name", "source", "code"},
	// A config create records the name, how many labels it has, the size of the content in bytes, the
	// templating driver (none or golang), where the request came from (typed, file, rotate, apply or api)
	// and the code. A remove records the name and the code (plan 06-24, RES-08).
	audit.ActionConfigCreated: {"name", "labels", "size", "templating", "source", "code"},
	audit.ActionConfigRemoved: {"name", "source", "code"},
	// A rotate records the name of the object it replaced and of the new one, how many services it moved
	// and skipped, whether the old object was removed and the code; never a value (plan 06-28, RES-08).
	audit.ActionSecretRotated: {"from", "to", "updated", "skipped", "removed_old", "code"},
	audit.ActionConfigRotated: {"from", "to", "updated", "skipped", "removed_old", "code"},
}

func TestEveryPhase6ActionIsDefinedAndItsDetailKeysAreRecordable(t *testing.T) {
	f := newFixture(t)
	for action, keys := range phase6DetailKeys {
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
	if got := f.verify(t); got.Broken != nil || got.Entries != len(phase6DetailKeys) {
		t.Fatalf("verify = %+v", got)
	}
	for key, constant := range map[string]string{
		"service.scaled":        audit.ActionServiceScaled,
		"service.force_updated": audit.ActionServiceForceUpdated,
		"service.rolled_back":   audit.ActionServiceRolledBack,
		"service.removed":       audit.ActionServiceRemoved,
		"service.updated":       audit.ActionServiceUpdated,
		"service.created":       audit.ActionServiceCreated,

		"node.availability_changed": audit.ActionNodeAvailabilityChanged,
		"node.role_changed":         audit.ActionNodeRoleChanged,
		"node.labels_changed":       audit.ActionNodeLabelsChanged,
		"node.removed":              audit.ActionNodeRemoved,

		"secret.created": audit.ActionSecretCreated,
		"secret.removed": audit.ActionSecretRemoved,
		"config.created": audit.ActionConfigCreated,
		"config.removed": audit.ActionConfigRemoved,

		"secret.rotated": audit.ActionSecretRotated,
		"config.rotated": audit.ActionConfigRotated,
	} {
		if key != constant {
			t.Errorf("constant = %q, want %q", constant, key)
		}
	}
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
	for key, constant := range map[string]string{
		"image.pulled":                audit.ActionImagePulled,
		"image.pull_cancelled":        audit.ActionImagePullCancelled,
		"image.removed":               audit.ActionImageRemoved,
		"image.untagged":              audit.ActionImageUntagged,
		"registry.credential_created": audit.ActionRegistryCredentialCreated,
		"registry.credential_updated": audit.ActionRegistryCredentialUpdated,
		"registry.credential_deleted": audit.ActionRegistryCredentialDeleted,
		"registry.credential_tested":  audit.ActionRegistryCredentialTested,
	} {
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
