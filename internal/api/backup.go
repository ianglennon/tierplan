// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Stable problem types of the backup routes.
const (
	problemPassphraseTooShort = "passphrase_too_short"
	problemAuditChainBroken   = "audit_chain_broken"
	problemBackupBusy         = "backup_busy"
)

// targetBackup is the audit target of a backup.
const targetBackup = "backup"

// BackupDeps are the dependencies of the backup routes.
type BackupDeps struct {
	Store    *store.Store
	Vault    *vault.Vault
	Audit    *audit.Writer
	Instance *instance.Instance
	// DataDir is where the export's temporary file is written, next to the
	// database, so it is on the same volume and never in a shared temp directory.
	DataDir string
	// AppVersion is recorded in the backup.
	AppVersion string
	// Now stamps the file name and the backup. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// BackupInfoBody is what the Backup page needs to know before it asks for one.
type BackupInfoBody struct {
	MinPassphraseLength int    `json:"minPassphraseLength" doc:"The fewest characters a backup passphrase may have"`
	FileExtension       string `json:"fileExtension" doc:"The extension of a backup file, with the dot"`
}

// BackupInfoOutput wraps BackupInfoBody for huma.
type BackupInfoOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         BackupInfoBody
}

// ExportBackupInput is the request of POST /api/backup/export.
type ExportBackupInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Passphrase string `json:"passphrase" maxLength:"1024" doc:"Locks the backup. At least 16 characters; Bayplan never stores it and cannot recover it"`
	}
}

// RegisterBackup registers the backup routes: the facts the Backup page needs
// and the download of an encrypted backup. Both need backup.manage, which only
// instance admins hold (D-28, D-29, D-32).
func RegisterBackup(r *httpx.Router, d BackupDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	need := rbac.Requirement{Perm: rbac.PermBackupManage, Scope: rbac.ScopeGlobal}
	internal := func(what string, err error) error {
		d.Log.Error("backup request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "getBackupInfo",
		Method:      http.MethodGet,
		Path:        "/api/backup/info",
		Summary:     "Get the rules for a backup passphrase",
		Description: "The shortest passphrase a backup accepts and the file extension a backup file has. Needs backup.manage.",
		Tags:        []string{"backup"},
	}, need, func(context.Context, *struct{}) (*BackupInfoOutput, error) {
		return &BackupInfoOutput{CacheControl: "no-store", Body: BackupInfoBody{
			MinPassphraseLength: backup.MinPassphraseLen, FileExtension: backup.FileExtension,
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "exportBackup",
		Method:      http.MethodPost,
		Path:        "/api/backup/export",
		Summary:     "Download an encrypted backup",
		Description: "Takes a consistent copy of the database while Bayplan runs and answers with it as a download named " +
			"bayplan-backup-<UTC YYYYMMDD-HHMM>.bayplan, locked with the passphrase in the body. The backup holds users, teams, " +
			"access, settings, environments and the audit log. It does not hold the master key, sessions, invite or reset links " +
			"or pending join tokens, and every secret in it is sealed under a key that is only inside the encrypted file. The " +
			"passphrase is sent in the body, is never stored or logged, and is not in the audit entry. 422 " + problemPassphraseTooShort +
			" for fewer than 16 characters; 409 " + problemAuditChainBroken + " when the audit log does not verify; 409 " +
			problemBackupBusy + " while another backup is being prepared. Audited as backup.exported. Needs backup.manage.",
		Tags: []string{"backup"},
		Responses: map[string]*huma.Response{"200": {
			Description: "The backup file",
			Content:     map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}},
			Headers:     map[string]*huma.Param{"Content-Disposition": {Schema: &huma.Schema{Type: "string"}}},
		}},
	}, need, func(ctx context.Context, in *ExportBackupInput) (*huma.StreamResponse, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}

		// Export into a file first, so a failure answers a problem instead of a
		// truncated download. It lives next to the database, readable by this
		// user only, and is removed on every path.
		tmp, err := os.CreateTemp(d.DataDir, ".bayplan-export-*.tmp")
		if err != nil {
			return nil, internal("create the export file", err)
		}
		keep := false
		defer func() {
			if !keep {
				_ = tmp.Close()
				_ = os.Remove(tmp.Name())
			}
		}()

		now := d.Now().UTC()
		res, err := backup.Export(ctx, d.Store, d.Vault, d.DataDir, tmp, []byte(in.Body.Passphrase), backup.ExportOptions{
			AppVersion: d.AppVersion, InstanceName: d.Instance.Name(), PublicURL: d.Instance.PublicURL(),
			Now: func() time.Time { return now },
		})
		switch {
		case errors.Is(err, backup.ErrPassphraseTooShort):
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemPassphraseTooShort,
				"The passphrase must be at least "+strconv.Itoa(backup.MinPassphraseLen)+" characters.")
		case errors.Is(err, backup.ErrBusy):
			return nil, httpx.Problem(http.StatusConflict, problemBackupBusy,
				"Another backup is being prepared. Try again in a moment.")
		case errors.Is(err, backup.ErrChainBroken):
			return nil, httpx.Problem(http.StatusConflict, problemAuditChainBroken,
				"The audit log does not verify, so a backup would carry a log that cannot be trusted. Run bayplan audit verify in the container, or take the backup there with --allow-broken-chain.")
		case err != nil:
			return nil, internal("export", err)
		}

		name := "bayplan-backup-" + now.Format("20060102-1504") + backup.FileExtension
		if err := d.Audit.Record(ctx, audit.Entry{
			Actor:      p.AuditActor(),
			Action:     audit.ActionBackupExported,
			TargetType: targetBackup,
			TargetID:   name,
			Result:     audit.ResultSuccess,
			IP:         httpx.ClientIP(ctx),
			UserAgent:  in.UserAgent,
			Details:    map[string]string{"file": name, "size": strconv.FormatInt(res.Size, 10), "source": "ui"},
		}); err != nil {
			return nil, internal("audit the export", err)
		}

		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return nil, internal("rewind the export", err)
		}
		keep = true
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() {
				_ = tmp.Close()
				_ = os.Remove(tmp.Name())
			}()
			hctx.SetHeader("Content-Type", "application/octet-stream")
			hctx.SetHeader("Content-Disposition", "attachment; filename="+strconv.Quote(name))
			hctx.SetHeader("Content-Length", strconv.FormatInt(res.Size, 10))
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetStatus(http.StatusOK)
			// Headers are on the wire once the first bytes are written, so a
			// failure past that point can only be logged; the client sees a
			// truncated download.
			if _, err := io.Copy(hctx.BodyWriter(), tmp); err != nil {
				d.Log.Error("backup download failed", "error", err)
			}
		}}, nil
	})
}
