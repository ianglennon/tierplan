// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/agents"
	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/internal/datadir"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/instance"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/internal/vault"
)

// Field error codes of POST /api/me/password. password_too_short and
// password_common are shared with the setup wizard; the SPA maps each to its
// UI-SPEC message.
const codeCurrentPasswordIncorrect = "current_password_incorrect" //nolint:gosec // a stable error code, not a credential

// Reasons recorded with a refused password change.
const reasonCurrentPasswordIncorrect = "current_password_incorrect" //nolint:gosec // an audit reason, not a credential

// Warning IDs returned by GET /api/warnings. The plain-HTTP warning is not
// here: only the browser knows how it reached the server.
const (
	warningDataNotPersisted = "data_not_persisted"
	warningKeyBackup        = "key_backup"
	warningRefusalBurst     = "refusal_burst"
	warningBackupFailed     = "backup_failed"
	warningUnknownAgents    = "unknown_agents"
	// warningProxyPrefix turns an observer signal kind into a warning ID: the
	// reverse proxy warnings are proxy_untrusted_forwarded, proxy_url_mismatch and
	// the third kind with the same prefix (D-53).
	warningProxyPrefix = "proxy_"
)

// proxyKinds are the observer's signal kinds in the order the warnings are listed.
var proxyKinds = []string{httpx.SignalUntrustedForwarded, httpx.SignalURLMismatch, httpx.SignalWebSocketFailing}

const (
	// settingKeyBackupAck holds the UTC nanoseconds at which an admin said they
	// had backed up the generated encryption key.
	settingKeyBackupAck = "key_backup_ack"
	// keySourceFile is vault.KeySource() for a key that Coaming generated into
	// the data directory, the only key it can lose with the container.
	keySourceFile = "file"
	// targetKeyBackup is the audit target of that acknowledgement.
	targetKeyBackup = "key_backup"
)

// AccountDeps are the dependencies of the account and warnings routes.
type AccountDeps struct {
	Sessions *auth.Sessions
	Instance *instance.Instance
	Store    *store.Store
	Audit    *audit.Writer
	// DataDir drives the data_not_persisted warning.
	DataDir datadir.Info
	// Vault drives the key_backup warning.
	Vault *vault.Vault
	// Proxy holds what the reverse proxy observer has seen, for the proxy
	// warnings. Nil means there are none.
	Proxy *httpx.ProxyObserver
	// Checker decides who is an instance admin, who alone sees the refusal_burst
	// warning. Nil means nobody is.
	Checker *rbac.Checker
	// Refusals lists the recent bursts of refused requests for the refusal_burst
	// warning. Nil means there are none.
	Refusals interface{ Bursts(time.Time) []auth.Burst }
	// Now is the clock the bursts are aged by. Nil means time.Now.
	Now func() time.Time
	// Unknown lists the agents refused unknown_agent after a restore, for the
	// unknown_agents warning. Nil means there are none.
	Unknown *agents.UnknownAgents
	// Limiter throttles POST /api/me/password per user ID, right and wrong
	// attempts alike, so a stolen session cannot guess the current password at
	// the speed of the hash alone (T-01-60). Nil means no throttle.
	Limiter *auth.IPLimiter
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// ChangePasswordInput is a password change. The length caps bound what the
// server reads; the policy is checked after the current password.
type ChangePasswordInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		CurrentPassword string `json:"currentPassword" maxLength:"1024" doc:"The password the account has now"`
		NewPassword     string `json:"newPassword" maxLength:"1024" doc:"The new password; at least 12 characters, not a common password and not containing the username"`
	}
}

// ChangePasswordBody is the new session's CSRF token.
type ChangePasswordBody struct {
	CSRFToken string `json:"csrfToken" doc:"The CSRF token of the new session, sent as X-CSRF-Token on every mutating request"`
}

// ChangePasswordOutput carries the new session cookie with the body.
type ChangePasswordOutput struct {
	SetCookie    http.Cookie `header:"Set-Cookie"`
	CacheControl string      `header:"Cache-Control"`
	Body         ChangePasswordBody
}

// WarningBody is one instance warning that needs an admin's attention.
type WarningBody struct {
	ID      string `json:"id" enum:"data_not_persisted,key_backup,proxy_untrusted_forwarded,proxy_url_mismatch,proxy_websocket_failing,refusal_burst,backup_failed,unknown_agents" doc:"Which warning this is"`
	Reason  string `json:"reason,omitempty" doc:"For data_not_persisted: why the data directory is not persisted"`
	KeyPath string `json:"keyPath,omitempty" doc:"For key_backup: where the generated encryption key is stored"`
	IP      string `json:"ip,omitempty" doc:"For the untrusted-forwarded and failing-upgrade warnings: the address the newest sighting came from"`
	Host    string `json:"host,omitempty" doc:"For proxy_url_mismatch: the host the newest request was for"`
	Scheme  string `json:"scheme,omitempty" doc:"For proxy_url_mismatch: the scheme the newest request effectively used, http or https"`
	URL     string `json:"url,omitempty" doc:"For proxy_url_mismatch: the public URL the request should have matched"`
	Subject string `json:"subject,omitempty" doc:"For refusal_burst: the username that was refused. For backup_failed: why the last scheduled backup failed"`
	Count   int    `json:"count,omitempty" doc:"For refusal_burst: how many times that user was refused inside one minute. For unknown_agents: how many agents Coaming does not know"`
	// Since is a pointer so that "no good backup yet" leaves it out.
	Since *time.Time `json:"since,omitempty" doc:"For backup_failed: when the last good scheduled backup finished; absent when there has not been one"`
}

// WarningsBody lists the warnings that apply right now.
type WarningsBody struct {
	Items []WarningBody `json:"items" doc:"Only the warnings that apply; empty when there are none"`
}

// WarningsOutput wraps WarningsBody for huma.
type WarningsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         WarningsBody
}

// AcknowledgeKeyBackupInput is the acknowledgement of the key backup reminder. It has no body.
type AcknowledgeKeyBackupInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// RegisterAccount registers the signed-in user's own password change and the
// instance warnings. Changing your own password is self-service (D-44); the
// warnings reveal deployment details, so they need the settings permissions.
func RegisterAccount(r *httpx.Router, d AccountDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	internal := func(what string, err error) error {
		d.Log.Error("account request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "changePassword",
		Method:      http.MethodPost,
		Path:        "/api/me/password",
		Summary:     "Change the signed-in user's own password",
		Description: "Needs the current password. On success every session of the user ends, this one included, and a new " +
			"session starts: its cookie is set and its CSRF token returned (D-32, D-34). A wrong current password or a new " +
			"password that breaks the policy answers 422 with a stable code per field. Attempts are throttled per user and the " +
			"refusal is 429 rate_limited. Audited as auth.password_changed.",
		Tags: []string{"auth"},
	}, rbac.SelfService, func(ctx context.Context, in *ChangePasswordInput) (*ChangePasswordOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil || len(p.SessionIDHash) == 0 {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		ip := httpx.ClientIP(ctx)

		// Before the lookup and the hash: a refused attempt costs almost nothing.
		if d.Limiter != nil && !d.Limiter.Allow(p.ID) {
			d.Log.Warn("password change attempts rate limited", "user_id", p.ID, "ip", ip)
			if err := d.Audit.Record(ctx, audit.Entry{
				Actor:      p.AuditActor(),
				Action:     audit.ActionAuthPasswordChanged,
				TargetType: "user",
				TargetID:   p.ID,
				Result:     audit.ResultDenied,
				Reason:     reasonRateLimited,
				IP:         ip,
				UserAgent:  in.UserAgent,
			}); err != nil {
				return nil, internal("audit refused password change", err)
			}
			return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many attempts. Wait a minute and try again.")
		}

		u, err := d.Store.RQ.GetUserByID(ctx, p.ID)
		if err != nil {
			return nil, internal("look up user", err)
		}
		var problems []*huma.ErrorDetail
		add := func(location, code string) {
			problems = append(problems, &huma.ErrorDetail{Location: location, Message: code})
		}
		ok, err := auth.VerifyPassword(in.Body.CurrentPassword, u.PasswordHash)
		if err != nil {
			// An unreadable stored hash is a data problem, not a way past the check.
			d.Log.Error("stored password hash is unreadable", "user_id", u.ID, "error", err)
			ok = false
		}
		if !ok {
			add("body.currentPassword", codeCurrentPasswordIncorrect)
		}
		switch err := auth.ValidatePassword(in.Body.NewPassword, u.Username); {
		case errors.Is(err, auth.ErrPasswordTooShort):
			add("body.newPassword", codePasswordTooShort)
		case errors.Is(err, auth.ErrPasswordCommon):
			add("body.newPassword", codePasswordCommon)
		}
		if len(problems) > 0 {
			if !ok {
				// Guessing the current password from a session is worth seeing in the log (T-01-60).
				if err := d.Audit.Record(ctx, audit.Entry{
					Actor:      p.AuditActor(),
					Action:     audit.ActionAuthPasswordChanged,
					TargetType: "user",
					TargetID:   u.ID,
					Result:     audit.ResultFailure,
					Reason:     reasonCurrentPasswordIncorrect,
					IP:         ip,
					UserAgent:  in.UserAgent,
				}); err != nil {
					return nil, internal("audit refused password change", err)
				}
			}
			return nil, &huma.ErrorModel{
				Type:   problemValidationFailed,
				Title:  http.StatusText(http.StatusUnprocessableEntity),
				Status: http.StatusUnprocessableEntity,
				Detail: "validation failed",
				Errors: problems,
			}
		}

		// Hashing is slow and memory-hungry: do it before taking the write
		// connection, so other writers are not held up behind it.
		hash, err := auth.HashPassword(in.Body.NewPassword)
		if err != nil {
			return nil, internal("hash password", err)
		}
		cur, err := d.Store.RQ.GetSessionByHash(ctx, p.SessionIDHash)
		if err != nil {
			return nil, internal("look up session", err)
		}
		remember := cur.Remember != 0

		tx, err := d.Store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, internal("begin", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed
		q := d.Store.Q.WithTx(tx)

		now := time.Now().UTC().UnixNano()
		if err := q.UpdatePassword(ctx, sqlc.UpdatePasswordParams{PasswordHash: hash, PasswordChangedAt: now, UpdatedAt: now, ID: u.ID}); err != nil {
			return nil, internal("update password", err)
		}
		// No session is kept: this one ends too, and the new one below replaces it (D-32).
		// The empty hash matches no row, so every session of the user goes.
		if err := q.DeleteUserSessionsExcept(ctx, sqlc.DeleteUserSessionsExceptParams{UserID: u.ID, IDHash: []byte{}}); err != nil {
			return nil, internal("end sessions", err)
		}
		raw, idHash, err := d.Sessions.CreateTx(ctx, tx, u.ID, remember, ip, in.UserAgent)
		if err != nil {
			return nil, internal("create session", err)
		}
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor:      p.AuditActor(),
			Action:     audit.ActionAuthPasswordChanged,
			TargetType: "user",
			TargetID:   u.ID,
			Result:     audit.ResultSuccess,
			IP:         ip,
			UserAgent:  in.UserAgent,
		}); err != nil {
			return nil, internal("audit password change", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, internal("commit", err)
		}
		return &ChangePasswordOutput{
			SetCookie:    d.Sessions.Cookie(raw, remember, d.Instance.CookieScope()),
			CacheControl: "no-store",
			Body:         ChangePasswordBody{CSRFToken: d.Sessions.CSRFToken(idHash)},
		}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listWarnings",
		Method:      http.MethodGet,
		Path:        "/api/warnings",
		Summary:     "List the instance warnings that need an admin's attention",
		Description: "Includes data_not_persisted while the data directory would be lost with the container, and key_backup " +
			"while the encryption key Coaming generated into the data directory has not been acknowledged (D-15, D-59). " +
			"The reverse proxy warnings (the proxy_ ones) are listed while " +
			"Coaming has seen that problem in the last hour, one item per kind, naming the newest sighting (D-53). " +
			"refusal_burst lists, for instance admins only, each user who was refused ten or more times in a minute in the last hour (D-20). " +
			"backup_failed is listed, for instance admins only, while scheduled backups are on and the newest scheduled run failed; subject is why and since is when the last good backup finished (D-32). " +
			"unknown_agents is listed, for instance admins only, while agents that were installed after the backup this Coaming was restored from keep trying; count is how many (D-31). " +
			"The plain-HTTP warning is the browser's to compute and is not listed.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(ctx context.Context, _ *struct{}) (*WarningsOutput, error) {
		items := []WarningBody{}
		if !d.DataDir.Persisted {
			items = append(items, WarningBody{ID: warningDataNotPersisted, Reason: d.DataDir.Reason})
		}
		if d.Vault != nil && d.Vault.KeySource() == keySourceFile {
			_, err := d.Store.RQ.GetSetting(ctx, settingKeyBackupAck)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				items = append(items, WarningBody{ID: warningKeyBackup, KeyPath: filepath.Join(d.DataDir.Path, vault.KeyFile)})
			case err != nil:
				return nil, internal("read key backup acknowledgement", err)
			}
		}
		items = append(items, proxyItems(d)...)
		burstItems, err := refusalItems(ctx, d)
		if err != nil {
			return nil, internal("read refusal bursts", err)
		}
		items = append(items, burstItems...)
		failed, err := backupFailedItems(ctx, d)
		if err != nil {
			return nil, internal("read the scheduled backup runs", err)
		}
		items = append(items, failed...)
		unknown, err := unknownAgentsWarning(ctx, d)
		if err != nil {
			return nil, internal("read the unknown agents", err)
		}
		items = append(items, unknown...)
		return &WarningsOutput{CacheControl: "no-store", Body: WarningsBody{Items: items}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "acknowledgeKeyBackup",
		Method:        http.MethodPost,
		Path:          "/api/warnings/key-backup/ack",
		Summary:       "Acknowledge the encryption key backup reminder",
		Description:   "Records, once for the whole instance, that the generated encryption key has been backed up, and audits it as warning.acknowledged.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *AcknowledgeKeyBackupInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		tx, err := d.Store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, internal("begin", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed

		now := time.Now().UTC().UnixNano()
		if err := d.Store.Q.WithTx(tx).UpsertSetting(ctx, sqlc.UpsertSettingParams{
			Key: settingKeyBackupAck, Value: strconv.FormatInt(now, 10), UpdatedAt: now,
		}); err != nil {
			return nil, internal("store acknowledgement", err)
		}
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor:      p.AuditActor(),
			Action:     audit.ActionWarningAcknowledged,
			TargetType: targetKeyBackup,
			TargetID:   targetKeyBackup,
			Result:     audit.ResultSuccess,
			IP:         httpx.ClientIP(ctx),
			UserAgent:  in.UserAgent,
		}); err != nil {
			return nil, internal("audit acknowledgement", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, internal("commit", err)
		}
		return nil, nil
	})
}

// proxyItems turns what the proxy observer has seen into warnings: one per kind,
// from the newest sighting of it. The sightings carry the TCP peer and the
// request's own host and scheme, never a forwarded value, so nothing a caller
// chose is believed here; the bell shows them to settings.read holders only.
func proxyItems(d AccountDeps) []WarningBody {
	if d.Proxy == nil {
		return nil
	}
	newest := map[string]httpx.Signal{}
	for _, s := range d.Proxy.Signals() {
		if _, ok := newest[s.Kind]; !ok {
			newest[s.Kind] = s
		}
	}
	var out []WarningBody
	for _, kind := range proxyKinds {
		s, ok := newest[kind]
		if !ok {
			continue
		}
		w := WarningBody{ID: warningProxyPrefix + kind, IP: s.Peer}
		if kind == httpx.SignalURLMismatch {
			w.Host, w.Scheme, w.URL = s.Host, s.Scheme, d.Instance.PublicURL()
		}
		out = append(out, w)
	}
	return out
}

// refusalItems turns the recent refusal bursts into warnings, one per user
// naming the newest burst, for an active instance admin only: a refusal burst
// names a user, which no one else may see (T-03-39).
func refusalItems(ctx context.Context, d AccountDeps) ([]WarningBody, error) {
	if d.Refusals == nil || d.Checker == nil {
		return nil, nil
	}
	a, err := d.Checker.Access(ctx, httpx.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if !a.Active || !a.InstanceAdmin {
		return nil, nil
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	var out []WarningBody
	seen := map[string]bool{}
	for _, b := range d.Refusals.Bursts(now()) { // newest first
		if seen[b.ActorID] {
			continue
		}
		seen[b.ActorID] = true
		out = append(out, WarningBody{ID: warningRefusalBurst, Subject: b.ActorName, Count: b.Count})
	}
	return out, nil
}

// backupFailedItems is the backup_failed warning: while scheduled backups are on
// and the newest finished run did not write a backup, naming why and when the
// last good one finished. A later successful run ends it, and so does turning the
// schedule off, since nothing is then going to run. It is for an active instance
// admin only: the reason can name paths on the host (T-03-114).
func backupFailedItems(ctx context.Context, d AccountDeps) ([]WarningBody, error) {
	if d.Checker == nil {
		return nil, nil
	}
	a, err := d.Checker.Access(ctx, httpx.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if !a.Active || !a.InstanceAdmin {
		return nil, nil
	}
	sched, err := backup.LoadSchedule(ctx, d.Store)
	if err != nil {
		return nil, err
	}
	if !sched.Enabled {
		return nil, nil
	}
	last, err := d.Store.RQ.LastScheduleRun(ctx)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && last.Ok != 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item := WarningBody{ID: warningBackupFailed, Subject: last.Error}
	switch good, err := d.Store.RQ.LastSuccessfulScheduleRun(ctx); {
	case err == nil && good.FinishedAt.Valid:
		since := time.Unix(0, good.FinishedAt.Int64).UTC()
		item.Since = &since
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	return []WarningBody{item}, nil
}

// unknownAgentsWarning is the unknown_agents warning: how many agents are being
// refused unknown_agent right now, for an active instance admin only (the list
// names nodes and addresses). It is derived from the in-memory registry, so it
// empties by itself 24 hours after the agents stop trying (D-31).
func unknownAgentsWarning(ctx context.Context, d AccountDeps) ([]WarningBody, error) {
	if d.Unknown == nil || d.Checker == nil {
		return nil, nil
	}
	a, err := d.Checker.Access(ctx, httpx.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if !a.Active || !a.InstanceAdmin {
		return nil, nil
	}
	list, err := unknownAgentItems(ctx, UnknownAgentsDeps{Unknown: d.Unknown, Store: d.Store, Now: d.Now})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return []WarningBody{{ID: warningUnknownAgents, Count: len(list)}}, nil
}
