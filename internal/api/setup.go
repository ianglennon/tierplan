// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// Problem types the setup endpoints emit. The SPA maps them to the copy in
// UI-SPEC "Setup wizard": invalid_setup_token and rate_limited are step 1's two
// errors, and setup_complete sends the browser to /login.
const (
	problemInvalidSetupToken = "invalid_setup_token"
	problemRateLimited       = "rate_limited"
	problemSetupComplete     = "setup_complete"
)

// Audit reasons recorded with setup.token_failed.
const (
	reasonInvalidToken = "invalid_token"
	reasonRateLimited  = "rate_limited"
)

// settingInstanceName is the settings key holding the instance name (D-03).
// Plan 10 writes it when setup completes.
const settingInstanceName = "instance_name"

// SetupDeps are the dependencies of the setup routes.
type SetupDeps struct {
	Gate    *auth.SetupGate
	Limiter *auth.IPLimiter
	Audit   httpx.Auditor
	Store   *store.Store
	Config  config.Config
	// SPA serves the single-page app for GET /setup while setup is needed.
	SPA http.Handler
	// Hostname supplies the default instance name. Nil means os.Hostname.
	Hostname func() (string, error)
	// Log receives the wrong-token warnings. Nil means slog.Default().
	Log *slog.Logger
}

// InstanceBody is the public instance identity the SPA needs before sign-in.
type InstanceBody struct {
	InstanceName string `json:"instanceName" doc:"Display name of this instance, empty before setup"`
	NeedsSetup   bool   `json:"needsSetup" doc:"True while no admin exists and the setup wizard is open"`
	RememberDays int    `json:"rememberDays" doc:"Days a 'keep me signed in' session lasts"`
}

// InstanceOutput wraps InstanceBody for huma.
type InstanceOutput struct {
	Body InstanceBody
}

// VerifySetupTokenInput is the step 1 submission.
type VerifySetupTokenInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		// maxLength caps the submission well above a real token (35 characters
		// with dashes) so a client cannot make the server buffer more.
		Token string `json:"token" maxLength:"128" doc:"The setup token printed in the container log"`
	}
}

// SetupDefaultsBody carries the defaults step 3 pre-fills.
type SetupDefaultsBody struct {
	DefaultInstanceName string `json:"defaultInstanceName" doc:"The server's hostname"`
	PublicURL           string `json:"publicUrl" doc:"The configured public URL, empty when none is set"`
	PublicURLLocked     bool   `json:"publicUrlLocked" doc:"True when BAYPLAN_PUBLIC_URL fixes the public URL"`
}

// VerifySetupTokenOutput wraps SetupDefaultsBody for huma.
type VerifySetupTokenOutput struct {
	Body SetupDefaultsBody
}

// RegisterSetup registers the first-run operations. All of them are public by
// necessity: they exist before any user does. Once an admin exists the API
// answers 409 and /setup redirects, both decided server-side (D-06).
func RegisterSetup(r *httpx.Router, d SetupDeps) {
	if d.Hostname == nil {
		d.Hostname = os.Hostname
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}

	httpx.Register(r, huma.Operation{
		OperationID: "getInstance",
		Method:      http.MethodGet,
		Path:        "/api/instance",
		Summary:     "Get the instance name, whether setup is needed and the remember-me length",
		Tags:        []string{"setup"},
	}, rbac.Public, func(ctx context.Context, _ *struct{}) (*InstanceOutput, error) {
		needs, err := d.Gate.Check(ctx)
		if err != nil {
			d.Log.Error("setup check failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		name, err := instanceName(ctx, d.Store)
		if err != nil {
			d.Log.Error("read instance name failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &InstanceOutput{Body: InstanceBody{
			InstanceName: name,
			NeedsSetup:   needs,
			RememberDays: int(d.Config.SessionRememberLifetime / (24 * time.Hour)),
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "verifySetupToken",
		Method:      http.MethodPost,
		Path:        "/api/setup/token",
		Summary:     "Check the setup token and return the step 3 defaults",
		Tags:        []string{"setup"},
	}, rbac.Public, func(ctx context.Context, in *VerifySetupTokenInput) (*VerifySetupTokenOutput, error) {
		ip := httpx.ClientIP(ctx)
		record := func(action string, result audit.Result, reason string) error {
			return d.Audit.Record(ctx, audit.Entry{
				Actor:      audit.Actor{Type: audit.ActorAnonymous},
				Action:     action,
				TargetType: "setup",
				Result:     result,
				Reason:     reason,
				IP:         ip,
				UserAgent:  in.UserAgent,
			})
		}
		fail := func(err error) (*VerifySetupTokenOutput, error) {
			d.Log.Error("could not audit a setup attempt; refusing", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}

		needs, err := d.Gate.Check(ctx)
		if err != nil {
			d.Log.Error("setup check failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		if !needs {
			if err := record(audit.ActionSetupAttemptDenied, audit.ResultDenied, "setup_complete"); err != nil {
				return fail(err)
			}
			return nil, httpx.Problem(http.StatusConflict, problemSetupComplete, "Setup is already complete.")
		}

		if !d.Limiter.Allow(ip) {
			d.Log.Warn("setup token attempts rate limited", "ip", ip)
			if err := record(audit.ActionSetupTokenFailed, audit.ResultFailure, reasonRateLimited); err != nil {
				return fail(err)
			}
			return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many attempts. Wait a minute and try again.")
		}

		// The submitted value is compared and discarded: it is never logged,
		// audited or echoed (T-01-39).
		if !d.Gate.Verify(in.Body.Token) {
			d.Log.Warn("wrong setup token", "ip", ip)
			if err := record(audit.ActionSetupTokenFailed, audit.ResultFailure, reasonInvalidToken); err != nil {
				return fail(err)
			}
			return nil, httpx.Problem(http.StatusUnauthorized, problemInvalidSetupToken, "That setup token isn't valid.")
		}

		host, err := d.Hostname()
		if err != nil || host == "" {
			host = "bayplan"
		}
		return &VerifySetupTokenOutput{Body: SetupDefaultsBody{
			DefaultInstanceName: host,
			PublicURL:           d.Config.PublicURL,
			PublicURLLocked:     d.Config.PublicURL != "",
		}}, nil
	})

	// GET /setup is a page, not an operation: server-side lock plus the SPA (D-06).
	page := func(w http.ResponseWriter, req *http.Request) {
		needs, err := d.Gate.Check(req.Context())
		if err != nil {
			d.Log.Error("setup check failed", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if !needs {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, req, "/login", http.StatusFound)
			return
		}
		if d.SPA == nil {
			http.NotFound(w, req)
			return
		}
		d.SPA.ServeHTTP(w, req)
	}
	r.Exempt(http.MethodGet, "/setup", page)
	r.Exempt(http.MethodGet, "/setup/*", page)
}

// instanceName reads the stored instance name; it is empty before setup.
func instanceName(ctx context.Context, st *store.Store) (string, error) {
	v, err := st.RQ.GetSetting(ctx, settingInstanceName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
