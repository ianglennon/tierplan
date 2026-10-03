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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Problem types the setup endpoints emit. The SPA maps them to the copy in
// UI-SPEC "Setup wizard": invalid_setup_token and rate_limited are step 1's two
// errors, setup_complete sends the browser to /login, and validation_failed
// carries the field codes below.
const (
	problemInvalidSetupToken = "invalid_setup_token"
	problemRateLimited       = "rate_limited"
	problemSetupComplete     = "setup_complete"
	problemValidationFailed  = "validation_failed"
)

// Field error codes returned in errors[].message by POST /api/setup/complete.
// They are stable: the SPA maps each to its UI-SPEC message.
const (
	codeInvalidUsername       = "invalid_username"
	codePasswordTooShort      = "password_too_short"
	codePasswordCommon        = "password_common"
	codeInstanceNameRequired  = "instance_name_required"
	codeInstanceNameTooLong   = "instance_name_too_long"
	codeInvalidPublicURL      = "invalid_public_url"
	maxInstanceNameCharacters = 64
)

// Audit reasons recorded with setup.token_failed.
const (
	reasonInvalidToken = "invalid_token"
	reasonRateLimited  = "rate_limited"
)

// SetupAuditor is what the setup routes need from the audit writer: Record for
// attempts that change nothing, and RecordTx so the completion's audit entry
// commits or rolls back with the account it describes. *audit.Writer
// implements it.
type SetupAuditor interface {
	httpx.Auditor
	RecordTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SetupDeps are the dependencies of the setup routes.
type SetupDeps struct {
	Gate    *auth.SetupGate
	Limiter *auth.IPLimiter
	Audit   SetupAuditor
	// AnonAudit records the refusals an anonymous caller can repeat without
	// limit: the 409 after setup is done and the 429 from the address limiter.
	// It is normally an auth.AuditThrottle, so a flood of them cannot starve the
	// single database writer while every refusal is still counted (WR-04). Nil
	// means Audit. The 401 for a wrong token always goes to Audit: it sits behind
	// the limiter, and each wrong token is its own row (D-05).
	AnonAudit httpx.Auditor
	Store     *store.Store
	Config    config.Config
	Instance  *instance.Instance
	Sessions  *auth.Sessions
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

// CompleteSetupInput is the one submission that creates the first admin. The
// length caps are generous buffers that bound what the server will read; the
// real limits are checked by the handler so each failure carries a stable code.
type CompleteSetupInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Token        string `json:"token" maxLength:"128" doc:"The setup token printed in the container log"`
		Username     string `json:"username" maxLength:"128" doc:"3 to 32 characters from a-z, 0-9, dot, dash and underscore; stored lower-case"`
		Password     string `json:"password" maxLength:"1024" doc:"At least 12 characters, not on the common-password list and not containing the username"`
		InstanceName string `json:"instanceName" maxLength:"256" doc:"Display name, 1 to 64 characters"`
		PublicURL    string `json:"publicUrl" maxLength:"2048" doc:"The address people use to reach Bayplan; ignored when BAYPLAN_PUBLIC_URL fixes it"`
	}
}

// SetupUserBody is the admin the completion created.
type SetupUserBody struct {
	ID       string `json:"id" doc:"The user's ID"`
	Username string `json:"username" doc:"The lower-cased username"`
	Role     string `json:"role" doc:"The user's global role"`
}

// CompleteSetupBody is the signed-in state the SPA needs after setup.
type CompleteSetupBody struct {
	User            SetupUserBody `json:"user" doc:"The admin account that was created"`
	CSRFToken       string        `json:"csrfToken" doc:"Sent as X-CSRF-Token on every mutating request"`
	InstanceName    string        `json:"instanceName" doc:"The stored instance name"`
	PublicURL       string        `json:"publicUrl" doc:"The public URL in effect"`
	PublicURLLocked bool          `json:"publicUrlLocked" doc:"True when BAYPLAN_PUBLIC_URL fixed the public URL and the submitted one was ignored"`
}

// CompleteSetupOutput carries the session cookie with the body.
type CompleteSetupOutput struct {
	SetCookie    http.Cookie `header:"Set-Cookie"`
	CacheControl string      `header:"Cache-Control"`
	Body         CompleteSetupBody
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
		return &InstanceOutput{Body: InstanceBody{
			InstanceName: d.Instance.Name(),
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
		if err := d.checkAccess(ctx, in.UserAgent, in.Body.Token); err != nil {
			return nil, err
		}
		host, err := d.Hostname()
		if err != nil || host == "" {
			host = "bayplan"
		}
		return &VerifySetupTokenOutput{Body: SetupDefaultsBody{
			DefaultInstanceName: host,
			PublicURL:           d.Instance.PublicURL(),
			PublicURLLocked:     d.Instance.Locked(),
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "completeSetup",
		Method:      http.MethodPost,
		Path:        "/api/setup/complete",
		Summary:     "Create the first admin, store the instance settings and sign in",
		Description: "Needs the setup token. Runs once: it creates the admin, assigns the global admin role, stores the " +
			"instance name and public URL, starts a session and audits setup.completed in one transaction, then " +
			"consumes the token. A second call answers 409 setup_complete.",
		Tags: []string{"setup"},
	}, rbac.Public, func(ctx context.Context, in *CompleteSetupInput) (*CompleteSetupOutput, error) {
		return d.complete(ctx, in)
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

// checkAccess is what both /api/setup/token and /api/setup/complete do first.
// In order it answers 409 once an admin exists, 429 when the caller's address
// has used its attempts, and 401 for a token that is not the active one. Each of
// those is audited. It returns nil only for the holder of the live token.
//
// The submitted value is compared and discarded: it is never logged, audited or
// echoed (T-01-39).
func (d SetupDeps) checkAccess(ctx context.Context, userAgent, token string) error {
	ip := httpx.ClientIP(ctx)
	record := func(rec httpx.Auditor, action string, result audit.Result, reason string) error {
		return rec.Record(ctx, audit.Entry{
			Actor:      audit.Actor{Type: audit.ActorAnonymous},
			Action:     action,
			TargetType: "setup",
			Result:     result,
			Reason:     reason,
			IP:         ip,
			UserAgent:  userAgent,
		})
	}
	var anon httpx.Auditor = d.Audit
	if d.AnonAudit != nil {
		anon = d.AnonAudit
	}
	fail := func(err error) error {
		d.Log.Error("could not audit a setup attempt; refusing", "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	needs, err := d.Gate.Check(ctx)
	if err != nil {
		d.Log.Error("setup check failed", "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	if !needs {
		if err := record(anon, audit.ActionSetupAttemptDenied, audit.ResultDenied, problemSetupComplete); err != nil {
			return fail(err)
		}
		return httpx.Problem(http.StatusConflict, problemSetupComplete, "Setup is already complete.")
	}

	if !d.Limiter.Allow(ip) {
		d.Log.Warn("setup token attempts rate limited", "ip", ip)
		if err := record(anon, audit.ActionSetupTokenFailed, audit.ResultFailure, reasonRateLimited); err != nil {
			return fail(err)
		}
		return httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many attempts. Wait a minute and try again.")
	}

	if !d.Gate.Verify(token) {
		d.Log.Warn("wrong setup token", "ip", ip)
		if err := record(d.Audit, audit.ActionSetupTokenFailed, audit.ResultFailure, reasonInvalidToken); err != nil {
			return fail(err)
		}
		return httpx.Problem(http.StatusUnauthorized, problemInvalidSetupToken, "That setup token isn't valid.")
	}
	return nil
}

// complete creates the first admin. Everything that must be true together
// happens in one immediate write transaction: the "no user yet" re-check, the
// user, the admin role, the settings, the session and the audit entry. Two
// callers holding the token race for that transaction and exactly one wins; the
// other finds a user and gets 409 (T-01-41).
func (d SetupDeps) complete(ctx context.Context, in *CompleteSetupInput) (*CompleteSetupOutput, error) {
	if err := d.checkAccess(ctx, in.UserAgent, in.Body.Token); err != nil {
		return nil, err
	}
	ip := httpx.ClientIP(ctx)
	internal := func(what string, err error) (*CompleteSetupOutput, error) {
		d.Log.Error("setup completion failed", "step", what, "error", err)
		return nil, huma.Error500InternalServerError("internal error")
	}

	c, problems := d.validate(in)
	if len(problems) > 0 {
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
	hash, err := auth.HashPassword(in.Body.Password)
	if err != nil {
		return internal("hash password", err)
	}
	userID, err := uuid.NewV7()
	if err != nil {
		return internal("generate user id", err)
	}

	tx, err := d.Store.Write.BeginTx(ctx, nil) // BEGIN IMMEDIATE: the writer lock is held from here
	if err != nil {
		return internal("begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	n, err := d.Store.Q.WithTx(tx).CountUsers(ctx)
	if err != nil {
		return internal("count users", err)
	}
	if n > 0 {
		// Someone else completed setup between the gate check and here. Release
		// the write connection before auditing: Record needs it.
		_ = tx.Rollback()
		if err := d.Audit.Record(ctx, audit.Entry{
			Actor:      audit.Actor{Type: audit.ActorAnonymous},
			Action:     audit.ActionSetupAttemptDenied,
			TargetType: "setup",
			Result:     audit.ResultDenied,
			Reason:     problemSetupComplete,
			IP:         ip,
			UserAgent:  in.UserAgent,
		}); err != nil {
			return internal("audit denied completion", err)
		}
		return nil, httpx.Problem(http.StatusConflict, problemSetupComplete, "Setup is already complete.")
	}

	now := time.Now().UTC().UnixNano()
	id := userID.String()
	if err := d.Store.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: id, Username: c.username, PasswordHash: hash,
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		return internal("insert user", err)
	}
	if err := rbac.AssignGlobalRole(ctx, tx, d.Store.Q, rbac.PrincipalUser, id, rbac.AdminRoleID); err != nil {
		return internal("assign admin role", err)
	}
	if err := d.Instance.SetTx(ctx, tx, d.Store.Q, c.instanceName, c.publicURL); err != nil {
		return internal("store instance settings", err)
	}
	rawSession, idHash, err := d.Sessions.CreateTx(ctx, tx, id, false, ip, in.UserAgent)
	if err != nil {
		return internal("create session", err)
	}
	if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor:      audit.Actor{Type: audit.ActorUser, ID: id, Name: c.username},
		Action:     audit.ActionSetupCompleted,
		TargetType: "user",
		TargetID:   id,
		Result:     audit.ResultSuccess,
		IP:         ip,
		UserAgent:  in.UserAgent,
		Details:    map[string]string{"username": c.username, "instance_name": c.instanceName},
	}); err != nil {
		return internal("audit completion", err)
	}
	if err := tx.Commit(); err != nil {
		return internal("commit", err)
	}

	// The account exists. Spend the token and let the Origin guard see the new
	// public URL now. A leftover token file is cleaned by the gate's own check on
	// the next request, so a failure here is logged, not fatal.
	if err := d.Gate.Consume(); err != nil {
		d.Log.Warn("could not remove the setup token file", "error", err)
	}
	d.Instance.Apply(c.instanceName, c.publicURL)

	return &CompleteSetupOutput{
		SetCookie:    d.Sessions.Cookie(rawSession, false, d.Instance.CookieScope()),
		CacheControl: "no-store",
		Body: CompleteSetupBody{
			User:            SetupUserBody{ID: id, Username: c.username, Role: rbac.AdminRoleID},
			CSRFToken:       d.Sessions.CSRFToken(idHash),
			InstanceName:    c.instanceName,
			PublicURL:       d.Instance.PublicURL(),
			PublicURLLocked: d.Instance.Locked(),
		},
	}, nil
}

// completion is the validated, normalised form of a CompleteSetupInput.
type completion struct {
	username     string
	instanceName string
	publicURL    string
}

// validate checks every field and reports all the problems at once, each as a
// stable code at the field's location. It never echoes a submitted value.
func (d SetupDeps) validate(in *CompleteSetupInput) (completion, []*huma.ErrorDetail) {
	var problems []*huma.ErrorDetail
	add := func(location, code string) {
		problems = append(problems, &huma.ErrorDetail{Location: location, Message: code})
	}
	var c completion

	username, err := auth.NormalizeUsername(in.Body.Username)
	if err != nil {
		add("body.username", codeInvalidUsername)
		username = "" // no username to look for inside the password
	}
	c.username = username

	switch err := auth.ValidatePassword(in.Body.Password, username); {
	case errors.Is(err, auth.ErrPasswordTooShort):
		add("body.password", codePasswordTooShort)
	case errors.Is(err, auth.ErrPasswordCommon):
		add("body.password", codePasswordCommon)
	}

	c.instanceName = strings.TrimSpace(in.Body.InstanceName)
	switch n := utf8.RuneCountInString(c.instanceName); {
	case n == 0:
		add("body.instanceName", codeInstanceNameRequired)
	case n > maxInstanceNameCharacters:
		add("body.instanceName", codeInstanceNameTooLong)
	}

	if d.Instance.Locked() {
		// BAYPLAN_PUBLIC_URL wins (D-04): what was submitted is not even looked at.
		c.publicURL = d.Instance.PublicURL()
	} else {
		u, err := config.NormalisePublicURL(strings.TrimSpace(in.Body.PublicURL))
		if err != nil || u == "" {
			add("body.publicUrl", codeInvalidPublicURL)
		}
		c.publicURL = u
	}
	return c, problems
}
