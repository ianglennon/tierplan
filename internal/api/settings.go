// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// problemPublicURLLocked is the 409 returned when BAYPLAN_PUBLIC_URL fixes the
// public URL and a request asks for a different one (D-04).
const problemPublicURLLocked = "public_url_locked"

// targetSettings is the audit target of a settings change.
const targetSettings = "settings"

// SettingsDeps are the dependencies of the settings routes.
type SettingsDeps struct {
	Instance *instance.Instance
	Store    *store.Store
	Audit    *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// SettingsBody is the instance's editable settings.
type SettingsBody struct {
	InstanceName    string `json:"instanceName" doc:"The name shown in the header, tab titles and the sign-in page"`
	PublicURL       string `json:"publicUrl" doc:"The URL users reach Bayplan at; drives the Origin check and the cookie Secure flag"`
	PublicURLLocked bool   `json:"publicUrlLocked" doc:"True when the BAYPLAN_PUBLIC_URL environment variable fixes the public URL"`
}

// SettingsOutput wraps SettingsBody for huma.
type SettingsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SettingsBody
}

// UpdateSettingsInput changes the settings it names and leaves the others alone.
type UpdateSettingsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		InstanceName *string `json:"instanceName,omitempty" doc:"New instance name, 1 to 64 characters"`
		PublicURL    *string `json:"publicUrl,omitempty" doc:"New public URL, an absolute http or https URL. Refused with 409 public_url_locked while BAYPLAN_PUBLIC_URL is set"`
	}
}

// RegisterSettings registers GET and PATCH /api/settings: the instance name
// (D-03) and the public URL (D-04). Both are global settings.
func RegisterSettings(r *httpx.Router, d SettingsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	internal := func(what string, err error) error {
		d.Log.Error("settings request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	current := func() SettingsBody {
		return SettingsBody{
			InstanceName:    d.Instance.Name(),
			PublicURL:       d.Instance.PublicURL(),
			PublicURLLocked: d.Instance.Locked(),
		}
	}
	// Changes are rare and must reach the cache in the order they reach the
	// database, so one at a time.
	var mu sync.Mutex

	httpx.Register(r, huma.Operation{
		OperationID: "getSettings",
		Method:      http.MethodGet,
		Path:        "/api/settings",
		Summary:     "Get the instance name and public URL",
		Description: "publicUrlLocked is true when the BAYPLAN_PUBLIC_URL environment variable fixes the public URL (D-04).",
		Tags:        []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(context.Context, *struct{}) (*SettingsOutput, error) {
		return &SettingsOutput{CacheControl: "no-store", Body: current()}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateSettings",
		Method:      http.MethodPatch,
		Path:        "/api/settings",
		Summary:     "Change the instance name or the public URL",
		Description: "Only the fields sent are changed. Invalid fields answer 422 with a stable code per field " +
			"(instance_name_required, instance_name_too_long, invalid_public_url). A different public URL while " +
			"BAYPLAN_PUBLIC_URL is set answers 409 public_url_locked and changes nothing. A change takes effect at once: " +
			"the Origin check and the cookie Secure flag use the new public URL from the next request. Each change is " +
			"audited as settings.updated with the old and new value of every changed field.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *UpdateSettingsInput) (*SettingsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}

		mu.Lock()
		defer mu.Unlock()

		before := current()
		name, url := before.InstanceName, before.PublicURL
		details := map[string]string{}

		var problems []*huma.ErrorDetail
		add := func(location, code string) {
			problems = append(problems, &huma.ErrorDetail{Location: location, Message: code})
		}

		if in.Body.InstanceName != nil {
			n := strings.TrimSpace(*in.Body.InstanceName)
			switch c := utf8.RuneCountInString(n); {
			case c == 0:
				add("body.instanceName", codeInstanceNameRequired)
			case c > maxInstanceNameCharacters:
				add("body.instanceName", codeInstanceNameTooLong)
			case n != name:
				details["instance_name_from"], details["instance_name_to"] = name, n
				name = n
			}
		}

		if in.Body.PublicURL != nil {
			raw := strings.TrimSpace(*in.Body.PublicURL)
			u, err := config.NormalisePublicURL(raw)
			switch {
			case before.PublicURLLocked:
				// Echoing the fixed value back is not a change; anything else is refused.
				if err != nil || u != before.PublicURL {
					return nil, httpx.Problem(http.StatusConflict, problemPublicURLLocked, "The public URL is set by the BAYPLAN_PUBLIC_URL environment variable.")
				}
			case err != nil || u == "":
				add("body.publicUrl", codeInvalidPublicURL)
			case u != url:
				details["public_url_from"], details["public_url_to"] = url, u
				url = u
			}
		}

		if len(problems) > 0 {
			return nil, &huma.ErrorModel{
				Type:   problemValidationFailed,
				Title:  http.StatusText(http.StatusUnprocessableEntity),
				Status: http.StatusUnprocessableEntity,
				Detail: "validation failed",
				Errors: problems,
			}
		}
		if len(details) == 0 {
			return &SettingsOutput{CacheControl: "no-store", Body: before}, nil
		}

		tx, err := d.Store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, internal("begin", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed

		if err := d.Instance.SetTx(ctx, tx, d.Store.Q, name, url); err != nil {
			if errors.Is(err, instance.ErrPublicURLLocked) {
				return nil, httpx.Problem(http.StatusConflict, problemPublicURLLocked, "The public URL is set by the BAYPLAN_PUBLIC_URL environment variable.")
			}
			return nil, internal("store settings", err)
		}
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor:      p.AuditActor(),
			Action:     audit.ActionSettingsUpdated,
			TargetType: targetSettings,
			TargetID:   targetSettings,
			Result:     audit.ResultSuccess,
			IP:         httpx.ClientIP(ctx),
			UserAgent:  in.UserAgent,
			Details:    details,
		}); err != nil {
			return nil, internal("audit settings change", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, internal("commit", err)
		}
		// Only after the commit, so a rollback leaves the cache and the database in step.
		d.Instance.Apply(name, url)
		return &SettingsOutput{CacheControl: "no-store", Body: current()}, nil
	})
}
