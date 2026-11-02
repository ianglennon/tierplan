// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

const (
	problemShellPreferenceNotFound = "shell_preference_not_found"
	// maxShellPrefImage is how long an image reference may be to be remembered or asked
	// for; a longer one is never a real reference.
	maxShellPrefImage = 512
)

// ShellPreferenceInput is the image whose remembered shell choice is asked for.
type ShellPreferenceInput struct {
	Image string `query:"image" required:"true" minLength:"1" maxLength:"512" doc:"The image reference of the container, for example nginx:1.27, as the container detail names it"`
}

// ShellPreferenceBody is how the signed-in user last opened a shell in an image.
type ShellPreferenceBody struct {
	Shell      string `json:"shell" enum:"auto,bash,sh,ash,custom" doc:"The shell that was chosen"`
	Command    string `json:"command" doc:"With shell custom, the command that was entered; otherwise empty"`
	User       string `json:"user" doc:"The user it ran as; empty means the container's own"`
	WorkingDir string `json:"workingDir" doc:"Its working directory; empty means the container's own"`
}

// ShellPreferenceOutput wraps ShellPreferenceBody for huma.
type ShellPreferenceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ShellPreferenceBody
}

// registerShellPreference registers the signed-in user's remembered shell choice for an
// image (D-26). It is self-service and answers only the caller's own row: another user's
// choice, or an image the caller never opened a shell in, is the same 404.
func registerShellPreference(r *httpx.Router, d ShellsDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getShellPreference", Method: http.MethodGet, Path: "/api/me/shell-preferences",
		Summary: "How the signed-in user last opened a shell in an image",
		Description: "The shell, custom command, user and working directory of the caller's most recent successful shell in a container of the image, " +
			"so the launcher can preselect them on any browser. 404 shell_preference_not_found when the caller has none for that image. " +
			"At most 200 images are remembered per user; the oldest are dropped. Nothing typed in a shell is part of it.",
		Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, in *ShellPreferenceInput) (*ShellPreferenceOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Store == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
		}
		if p.Type != rbac.PrincipalUser {
			return nil, httpx.Problem(http.StatusForbidden, "", "Only a signed-in user has shell preferences.")
		}
		row, err := d.Store.RQ.GetShellPref(ctx, sqlc.GetShellPrefParams{UserID: p.ID, Image: in.Image})
		if errors.Is(err, sql.ErrNoRows) {
			return nil, httpx.Problem(http.StatusNotFound, problemShellPreferenceNotFound, "No shell choice is remembered for that image.")
		}
		if err != nil {
			d.Log.Error("reading a shell preference failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &ShellPreferenceOutput{CacheControl: "no-store", Body: ShellPreferenceBody{
			Shell: row.Shell, Command: row.Command, User: row.RunAs, WorkingDir: row.WorkingDir,
		}}, nil
	})
}

// rememberShell saves how p opened a shell, keyed by the container's image, and trims
// the user's rows to the newest 200. The image is the inspected Config.Image, else the
// list's. Nothing is saved for a caller who is not a user, for an image with no
// reference or one too long to be real, and a failure is logged and goes no further.
func rememberShell(ctx context.Context, d ShellsDeps, p *rbac.Principal, inspected, listed string, b ShellOpenBody) {
	if d.Store == nil || p.Type != rbac.PrincipalUser {
		return
	}
	image := inspected
	if image == "" {
		image = listed
	}
	if image == "" || len(image) > maxShellPrefImage {
		return
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	command := ""
	if b.Shell == "custom" {
		command = b.Command
	}
	// The write must outlive the request being cancelled by the browser going away.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shellAuditTimeout)
	defer cancel()
	save := func() error {
		tx, err := d.Store.Write.BeginTx(wctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed
		q := d.Store.Q.WithTx(tx)
		if err := q.UpsertShellPref(wctx, sqlc.UpsertShellPrefParams{
			UserID: p.ID, Image: image, Shell: b.Shell, Command: command, RunAs: b.User, WorkingDir: b.WorkingDir,
			UpdatedAt: now().UTC().UnixNano(),
		}); err != nil {
			return err
		}
		if err := q.TrimShellPrefs(wctx, p.ID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := save(); err != nil {
		d.Log.Warn("could not remember a shell choice", "error", err)
	}
}
