// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package audit is Bayplan's tamper-evident record of what happened (AUTH-08,
// D-39 to D-42).
package audit

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
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
	ActionWarningAcknowledged = "warning.acknowledged"
	ActionAuditPruned         = "audit.pruned"
	ActionPermissionDenied    = "permission.denied"
)

// Errors returned by Record.
var (
	// ErrSecretField means a detail key names a secret; nothing was written.
	ErrSecretField = errors.New("audit: detail key names a secret")
	// ErrInvalidEntry means a required field is missing or unknown.
	ErrInvalidEntry = errors.New("audit: invalid entry")
)

var errNotImplemented = errors.New("audit: not implemented")

// Entry is what a caller records. The writer adds the ID, the timestamp, the
// key ID and the chain fields.
type Entry struct {
	Actor                        Actor
	Action, TargetType, TargetID string
	EnvID                        string
	Result                       Result
	Reason, IP, UserAgent        string
	Details                      map[string]string
}

// Writer appends entries to the chain.
type Writer struct{}

// NewWriter returns a Writer. now is injected so tests control the clock.
func NewWriter(s *store.Store, v *vault.Vault, log *slog.Logger, now func() time.Time) *Writer {
	return &Writer{}
}

// Record appends e in its own transaction.
func (w *Writer) Record(ctx context.Context, e Entry) error { return errNotImplemented }

// RecordTx appends e inside the caller's write transaction.
func (w *Writer) RecordTx(ctx context.Context, tx *sql.Tx, e Entry) error {
	return errNotImplemented
}
