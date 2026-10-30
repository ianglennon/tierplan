// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/instance"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/vault"
)

// Stable problem types of the restore routes. The Settings page and the setup
// wizard map them to the same copy; setup_complete is shared with the setup
// routes.
const (
	problemBackupUnreadable = "backup_unreadable"
	problemNotABackup       = "not_a_backup"
	problemBackupNewer      = "backup_newer"
	problemUploadTooLarge   = "upload_too_large"
	problemUploadIncomplete = "upload_incomplete"
	problemRestoreBusy      = "restore_busy"
	problemConfirmMismatch  = "confirm_mismatch"
	problemUploadNotFound   = "upload_not_found"
)

// Restore sources recorded in backup.restored.
const (
	restoreSourceUI     = "ui"
	restoreSourceWizard = "wizard"
)

// SetupTokenHeader carries the setup token on the wizard's raw upload, where
// there is no JSON body to put it in.
const SetupTokenHeader = "X-Setup-Token" //nolint:gosec // the name of a header, not a credential

// uploadReadWindow is how long one upload may take to arrive. The server's own
// read timeout is 30 seconds for a whole request, which a large backup over a
// slow link would exceed; only a caller who passed the policy (or the setup
// token) gets this longer window.
const uploadReadWindow = 15 * time.Minute

// RestoreDeps are the dependencies of the restore routes.
type RestoreDeps struct {
	// Uploads holds the one encrypted file waiting to be checked and applied.
	Uploads *backup.Uploads
	// Vault is the live master key. Prepare re-wraps the backup's secrets to it,
	// and it stays the key after the restart.
	Vault *vault.Vault
	// Audit is the live chain, where backup.checked is recorded.
	Audit *audit.Writer
	// Instance supplies the name the restore is confirmed against and the public
	// URL the backup's is compared with.
	Instance *instance.Instance
	// Setup is the setup routes' gate, token limiter and anonymous audit, which
	// the wizard's restore routes use unchanged.
	Setup SetupDeps
	// DataDir is the data directory the swap happens in.
	DataDir string
	// AppVersion is recorded as the restored instance's version.
	AppVersion string
	// Restart asks the server to stop, run swap with the database closed and the
	// data-directory lock still held, and restart. It reports false when a restart
	// is already pending.
	Restart func(swap func(context.Context) error) bool
	// Limiter throttles the Settings check and restore per user ID, right and
	// wrong attempts alike, so a stolen session cannot try passphrases against an
	// uploaded backup at the speed of the key derivation alone. The wizard has
	// the setup token's limiter instead. Nil means no throttle.
	Limiter *auth.IPLimiter
	// Now stamps the pre-restore snapshot and the restore. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RestoreCheckInput is the request of the Settings check.
type RestoreCheckInput struct {
	// UserAgent and Origin are read only for the audit entry and the public URL
	// comparison; they are not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Origin    string `header:"Origin" hidden:"true"`
	Body      struct {
		UploadID   string `json:"uploadId" maxLength:"64" doc:"The ID the upload answered"`
		Passphrase string `json:"passphrase" maxLength:"1024" doc:"The passphrase the backup was made with"`
	}
}

// RestoreApplyInput is the request of the Settings restore.
type RestoreApplyInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		UploadID   string `json:"uploadId" maxLength:"64" doc:"The ID the upload answered"`
		Passphrase string `json:"passphrase" maxLength:"1024" doc:"The passphrase the backup was made with"`
		Confirm    string `json:"confirm" maxLength:"256" doc:"The name of this instance, typed to confirm that everything in it is replaced"`
	}
}

// SetupRestoreCheckInput is the wizard's check: the setup token travels in the body.
type SetupRestoreCheckInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
	Origin    string `header:"Origin" hidden:"true"`
	Body      struct {
		SetupToken string `json:"setupToken" maxLength:"128" doc:"The setup token printed in the container log"`
		UploadID   string `json:"uploadId" maxLength:"64" doc:"The ID the upload answered"`
		Passphrase string `json:"passphrase" maxLength:"1024" doc:"The passphrase the backup was made with"`
	}
}

// SetupRestoreApplyInput is the wizard's restore.
type SetupRestoreApplyInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		SetupToken string `json:"setupToken" maxLength:"128" doc:"The setup token printed in the container log"`
		UploadID   string `json:"uploadId" maxLength:"64" doc:"The ID the upload answered"`
		Passphrase string `json:"passphrase" maxLength:"1024" doc:"The passphrase the backup was made with"`
	}
}

// DiscardRestoreUploadInput is the request of the Settings discard.
type DiscardRestoreUploadInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// RestoreCountsBody are the headline numbers of a backup.
type RestoreCountsBody struct {
	Users        int64 `json:"users" doc:"Users in the backup"`
	Teams        int64 `json:"teams" doc:"Teams in the backup"`
	Environments int64 `json:"environments" doc:"Environments in the backup"`
	AuditEntries int64 `json:"auditEntries" doc:"Audit entries in the backup"`
}

// RestoreSummaryBody is what a good check shows before anything is replaced.
type RestoreSummaryBody struct {
	InstanceName     string            `json:"instanceName" doc:"The instance the backup was made on"`
	Created          time.Time         `json:"created" doc:"When the backup was made"`
	AppVersion       string            `json:"appVersion" doc:"The Coaming release that made it"`
	Schema           int64             `json:"schema" doc:"The database schema version of the backup"`
	Counts           RestoreCountsBody `json:"counts" doc:"What the backup holds"`
	PublicURL        string            `json:"publicUrl" doc:"The public URL stored in the backup, empty when it had none"`
	Older            bool              `json:"older" doc:"True when the backup is from an older Coaming and is upgraded while restoring"`
	PublicURLDiffers bool              `json:"publicUrlDiffers" doc:"True when the backup's public URL is not the address in use now"`
	ChainVerified    bool              `json:"chainVerified" doc:"False when the audit log did not verify when the backup was made"`
}

// RestoreSummaryOutput wraps RestoreSummaryBody for huma.
type RestoreSummaryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RestoreSummaryBody
}

// RestoreApplyBody answers an accepted restore.
type RestoreApplyBody struct {
	Restarting bool `json:"restarting" doc:"Always true: Coaming is swapping the database and restarting, and everyone is signed out"`
}

// RestoreApplyOutput wraps RestoreApplyBody for huma.
type RestoreApplyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RestoreApplyBody
}

// restoreUploadBody is what a stored upload answers.
type restoreUploadBody struct {
	UploadID  string    `json:"uploadId"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// restoreAPI is the two restore paths over one set of dependencies. They differ
// only in who may call them; checking, applying and the answers are shared.
type restoreAPI struct {
	RestoreDeps
	// busy lets one check or restore run at a time: each derives a key with a lot
	// of memory and one of them replaces the database.
	busy sync.Mutex
}

// RegisterRestore registers the restore routes: upload, check and apply for the
// Settings page (instance admins only) and for the setup wizard (the setup token,
// and only while no user exists).
func RegisterRestore(r *httpx.Router, d RestoreDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Setup.Log == nil {
		d.Setup.Log = d.Log
	}
	a := &restoreAPI{RestoreDeps: d}
	need := rbac.Requirement{Perm: rbac.PermBackupManage, Scope: rbac.ScopeGlobal}

	r.Guarded(http.MethodPost, "/api/backup/restore/upload", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		a.serveUpload(w, req, nil)
	}))

	httpx.Register(r, huma.Operation{
		OperationID:   "discardRestoreUpload",
		Method:        http.MethodDelete,
		Path:          "/api/backup/restore/upload",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Discard the uploaded backup",
		Description: "Removes the backup file that was uploaded and is waiting to be checked or restored, so a file sent by mistake " +
			"does not hold the single upload slot until it expires. Answers 204 whether or not a file was waiting. " +
			"409 " + problemRestoreBusy + " while a check or restore runs or another file is still arriving. " +
			"Audited as backup.upload_discarded when a file was removed. Needs backup.manage.",
		Tags: []string{"backup"},
	}, need, func(ctx context.Context, in *DiscardRestoreUploadInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		return nil, a.discard(ctx, restoreSourceUI, p.AuditActor(), in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "checkRestore",
		Method:      http.MethodPost,
		Path:        "/api/backup/restore/check",
		Summary:     "Check an uploaded backup",
		Description: "Opens the uploaded file with the passphrase and answers what is in it, without changing anything. " +
			"422 " + problemBackupUnreadable + " for a wrong passphrase or a damaged file, 422 " + problemNotABackup + " for a file that is not a backup, " +
			"409 " + problemBackupNewer + " for a backup from a newer Coaming, 404 " + problemUploadNotFound + " for an upload that is gone or expired, " +
			"409 " + problemRestoreBusy + " while another check or restore runs, 429 " + problemRateLimited + " when the caller has used their attempts. " +
			"Audited as backup.checked; a refused check is recorded too, with result failure and a reason (unreadable, not_a_backup, newer, rate_limited). Needs backup.manage.",
		Tags: []string{"backup"},
	}, need, func(ctx context.Context, in *RestoreCheckInput) (*RestoreSummaryOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := a.allow(ctx, p, audit.ActionBackupChecked, restoreSourceUI, in.UserAgent); err != nil {
			return nil, err
		}
		return a.check(ctx, in.Body.UploadID, in.Body.Passphrase, in.Origin, restoreSourceUI, p.AuditActor(), in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "applyRestore",
		Method:        http.MethodPost,
		Path:          "/api/backup/restore/apply",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Restore an uploaded backup over this instance",
		Description: "Replaces everything in this Coaming with the uploaded backup. The backup is opened, migrated to this version, " +
			"re-keyed to this instance's master key and verified on a private copy first; nothing live changes until all of that has " +
			"worked. Then Coaming saves the current database as backups/prerestore-<time>.db, swaps the restored one in and restarts " +
			"itself. The answer is 202 and Coaming is unreachable for a moment; everyone is signed out, including the caller. " +
			"confirm must be this instance's name (422 " + problemConfirmMismatch + "). The same refusals as the check apply and leave " +
			"everything as it was, and 429 " + problemRateLimited + " when the caller has used their attempts. Audited in the restored log as " +
			"audit.reanchored then backup.restored; a refused restore is recorded in this log as backup.failed (stage restore_apply) with a reason. Needs backup.manage.",
		Tags: []string{"backup"},
	}, need, func(ctx context.Context, in *RestoreApplyInput) (*RestoreApplyOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := a.allow(ctx, p, audit.ActionBackupFailed, restoreSourceUI, in.UserAgent); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Body.Confirm) != a.Instance.Name() {
			err := httpx.Problem(http.StatusUnprocessableEntity, problemConfirmMismatch,
				"Type the name of this instance to confirm.")
			a.auditRefusal(ctx, audit.ActionBackupFailed, restoreSourceUI, p.AuditActor(), in.UserAgent, err)
			return nil, err
		}
		return a.apply(ctx, in.Body.UploadID, in.Body.Passphrase, restoreSourceUI, p.AuditActor(), in.UserAgent, nil)
	})

	// The wizard: the same three steps, open to the holder of the setup token for
	// as long as setup is needed. Every one of them asks the gate first, so the
	// answer is 409 setup_complete the moment any user exists.
	r.Guarded(http.MethodPost, "/api/setup/restore/upload", rbac.Public, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		a.serveUpload(w, req, func(ctx context.Context) error {
			return a.Setup.checkAccess(ctx, req.UserAgent(), req.Header.Get(SetupTokenHeader))
		})
	}))
	// Discarding a file sent by mistake takes the same token as sending it.
	r.Guarded(http.MethodDelete, "/api/setup/restore/upload", rbac.Public, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := a.Setup.checkAccess(req.Context(), req.UserAgent(), req.Header.Get(SetupTokenHeader)); err != nil {
			writeProblemError(w, err)
			return
		}
		if err := a.discard(req.Context(), restoreSourceWizard, audit.Actor{Type: audit.ActorAnonymous}, req.UserAgent()); err != nil {
			writeProblemError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}))

	httpx.Register(r, huma.Operation{
		OperationID: "setupRestoreCheck",
		Method:      http.MethodPost,
		Path:        "/api/setup/restore/check",
		Summary:     "Check an uploaded backup during setup",
		Description: "The setup wizard's version of the backup check. Needs the setup token; 409 " + problemSetupComplete +
			" once any user exists, 401 for a wrong token, 429 when the address has used its attempts. Otherwise as the Settings check.",
		Tags: []string{"setup"},
	}, rbac.Public, func(ctx context.Context, in *SetupRestoreCheckInput) (*RestoreSummaryOutput, error) {
		if err := a.Setup.checkAccess(ctx, in.UserAgent, in.Body.SetupToken); err != nil {
			return nil, err
		}
		return a.check(ctx, in.Body.UploadID, in.Body.Passphrase, in.Origin, restoreSourceWizard, audit.Actor{Type: audit.ActorAnonymous}, in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "setupRestoreApply",
		Method:        http.MethodPost,
		Path:          "/api/setup/restore/apply",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Restore an uploaded backup instead of creating the first admin",
		Description: "The setup wizard's restore. Needs the setup token; 409 " + problemSetupComplete + " once any user exists. " +
			"Validates and prepares the backup as the Settings restore does, swaps it in and restarts; the restored accounts then sign in.",
		Tags: []string{"setup"},
	}, rbac.Public, func(ctx context.Context, in *SetupRestoreApplyInput) (*RestoreApplyOutput, error) {
		if err := a.Setup.checkAccess(ctx, in.UserAgent, in.Body.SetupToken); err != nil {
			return nil, err
		}
		// The gate is asked once more at the last moment: an admin created while the
		// backup was being prepared would be replaced without being asked.
		return a.apply(ctx, in.Body.UploadID, in.Body.Passphrase, restoreSourceWizard, audit.Actor{Type: audit.ActorAnonymous}, in.UserAgent, func(ctx context.Context) error {
			needs, err := a.Setup.Gate.Check(ctx)
			if err != nil {
				return err
			}
			if !needs {
				return httpx.Problem(http.StatusConflict, problemSetupComplete, "Setup is already complete.")
			}
			return nil
		})
	})
}

// restoreProblem is the one place a backup error becomes an answer, shared by
// both paths. An error it does not know is logged and answered 500 without
// detail.
func (a *restoreAPI) restoreProblem(step string, err error) error {
	var newer *backup.NewerBackupError
	switch {
	case errors.As(err, &newer):
		return httpx.Problem(http.StatusConflict, problemBackupNewer,
			"This backup was made by a newer Coaming ("+newer.AppVersion+"). Upgrade this Coaming to "+newer.AppVersion+" or newer, then try again.")
	case errors.Is(err, backup.ErrNewerBackup), errors.Is(err, backup.ErrNewerFormat):
		return httpx.Problem(http.StatusConflict, problemBackupNewer,
			"This backup was made by a newer Coaming. Upgrade this Coaming, then try again.")
	case errors.Is(err, backup.ErrNotBackup):
		return httpx.Problem(http.StatusUnprocessableEntity, problemNotABackup, "That isn't a Coaming backup file.")
	case errors.Is(err, backup.ErrUnreadable), errors.Is(err, backup.ErrHeaderLimits):
		// A wrong passphrase and a damaged file cannot be told apart, and the
		// answer must not help someone guess.
		return httpx.Problem(http.StatusUnprocessableEntity, problemBackupUnreadable,
			"Couldn't open this backup. Check the passphrase, and that the file isn't damaged.")
	case errors.Is(err, backup.ErrTooLarge):
		return httpx.Problem(http.StatusRequestEntityTooLarge, problemUploadTooLarge,
			"This file is larger than the "+byteSize(a.Uploads.Max())+" Coaming accepts (COAMING_BACKUP_MAX_UPLOAD).")
	case errors.Is(err, backup.ErrIncomplete):
		return httpx.Problem(http.StatusBadRequest, problemUploadIncomplete,
			"The upload was cut off. If Coaming runs behind a reverse proxy, check its upload size limit.")
	case errors.Is(err, backup.ErrRestoreBusy):
		return httpx.Problem(http.StatusConflict, problemRestoreBusy,
			"Another restore is in progress. Wait for it to finish, or for its upload to expire in 15 minutes.")
	case errors.Is(err, backup.ErrUploadNotFound):
		return httpx.Problem(http.StatusNotFound, problemUploadNotFound,
			"That upload is gone or has expired. Upload the file again.")
	}
	a.Log.Error("restore request failed", "step", step, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// byteSize writes a byte count the way the setting is written.
func byteSize(n int64) string {
	const mib, gib = 1 << 20, 1 << 30
	switch {
	case n%gib == 0:
		return strconv.FormatInt(n/gib, 10) + " GiB"
	case n%mib == 0:
		return strconv.FormatInt(n/mib, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// serveUpload stores the request body as the one pending upload. gate, when set,
// is the wizard's token check; it runs before a byte of the body is read.
func (a *restoreAPI) serveUpload(w http.ResponseWriter, req *http.Request, gate func(context.Context) error) {
	if gate != nil {
		if err := gate(req.Context()); err != nil {
			writeProblemError(w, err)
			return
		}
	}
	if a.Uploads == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	max := a.Uploads.Max()
	if req.ContentLength > max {
		writeProblemError(w, a.restoreProblem("upload", backup.ErrTooLarge))
		return
	}
	// The server's 30 second read timeout covers the whole request, which a large
	// file over a slow link outlasts. The caller has passed the policy by now.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadWindow))

	id, err := a.Uploads.Save(http.MaxBytesReader(w, req.Body, max), req.ContentLength)
	if err != nil {
		writeProblemError(w, a.restoreProblem("upload", err))
		return
	}
	var size int64
	if f, err := a.Uploads.Open(id); err == nil {
		if info, serr := f.Stat(); serr == nil {
			size = info.Size()
		}
		_ = f.Close()
	}
	// The lifetime runs from the moment the file was complete, not from when the
	// request began, so a slow upload still has its whole time to check and apply.
	expires, eerr := a.Uploads.Expires(id)
	if eerr != nil {
		expires = a.Now().Add(a.Uploads.Lifetime())
	}
	body, _ := json.Marshal(restoreUploadBody{UploadID: id, Size: size, ExpiresAt: expires.UTC()})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// writeProblemError writes the problem a huma error carries, for the plain
// handlers that cannot return one. A problem with fields of its own, such as the
// environment problems, keeps them.
func writeProblemError(w http.ResponseWriter, err error) {
	var em *huma.ErrorModel
	if errors.As(err, &em) {
		writeJSONProblem(w, em.Status, em.Type, em.Detail)
		return
	}
	var se huma.StatusError
	if errors.As(err, &se) {
		if body, mErr := json.Marshal(se); mErr == nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(se.GetStatus())
			_, _ = w.Write(body)
			return
		}
	}
	writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
}

// discard removes the waiting upload, if any, and records it. It does not run
// beside a check or a restore, which have the file open.
func (a *restoreAPI) discard(ctx context.Context, source string, actor audit.Actor, userAgent string) error {
	if a.Uploads == nil {
		return httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	if !a.busy.TryLock() {
		return a.restoreProblem("discard", backup.ErrRestoreBusy)
	}
	defer a.busy.Unlock()

	removed, err := a.Uploads.Discard()
	if err != nil {
		return a.restoreProblem("discard", err)
	}
	if !removed {
		return nil
	}
	if err := a.Audit.Record(ctx, audit.Entry{
		Actor:      actor,
		Action:     audit.ActionBackupUploadDiscarded,
		TargetType: targetBackup,
		Result:     audit.ResultSuccess,
		IP:         httpx.ClientIP(ctx),
		UserAgent:  userAgent,
		Details:    map[string]string{"source": source},
	}); err != nil {
		return a.restoreProblem("audit the discard", err)
	}
	return nil
}

// allow spends one attempt of the signed-in caller's budget, before any key is
// derived. A refused attempt is audited and answered 429.
func (a *restoreAPI) allow(ctx context.Context, p *rbac.Principal, action, source, userAgent string) error {
	if a.Limiter == nil || a.Limiter.Allow(p.ID) {
		return nil
	}
	a.Log.Warn("restore attempts rate limited", "user_id", p.ID, "ip", httpx.ClientIP(ctx))
	err := httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many attempts. Wait a minute and try again.")
	a.auditRefusal(ctx, action, source, p.AuditActor(), userAgent, err)
	return err
}

// restoreFailureReason is the short reason recorded for a refused check or
// restore, or "" for an answer that says nothing about the backup or the caller
// (another restore running, a missing upload, a closed setup).
func restoreFailureReason(err error) string {
	var em *huma.ErrorModel
	if !errors.As(err, &em) {
		return "failed"
	}
	switch em.Type {
	case problemBackupUnreadable:
		return "unreadable"
	case problemNotABackup:
		return "not_a_backup"
	case problemBackupNewer:
		return "newer"
	case problemConfirmMismatch:
		return "confirm_mismatch"
	case problemRateLimited:
		return reasonRateLimited
	}
	if em.Status >= http.StatusInternalServerError {
		return "failed"
	}
	return ""
}

// auditRefusal records a check or a restore that did not go through, so a
// stolen session or a guesser trying passphrases leaves a trace (AUTH-10, D-49).
// It names the reason and the source and never the passphrase, the upload or the
// text of the error. The check is recorded as backup.checked and a restore as
// backup.failed at the stage restore_apply, both with a failure result. A
// failed write is logged and does not change the answer, which is already a
// refusal.
func (a *restoreAPI) auditRefusal(ctx context.Context, action, source string, actor audit.Actor, userAgent string, err error) {
	reason := restoreFailureReason(err)
	if reason == "" {
		return
	}
	result := audit.ResultFailure
	if reason == reasonRateLimited {
		result = audit.ResultDenied
	}
	details := map[string]string{"source": source}
	if action == audit.ActionBackupFailed {
		details["stage"] = "restore_apply"
	}
	// An anonymous caller's rows go through the throttle that keeps a flood of them
	// from starving the single writer.
	var rec httpx.Auditor = a.Audit
	if actor.Type == audit.ActorAnonymous && a.Setup.AnonAudit != nil {
		rec = a.Setup.AnonAudit
	}
	if werr := rec.Record(ctx, audit.Entry{
		Actor:      actor,
		Action:     action,
		TargetType: targetBackup,
		Result:     result,
		Reason:     reason,
		IP:         httpx.ClientIP(ctx),
		UserAgent:  userAgent,
		Details:    details,
	}); werr != nil {
		a.Log.Error("could not audit a refused restore step", "error", werr, "action", action, "reason", reason)
	}
}

// check opens the upload and answers what it holds. A refusal is audited.
func (a *restoreAPI) check(ctx context.Context, id, passphrase, origin, source string, actor audit.Actor, userAgent string) (*RestoreSummaryOutput, error) {
	out, err := a.checkUpload(ctx, id, passphrase, origin, actor, userAgent)
	if err != nil {
		a.auditRefusal(ctx, audit.ActionBackupChecked, source, actor, userAgent, err)
	}
	return out, err
}

func (a *restoreAPI) checkUpload(ctx context.Context, id, passphrase, origin string, actor audit.Actor, userAgent string) (*RestoreSummaryOutput, error) {
	if !a.busy.TryLock() {
		return nil, a.restoreProblem("check", backup.ErrRestoreBusy)
	}
	defer a.busy.Unlock()

	schema, err := store.TargetSchema()
	if err != nil {
		return nil, a.restoreProblem("target schema", err)
	}
	f, err := a.Uploads.Open(id)
	if err != nil {
		return nil, a.restoreProblem("open the upload", err)
	}
	in, err := backup.Inspect(f, []byte(passphrase), schema)
	_ = f.Close()
	if err != nil {
		return nil, a.restoreProblem("check", err)
	}
	m := in.Manifest

	if err := a.Audit.Record(ctx, audit.Entry{
		Actor:      actor,
		Action:     audit.ActionBackupChecked,
		TargetType: targetBackup,
		Result:     audit.ResultSuccess,
		IP:         httpx.ClientIP(ctx),
		UserAgent:  userAgent,
		Details: map[string]string{
			"format":      strconv.Itoa(int(in.Header.Format)),
			"schema":      strconv.FormatInt(m.Schema, 10),
			"app_version": m.AppVersion,
		},
	}); err != nil {
		return nil, a.restoreProblem("audit the check", err)
	}

	return &RestoreSummaryOutput{CacheControl: "no-store", Body: RestoreSummaryBody{
		InstanceName: m.InstanceName,
		Created:      m.Created.UTC(),
		AppVersion:   m.AppVersion,
		Schema:       m.Schema,
		Counts: RestoreCountsBody{
			Users: m.Counts.Users, Teams: m.Counts.Teams, Environments: m.Counts.Environments, AuditEntries: m.Counts.AuditEntries,
		},
		PublicURL:        m.PublicURL,
		Older:            m.Schema < schema,
		PublicURLDiffers: a.publicURLDiffers(m.PublicURL, origin),
		ChainVerified:    m.ChainVerified,
	}}, nil
}

// publicURLDiffers reports whether the backup's public URL is not the address in
// use. The address in use is the stored or configured public URL, or before one
// is stored (a fresh install) the origin the browser is at.
func (a *restoreAPI) publicURLDiffers(backupURL, requestOrigin string) bool {
	if backupURL == "" {
		return false
	}
	trim := func(s string) string { return strings.ToLower(strings.TrimRight(s, "/")) }
	if cur := a.Instance.PublicURL(); cur != "" {
		return trim(cur) != trim(backupURL)
	}
	if requestOrigin == "" {
		return false
	}
	return httpx.OriginOf(backupURL) != httpx.OriginOf(requestOrigin)
}

// apply validates and prepares the upload on a private copy, then hands the
// server the swap to run once it has stopped. recheck, when set, runs after the
// preparation and before the restart is requested. A refusal is audited.
func (a *restoreAPI) apply(ctx context.Context, id, passphrase, source string, actor audit.Actor, userAgent string, recheck func(context.Context) error) (*RestoreApplyOutput, error) {
	out, err := a.applyUpload(ctx, id, passphrase, source, recheck)
	if err != nil {
		a.auditRefusal(ctx, audit.ActionBackupFailed, source, actor, userAgent, err)
	}
	return out, err
}

func (a *restoreAPI) applyUpload(ctx context.Context, id, passphrase, source string, recheck func(context.Context) error) (*RestoreApplyOutput, error) {
	if !a.busy.TryLock() {
		return nil, a.restoreProblem("apply", backup.ErrRestoreBusy)
	}
	// Held for good once the restart is requested: the process is going away, and a
	// second restore must not start in the meantime.
	accepted := false
	defer func() {
		if !accepted {
			a.busy.Unlock()
		}
	}()

	schema, err := store.TargetSchema()
	if err != nil {
		return nil, a.restoreProblem("target schema", err)
	}
	f, err := a.Uploads.Open(id)
	if err != nil {
		return nil, a.restoreProblem("open the upload", err)
	}
	staged, err := backup.Stage(ctx, f, []byte(passphrase), a.DataDir, schema)
	_ = f.Close()
	if err != nil {
		return nil, a.restoreProblem("stage", err)
	}
	// Prepare removes the staged copy itself when it fails.
	started := a.Now()
	if err := staged.Prepare(ctx, a.Vault, backup.PrepareOptions{Source: source, AppVersion: a.AppVersion, Now: a.Now}); err != nil {
		return nil, a.restoreProblem("prepare", err)
	}
	if recheck != nil {
		if err := recheck(ctx); err != nil {
			staged.Discard()
			var em *huma.ErrorModel
			if errors.As(err, &em) {
				return nil, err
			}
			return nil, a.restoreProblem("recheck", err)
		}
	}

	swap := func(ctx context.Context) error {
		// The marker is written before the database is replaced, so a restored
		// database can never be live without its rollback record.
		if _, err := staged.SwapWithMarker(ctx, a.DataDir, started); err != nil {
			return err
		}
		a.Uploads.Remove(id)
		return nil
	}
	if !a.Restart(swap) {
		staged.Discard()
		return nil, a.restoreProblem("restart", backup.ErrRestoreBusy)
	}
	accepted = true
	return &RestoreApplyOutput{CacheControl: "no-store", Body: RestoreApplyBody{Restarting: true}}, nil
}
