// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// ScheduleRunBody is one scheduled backup run.
type ScheduleRunBody struct {
	At       time.Time `json:"at" doc:"When the run finished"`
	OK       bool      `json:"ok" doc:"Whether it wrote a backup"`
	FileName string    `json:"fileName,omitempty" doc:"The backup file it wrote, when it did"`
	Size     int64     `json:"size,omitempty" doc:"The size of that file in bytes"`
	Error    string    `json:"error,omitempty" doc:"Why it failed, when it did"`
}

// BackupScheduleBody is the backup schedule. The passphrase is never in it.
type BackupScheduleBody struct {
	Enabled         bool             `json:"enabled" doc:"Whether Coaming makes a backup by itself"`
	Frequency       string           `json:"frequency" enum:"daily,weekly" doc:"How often"`
	Weekday         int              `json:"weekday" minimum:"0" maximum:"6" doc:"For a weekly schedule: the day, 0 for Sunday to 6 for Saturday"`
	TimeUTC         string           `json:"timeUtc" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"The time of day in UTC, as 24-hour HH:MM"`
	Keep            int              `json:"keep" minimum:"1" maximum:"365" doc:"How many scheduled backups are kept; older ones are deleted"`
	PassphraseSaved bool             `json:"passphraseSaved" doc:"Whether a passphrase is saved. The passphrase itself is never returned"`
	Dir             string           `json:"dir" doc:"The folder the backups are written into on the Coaming host"`
	NextRun         *time.Time       `json:"nextRun,omitempty" doc:"When the next backup is due; absent while the schedule is off"`
	LastRun         *ScheduleRunBody `json:"lastRun,omitempty" doc:"The newest scheduled run, successful or not"`
	LastSuccess     *ScheduleRunBody `json:"lastSuccess,omitempty" doc:"The newest scheduled run that wrote a backup"`
}

// BackupScheduleOutput wraps BackupScheduleBody for huma.
type BackupScheduleOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         BackupScheduleBody
}

// PutBackupScheduleInput is a change to the schedule.
type PutBackupScheduleInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Enabled    bool   `json:"enabled" doc:"Turn scheduled backups on or off"`
		Frequency  string `json:"frequency" enum:"daily,weekly" doc:"How often"`
		Weekday    int    `json:"weekday,omitempty" minimum:"0" maximum:"6" doc:"For a weekly schedule: the day, 0 for Sunday to 6 for Saturday"`
		TimeUTC    string `json:"timeUtc" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"The time of day in UTC, as 24-hour HH:MM"`
		Keep       int    `json:"keep" minimum:"1" maximum:"365" doc:"How many scheduled backups to keep"`
		Passphrase string `json:"passphrase,omitempty" maxLength:"1024" doc:"Locks the scheduled backups; at least 16 characters. Leave it out to keep the saved one. It is stored sealed in the vault and never returned"`
	}
}

// BackupFileBody is one scheduled backup on disk.
type BackupFileBody struct {
	Name    string    `json:"name" doc:"The file name, coaming-backup-YYYYMMDD-HHMM.coaming"`
	Size    int64     `json:"size" doc:"Size in bytes"`
	Created time.Time `json:"created" doc:"When it was made, from its name (UTC, to the minute)"`
}

// BackupFilesOutput lists the scheduled backups, newest first.
type BackupFilesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []BackupFileBody `json:"items" doc:"The scheduled backups in the backups folder, newest first"`
	}
}

// DownloadBackupFileInput names one scheduled backup.
type DownloadBackupFileInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Name      string `path:"name" doc:"A file name from the list; only names of the form coaming-backup-YYYYMMDD-HHMM.coaming are served"`
}

func runBody(run sqlc.BackupRun) *ScheduleRunBody {
	b := &ScheduleRunBody{OK: run.Ok != 0, FileName: run.FileName, Size: run.SizeBytes, Error: run.Error}
	if run.FinishedAt.Valid {
		b.At = time.Unix(0, run.FinishedAt.Int64).UTC()
	}
	return b
}

// scheduleBody reads the schedule, its next run and its last runs.
func scheduleBody(ctx context.Context, d BackupDeps, s backup.Schedule) (BackupScheduleBody, error) {
	b := BackupScheduleBody{
		Enabled: s.Enabled, Frequency: s.Frequency, Weekday: s.Weekday, TimeUTC: s.TimeUTC, Keep: s.Keep,
		PassphraseSaved: s.PassphraseSaved, Dir: d.Dir,
	}
	if next, ok := backup.NextRun(s, d.Now()); ok {
		b.NextRun = &next
	}
	last, err := d.Store.RQ.LastScheduleRun(ctx)
	switch {
	case err == nil:
		b.LastRun = runBody(last)
	case !errors.Is(err, sql.ErrNoRows):
		return b, err
	}
	good, err := d.Store.RQ.LastSuccessfulScheduleRun(ctx)
	switch {
	case err == nil:
		b.LastSuccess = runBody(good)
	case !errors.Is(err, sql.ErrNoRows):
		return b, err
	}
	return b, nil
}

func registerBackupSchedule(r *httpx.Router, d BackupDeps, need rbac.Requirement, internal func(string, error) error) {
	httpx.Register(r, huma.Operation{
		OperationID: "getBackupSchedule",
		Method:      http.MethodGet,
		Path:        "/api/backup/schedule",
		Summary:     "Get the backup schedule",
		Description: "Whether Coaming makes a backup by itself, when, how many it keeps, where it writes them, when the next one is due " +
			"and how the last runs went. Whether a passphrase is saved is reported; the passphrase never is. Needs backup.manage.",
		Tags: []string{"backup"},
	}, need, func(ctx context.Context, _ *struct{}) (*BackupScheduleOutput, error) {
		s, err := backup.LoadSchedule(ctx, d.Store)
		if err != nil {
			return nil, internal("load the schedule", err)
		}
		body, err := scheduleBody(ctx, d, s)
		if err != nil {
			return nil, internal("read the schedule's runs", err)
		}
		return &BackupScheduleOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "putBackupSchedule",
		Method:      http.MethodPut,
		Path:        "/api/backup/schedule",
		Summary:     "Set the backup schedule",
		Description: "Turns scheduled backups on or off and sets when, how many to keep and the passphrase that locks them. The passphrase " +
			"is sealed in the vault so backups can run with nobody present, and is never returned or logged; leave it out to keep the saved " +
			"one. 422 " + problemPassphraseRequired + " when the schedule is on and no passphrase was ever saved; 422 " + problemPassphraseTooShort +
			" for fewer than 16 characters. Audited as backup.schedule_updated. Needs backup.manage.",
		Tags: []string{"backup"},
	}, need, func(ctx context.Context, in *PutBackupScheduleInput) (*BackupScheduleOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		var pass *string
		if in.Body.Passphrase != "" {
			pass = &in.Body.Passphrase
		}
		s, err := backup.SaveSchedule(ctx, d.Store, d.Vault, d.Audit, p.AuditActor(), backup.ScheduleInput{
			Enabled: in.Body.Enabled, Frequency: in.Body.Frequency, Weekday: in.Body.Weekday,
			TimeUTC: in.Body.TimeUTC, Keep: in.Body.Keep, Passphrase: pass,
		})
		switch {
		case errors.Is(err, backup.ErrInvalidSchedule):
			return nil, validationProblem("body", err.Error())
		case errors.Is(err, backup.ErrPassphraseTooShort):
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemPassphraseTooShort,
				"The passphrase must be at least "+strconv.Itoa(backup.MinPassphraseLen)+" characters.")
		case errors.Is(err, backup.ErrPassphraseRequired):
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemPassphraseRequired,
				"Scheduled backups need a passphrase. Enter one to turn them on.")
		case err != nil:
			return nil, internal("save the schedule", err)
		}
		if d.Scheduler != nil {
			d.Scheduler.Notify()
		}
		body, err := scheduleBody(ctx, d, s)
		if err != nil {
			return nil, internal("read the schedule's runs", err)
		}
		return &BackupScheduleOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listBackupFiles",
		Method:      http.MethodGet,
		Path:        "/api/backup/files",
		Summary:     "List the scheduled backups on disk",
		Description: "The scheduled backups in the backups folder, newest first, with their size and when each was made. Other files in the folder " +
			"(pre-migration and pre-restore snapshots, anything copied in by hand) are not listed. Needs backup.manage.",
		Tags: []string{"backup"},
	}, need, func(context.Context, *struct{}) (*BackupFilesOutput, error) {
		files, err := backup.ListBackupFiles(d.Dir)
		if err != nil {
			return nil, internal("list the backups", err)
		}
		out := &BackupFilesOutput{CacheControl: "no-store"}
		out.Body.Items = make([]BackupFileBody, 0, len(files))
		for _, f := range files {
			out.Body.Items = append(out.Body.Items, BackupFileBody{Name: f.Name, Size: f.Size, Created: f.Created})
		}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "downloadBackupFile",
		Method:      http.MethodGet,
		Path:        "/api/backup/files/{name}",
		Summary:     "Download a scheduled backup",
		Description: "The encrypted backup file as it is on disk. Only names of the form coaming-backup-YYYYMMDD-HHMM.coaming are served; any other " +
			"name is 404 and is never looked up in the file system. Audited as backup.exported with source saved. Needs backup.manage.",
		Tags: []string{"backup"},
		Responses: map[string]*huma.Response{"200": {
			Description: "The backup file",
			Content:     map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}},
			Headers:     map[string]*huma.Param{"Content-Disposition": {Schema: &huma.Schema{Type: "string"}}},
		}},
	}, need, func(ctx context.Context, in *DownloadBackupFileInput) (*huma.StreamResponse, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		f, info, err := backup.OpenBackupFile(d.Dir, in.Name)
		switch {
		case errors.Is(err, backup.ErrNoSuchBackup):
			return nil, httpx.Problem(http.StatusNotFound, "", "There is no such backup.")
		case err != nil:
			return nil, internal("open the backup", err)
		}
		if err := d.Audit.Record(ctx, audit.Entry{
			Actor:      p.AuditActor(),
			Action:     audit.ActionBackupExported,
			TargetType: targetBackup,
			TargetID:   info.Name,
			Result:     audit.ResultSuccess,
			IP:         httpx.ClientIP(ctx),
			UserAgent:  in.UserAgent,
			Details:    map[string]string{"file": info.Name, "size": strconv.FormatInt(info.Size, 10), "source": "saved"},
		}); err != nil {
			_ = f.Close()
			return nil, internal("audit the download", err)
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = f.Close() }()
			hctx.SetHeader("Content-Type", "application/octet-stream")
			hctx.SetHeader("Content-Disposition", "attachment; filename="+strconv.Quote(info.Name))
			hctx.SetHeader("Content-Length", strconv.FormatInt(info.Size, 10))
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetStatus(http.StatusOK)
			if _, err := io.Copy(hctx.BodyWriter(), f); err != nil {
				d.Log.Error("saved backup download failed", "error", err)
			}
		}}, nil
	})
}
