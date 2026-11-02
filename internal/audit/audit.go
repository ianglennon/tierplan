// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package audit is TierPlan's tamper-evident record of what happened (AUTH-08,
// D-39 to D-42).
package audit

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// ActorType says what kind of principal acted.
type ActorType string

// Actor types.
const (
	ActorUser      ActorType = "user"
	ActorSystem    ActorType = "system"
	ActorAnonymous ActorType = "anonymous"
)

// Actor is who performed an action.
type Actor struct {
	Type ActorType
	ID   string
	Name string
}

// System principals (D-51).
var (
	SystemCLI       = Actor{Type: ActorSystem, ID: "system:cli", Name: "system:cli"}
	SystemRetention = Actor{Type: ActorSystem, ID: "system:audit-retention", Name: "system:audit-retention"}
	SystemRestore   = Actor{Type: ActorSystem, ID: "system:restore", Name: "system:restore"}
	SystemBackup    = Actor{Type: ActorSystem, ID: "system:backup", Name: "system:backup"}
)

// Result is the outcome of an action.
type Result string

// Results.
const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
	ResultDenied  Result = "denied"
)

// Action keys. The backend owns this list; the UI maps each to a label.
const (
	ActionAuthLogin           = "auth.login"
	ActionAuthLoginFailed     = "auth.login_failed"
	ActionAuthLocked          = "auth.locked"
	ActionAuthUnlocked        = "auth.unlocked"
	ActionAuthLogout          = "auth.logout"
	ActionAuthPasswordChanged = "auth.password_changed"
	ActionSetupTokenFailed    = "setup.token_failed"
	ActionSetupCompleted      = "setup.completed"
	ActionSetupAttemptDenied  = "setup.attempt_denied"
	ActionSettingsUpdated     = "settings.updated"
	ActionEnvironmentAdded    = "environment.added"
	ActionEnvironmentRenamed  = "environment.renamed"
	ActionEnvironmentRemoved  = "environment.removed"
	ActionAgentEnrolled       = "agent.enrolled"
	ActionAgentEnrolRefused   = "agent.enrol_refused"
	ActionAgentCertRenewed    = "agent.cert_renewed"
	ActionAgentRevoked        = "agent.revoked"
	ActionAgentBlocked        = "agent.blocked"
	ActionAgentUnblocked      = "agent.unblocked"
	ActionAgentAutoRevoked    = "agent.auto_revoked"
	ActionAgentUpdated        = "agent.updated"
	ActionAgentUpdateFailed   = "agent.update_failed"
	ActionAgentUninstalled    = "agent.uninstalled"
	ActionAgentPolicyDenied   = "agent.policy_denied"

	ActionEnvironmentTokenRotated      = "environment.token_rotated"
	ActionEnvironmentTokenDisabled     = "environment.token_disabled"
	ActionEnvironmentFailover          = "environment.failover"
	ActionEnvironmentClusterAdopted    = "environment.cluster_adopted"
	ActionEnvironmentLocalFolded       = "environment.local_folded"
	ActionEnvironmentAutoUpdateChanged = "environment.auto_update_changed"
	// ActionEnvironmentRestartLoopSettingsChanged is a change of the failures and the window
	// that make a restart loop in one environment (plan 06-35, RES-13).
	ActionEnvironmentRestartLoopSettingsChanged = "environment.restart_loop_settings_changed"
	// ActionEnvironmentSmallSubnetsChanged is a change of the opt-in to small stack subnets, or
	// of their size, in one environment (plan 10-21, D-21).
	ActionEnvironmentSmallSubnetsChanged = "environment.small_subnets_changed"
	// ActionEnvironmentAddressPoolsSaved is a save of the address pools a person entered for a
	// standalone host (plan 10-21, D-07 as amended).
	ActionEnvironmentAddressPoolsSaved = "environment.address_pools_saved"

	ActionWarningAcknowledged = "warning.acknowledged"
	ActionAuditPruned         = "audit.pruned"
	ActionAuditReanchored     = "audit.reanchored"
	ActionPermissionDenied    = "permission.denied"

	ActionUserCreated                = "user.created"
	ActionUserUpdated                = "user.updated"
	ActionUserDisabled               = "user.disabled"
	ActionUserEnabled                = "user.enabled"
	ActionUserDeleted                = "user.deleted"
	ActionUserInviteCreated          = "user.invite_created"
	ActionUserInviteRegenerated      = "user.invite_regenerated"
	ActionUserInviteAccepted         = "user.invite_accepted"
	ActionUserPasswordResetIssued    = "user.password_reset_issued"
	ActionUserPasswordResetCompleted = "user.password_reset_completed"
	ActionUserAdminGranted           = "user.admin_granted"
	ActionUserAdminRevoked           = "user.admin_revoked"
	ActionSessionRevoked             = "session.revoked"
	ActionUserSessionsRevoked        = "user.sessions_revoked"
	ActionTeamCreated                = "team.created"
	ActionTeamRenamed                = "team.renamed"
	ActionTeamDeleted                = "team.deleted"
	ActionTeamMemberAdded            = "team.member_added"
	ActionTeamMemberRemoved          = "team.member_removed"
	ActionAccessGranted              = "access.granted"
	ActionAccessUpdated              = "access.updated"
	ActionAccessRevoked              = "access.revoked"
	ActionOwnerTransferred           = "owner.transferred"
	ActionAutomationPaused           = "automation.paused"
	ActionAutomationResumed          = "automation.resumed"
	ActionBackupExported             = "backup.exported"
	ActionBackupScheduledRun         = "backup.scheduled_run"
	ActionBackupFailed               = "backup.failed"
	ActionBackupChecked              = "backup.checked"
	ActionBackupRestored             = "backup.restored"
	ActionBackupScheduleUpdated      = "backup.schedule_updated"
	ActionBackupUploadDiscarded      = "backup.upload_discarded"
	ActionKeyRotated                 = "key.rotated"
	ActionContainerStarted           = "container.started"
	ActionContainerStopped           = "container.stopped"
	ActionContainerRestarted         = "container.restarted"
	ActionContainerKilled            = "container.killed"
	ActionContainerPaused            = "container.paused"
	ActionContainerUnpaused          = "container.unpaused"
	ActionContainerRemoved           = "container.removed"
	ActionNetworkCreated             = "network.created"
	ActionNetworkRemoved             = "network.removed"
	ActionNetworkInspectSaved        = "network.inspect_saved" // the whole masked inspect of a network, saved before it is removed (D-12)
	ActionNetworkKept                = "network.kept"          // a person asked never to list a network as orphaned or unused (D-11)
	ActionNetworkUnkept              = "network.unkept"        // and took it back
	ActionNetworkConnected           = "network.connected"
	ActionNetworkDisconnected        = "network.disconnected"
	ActionVolumeCreated              = "volume.created"
	ActionVolumeRemoved              = "volume.removed"
	ActionImagePulled                = "image.pulled"
	ActionImageRemoved               = "image.removed"
	ActionImageUntagged              = "image.untagged"
	ActionImagePullCancelled         = "image.pull_cancelled"
	ActionRegistryCredentialCreated  = "registry.credential_created" //nolint:gosec // G101: an action name, not a credential
	ActionRegistryCredentialUpdated  = "registry.credential_updated" //nolint:gosec // G101: an action name, not a credential
	ActionRegistryCredentialDeleted  = "registry.credential_deleted" //nolint:gosec // G101: an action name, not a credential
	ActionRegistryCredentialTested   = "registry.credential_tested"  //nolint:gosec // G101: an action name, not a credential
	ActionLogsViewed                 = "logs.viewed"
	ActionLogsDownloaded             = "logs.downloaded"
	ActionShellOpened                = "shell.opened"
	ActionShellClosed                = "shell.closed"
	ActionShellEnded                 = "shell.ended"
	ActionServiceScaled              = "service.scaled"
	ActionServiceForceUpdated        = "service.force_updated"
	ActionServiceRolledBack          = "service.rolled_back"
	ActionServiceRemoved             = "service.removed"
	ActionServiceUpdated             = "service.updated"
	ActionServiceCreated             = "service.created"
	ActionRestartLoopMuted           = "restart_loop.muted"
	ActionRestartLoopUnmuted         = "restart_loop.unmuted"
	ActionNodeAvailabilityChanged    = "node.availability_changed"
	ActionNodeRoleChanged            = "node.role_changed"
	ActionNodeLabelsChanged          = "node.labels_changed"
	ActionNodeRemoved                = "node.removed"
	ActionSecretCreated              = "secret.created"
	ActionSecretRemoved              = "secret.removed"
	ActionConfigCreated              = "config.created"
	ActionConfigRemoved              = "config.removed"
	ActionSecretRotated              = "secret.rotated"
	ActionConfigRotated              = "config.rotated"
	ActionStackCreated               = "stack.created"
	ActionStackCreatedFromGit        = "stack.created_from_git"
	ActionStackPointedAtGit          = "stack.pointed_at_git"
	ActionStackDraftSaved            = "stack.draft_saved"
	ActionStackDraftDiscarded        = "stack.draft_discarded"
	ActionStackDeployed              = "stack.deployed"
	ActionStackStopped               = "stack.stopped"
	ActionStackStarted               = "stack.started"
	ActionStackRemoved               = "stack.removed"
	ActionStackValueRevealed         = "stack.value_revealed"
	ActionStackAdopted               = "stack.adopted"
	ActionStackDriftAccepted         = "stack.drift_accepted"
	ActionGitCredentialCreated       = "git.credential_created" //nolint:gosec // G101: an action name, not a credential
	ActionGitCredentialUpdated       = "git.credential_updated" //nolint:gosec // G101: an action name, not a credential
	ActionGitCredentialDeleted       = "git.credential_deleted" //nolint:gosec // G101: an action name, not a credential
	ActionGitRepoCreated             = "git.repo_created"
	ActionGitRepoUpdated             = "git.repo_updated"
	ActionGitRepoDeleted             = "git.repo_deleted"
	ActionGitRepoTested              = "git.repo_tested"
	ActionGitHostKeyTrusted          = "git.host_key_trusted"
	ActionGitHostKeyRetrusted        = "git.host_key_retrusted"
	// The sync check of a git stack went into error, or into a different error (D-16). A repeat of the error already
	// recorded writes nothing.
	ActionGitSyncFailed = "git.sync_failed"
	// A person turned a git stack's polling on or off, or changed how often it is checked (D-14).
	ActionGitUpdatesChanged = "git.updates_changed"
	// A webhook accepted a delivery and asked for a check, and a request to a webhook that exists failed its signature.
	// A request to an address that is no webhook writes nothing (D-15). Neither carries the address, the token, the
	// secret or the signature.
	ActionGitWebhookDelivered = "git.webhook_delivered"
	ActionGitWebhookRejected  = "git.webhook_rejected"
	// A person gave a git stack a webhook, replaced its token and secret, or removed it (D-15). The entry names the stack
	// and the git host, never the address, the token or the secret.
	ActionGitWebhookCreated = "git.webhook_created"
	ActionGitWebhookRotated = "git.webhook_rotated"
	ActionGitWebhookRemoved = "git.webhook_removed"
	// A person turned a git stack's automatic deploys on or off (D-18). The entry names the stack.
	ActionGitAutoDeployEnabled  = "git.autodeploy_enabled"
	ActionGitAutoDeployDisabled = "git.autodeploy_disabled"
	// TierPlan deployed a new commit of a git stack by itself, on the authority of the person who turned auto-deploy on
	// (D-20). The actor is the system principal that ran it; the entry names the stack, the commit, what started it (poll
	// or webhook) and the owner.
	ActionGitAutoDeployed = "git.autodeployed"
	// A roll back paused a git stack's automatic deploys, so the repository head is not deployed over the restored
	// revision (D-22). The actor is the person who rolled back; the entry names the stack, the reason (rollback) and the
	// number of the revision restored.
	ActionGitAutoDeployPaused = "git.autodeploy_paused"
	// An Admin of the environment detached a git stack from its repository, so it is an inline stack again (D-25). The entry
	// names the stack, the number of the revision whose files became the draft and its commit, never a path or content.
	ActionGitStackDetached = "git.stack_detached"
	// A person pulled the head of a git stack's ref into its draft. The entry names the stack, the repository, the ref,
	// the folder, the commit that was read and the draft's version; it deploys nothing.
	ActionGitPulled = "git.pulled"
	// An Admin of the environment pointed a git stack at another repository, ref, folder or compose files, as a pending
	// source for review. The entry names the stack, the new repository, ref kind, ref, folder and commit and the ones it
	// replaces; the next good deploy of the draft makes it the stack's source.
	ActionGitSourceChanged = "git.source_changed"
	// One summary row per finished janitor run, scheduled or started by a person, with what it removed, kept, failed and
	// skipped (D-16). The environment is the target; the actor is the person, or the system principal for a scheduled run.
	ActionJanitorCleaned = "janitor.cleaned"
)

// ActionAuthLinkRefused is an anonymous visitor's refused use of an invite or
// reset link: an unusable link or the per-address limit. It never carries the
// link.
const ActionAuthLinkRefused = "auth.link_refused"

// Errors returned by Record.
var (
	// ErrSecretField means a detail key names a secret; nothing was written.
	ErrSecretField = errors.New("audit: detail key names a secret")
	// ErrInvalidEntry means a required field is missing or unknown.
	ErrInvalidEntry = errors.New("audit: invalid entry")
	// ErrEvidenceTooLarge means an evidence detail is longer than MaxEvidenceValue. Evidence is
	// stored whole or refused, never cut; nothing was written (D-12).
	ErrEvidenceTooLarge = errors.New("audit: evidence detail too large")
)

// metaLastPruned is the app_meta key holding the hex HMAC of the newest entry
// that retention deleted. It seeds the chain if the table is ever empty.
const metaLastPruned = "audit_last_pruned_hmac"

// Length caps for attacker-controlled strings (RESEARCH Pitfall 19). They are
// byte limits; truncation never splits a UTF-8 sequence.
const (
	maxUserAgent   = 512
	maxReason      = 512
	maxName        = 128 // actor id and name, target id, environment id
	maxShort       = 64  // action, target type, ip
	maxDetailKey   = 64
	maxDetailValue = 512
	maxDetails     = 32
)

// MaxEvidenceValue is the most bytes of one evidence detail the writer stores. Evidence is the
// one kind of detail that is kept whole: the full secret-masked inspect document of a network
// removed as orphaned, which D-12 requires in the log before the network is removed so that it
// can be recreated. Every other detail value is cut at 512 bytes. The allowance is one (action,
// key) pair, named in evidenceKeys, written once per delete attempt by an Env Admin, so the audit
// viewer's 50-entry page stays bounded.
const MaxEvidenceValue = 64 << 10

// evidenceKeys names the only (action, detail key) pairs whose value is evidence (D-12): stored
// byte for byte up to MaxEvidenceValue, refused with ErrEvidenceTooLarge above it, and refused
// with ErrInvalidEntry when cleaning would alter it, because a changed copy is no evidence.
var evidenceKeys = map[string]string{ActionNetworkInspectSaved: "inspect"}

// secretWords are the (case-insensitive) fragments that make a detail key
// ineligible: secret values are never recorded (D-39, T-01-28).
var secretWords = []string{"password", "token", "secret", "key", "cookie", "session", "csrf"}

// Entry is what a caller records. The writer adds the ID, the timestamp, the
// key ID and the chain fields.
type Entry struct {
	Actor                        Actor
	Action, TargetType, TargetID string
	EnvID                        string
	Result                       Result
	Reason, IP, UserAgent        string
	// Details are extra string facts. Keys that name a secret are rejected.
	Details map[string]string
}

// Writer appends entries to the chain. It is safe for concurrent use, and
// several Writers over separate database handles (the server plus a CLI
// process) may append to the same database: each append is one immediate
// transaction that reads the newest HMAC and inserts the next row.
type Writer struct {
	s   *store.Store
	v   *vault.Vault
	log *slog.Logger
	now func() time.Time
}

// NewWriter returns a Writer. now is injected so tests control the clock; nil
// means time.Now.
func NewWriter(s *store.Store, v *vault.Vault, log *slog.Logger, now func() time.Time) *Writer {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Writer{s: s, v: v, log: log, now: now}
}

// Record appends e in its own transaction. A failure is returned, never
// swallowed: callers fail the request rather than proceed unaudited.
func (w *Writer) Record(ctx context.Context, e Entry) error {
	rec, err := w.prepare(e)
	if err != nil {
		return err
	}
	tx, err := w.s.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("audit: begin: %w", err)
	}
	seq, err := w.appendTx(ctx, tx, rec)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("audit: commit: %w", err)
	}
	w.logEntry(seq, rec)
	return nil
}

// RecordTx appends e inside the caller's write transaction, which must come
// from the store's write pool. The write pool has exactly one connection, so a
// handler that already holds a write transaction MUST use RecordTx: Record
// would wait for a connection the handler is holding. The entry becomes
// visible when the caller commits and disappears if it rolls back. The log
// line is emitted when the entry is appended, before the caller's commit.
func (w *Writer) RecordTx(ctx context.Context, tx *sql.Tx, e Entry) error {
	rec, err := w.prepare(e)
	if err != nil {
		return err
	}
	seq, err := w.appendTx(ctx, tx, rec)
	if err != nil {
		return err
	}
	w.logEntry(seq, rec)
	return nil
}

// prepare validates e, caps and cleans every string and stamps the ID, time
// and key ID. Everything that is hashed is exactly what is stored, so a value
// the database could mangle (invalid UTF-8, NUL) never reaches either.
func (w *Writer) prepare(e Entry) (record, error) {
	switch e.Actor.Type {
	case ActorUser, ActorSystem, ActorAnonymous:
	default:
		return record{}, fmt.Errorf("%w: unknown actor type %q", ErrInvalidEntry, e.Actor.Type)
	}
	switch e.Result {
	case ResultSuccess, ResultFailure, ResultDenied:
	default:
		return record{}, fmt.Errorf("%w: unknown result %q", ErrInvalidEntry, e.Result)
	}
	if e.Action == "" {
		return record{}, fmt.Errorf("%w: no action", ErrInvalidEntry)
	}
	if len(e.Details) > maxDetails {
		return record{}, fmt.Errorf("%w: more than %d details", ErrInvalidEntry, maxDetails)
	}
	details := make(map[string]string, len(e.Details))
	for k, val := range e.Details {
		lower := strings.ToLower(k)
		for _, word := range secretWords {
			if strings.Contains(lower, word) {
				return record{}, fmt.Errorf("%w: %q", ErrSecretField, k)
			}
		}
		if evidenceKeys[e.Action] == k {
			if len(val) > MaxEvidenceValue {
				return record{}, fmt.Errorf("%w: %q is %d bytes, over %d", ErrEvidenceTooLarge, k, len(val), MaxEvidenceValue)
			}
			if clean(val, MaxEvidenceValue) != val {
				return record{}, fmt.Errorf("%w: %q holds invalid UTF-8 or a control character", ErrInvalidEntry, k)
			}
			details[clean(k, maxDetailKey)] = val
			continue
		}
		details[clean(k, maxDetailKey)] = clean(val, maxDetailValue)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return record{}, fmt.Errorf("audit: generate id: %w", err)
	}
	return record{
		ID:         id.String(),
		TS:         w.now().UTC().UnixNano(),
		ActorType:  string(e.Actor.Type),
		ActorID:    clean(e.Actor.ID, maxName),
		ActorName:  clean(e.Actor.Name, maxName),
		Action:     clean(e.Action, maxShort),
		TargetType: clean(e.TargetType, maxShort),
		TargetID:   clean(e.TargetID, maxName),
		EnvID:      clean(e.EnvID, maxName),
		Result:     string(e.Result),
		Reason:     clean(e.Reason, maxReason),
		IP:         clean(e.IP, maxShort),
		UserAgent:  clean(e.UserAgent, maxUserAgent),
		Details:    details,
		KeyID:      w.v.KeyID(),
	}, nil
}

// clean makes s safe to store, hash, export and log: invalid UTF-8 and control
// characters (including NUL and newlines) become U+FFFD, then it is cut to at
// most max bytes on a character boundary.
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		s = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return utf8.RuneError
			}
			return r
		}, s)
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// appendTx chains rec onto the newest entry inside tx and inserts it.
func (w *Writer) appendTx(ctx context.Context, tx *sql.Tx, rec record) (int64, error) {
	q := w.s.Q.WithTx(tx)
	prev, err := lastHMAC(ctx, q)
	if err != nil {
		return 0, err
	}
	details, err := json.Marshal(rec.Details)
	if err != nil {
		return 0, fmt.Errorf("audit: encode details: %w", err)
	}
	mac := computeHMAC(w.v.Subkey(vault.LabelAuditHMAC), prev, rec)
	seq, err := q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ID: rec.ID, Ts: rec.TS,
		ActorType: rec.ActorType, ActorID: rec.ActorID, ActorName: rec.ActorName,
		Action: rec.Action, TargetType: rec.TargetType, TargetID: rec.TargetID,
		EnvID:  sql.NullString{String: rec.EnvID, Valid: rec.EnvID != ""},
		Result: rec.Result, Reason: rec.Reason, Ip: rec.IP, UserAgent: rec.UserAgent,
		Details: string(details), KeyID: rec.KeyID, PrevHmac: prev, Hmac: mac,
	})
	if err != nil {
		return 0, fmt.Errorf("audit: insert: %w", err)
	}
	return seq, nil
}

// lastHMAC is the HMAC the next entry chains from: the newest entry's, else
// the HMAC of the last pruned entry, else 32 zero bytes (genesis).
func lastHMAC(ctx context.Context, q *sqlc.Queries) ([]byte, error) {
	prev, err := q.LastAuditHMAC(ctx)
	if err == nil {
		return prev, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("audit: read newest entry: %w", err)
	}
	meta, err := q.GetMeta(ctx, metaLastPruned)
	if errors.Is(err, sql.ErrNoRows) {
		return make([]byte, genesisLen), nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: read last pruned hmac: %w", err)
	}
	prev, err = hex.DecodeString(meta)
	if err != nil || len(prev) != genesisLen {
		return nil, fmt.Errorf("audit: app_meta %s is not a hex HMAC-SHA256 value", metaLastPruned)
	}
	return prev, nil
}

// logEntry writes one structured line per entry (D-05). User-controlled text
// is only ever an attribute value, never part of the message.
func (w *Writer) logEntry(seq int64, r record) {
	w.log.Info("audit entry",
		"seq", seq, "id", r.ID,
		"actor_type", r.ActorType, "actor_id", r.ActorID, "actor", r.ActorName,
		"action", r.Action, "target_type", r.TargetType, "target", r.TargetID,
		"env", r.EnvID, "result", r.Result, "reason", r.Reason, "ip", r.IP,
	)
}
