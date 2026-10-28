// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// problemPublicURLLocked is the 409 returned when BAYPLAN_PUBLIC_URL fixes the
// public URL and a request asks for a different one (D-04).
const problemPublicURLLocked = "public_url_locked"

// problemAgentURLLocked is the 409 returned when BAYPLAN_AGENT_URL fixes the agent
// URL and a request asks for a different one (D-18).
const problemAgentURLLocked = "agent_url_locked"

// codeInvalidAgentURL is the field error code of a malformed agent URL. The SPA
// maps it to the same "Enter a full address" copy as invalid_public_url.
const codeInvalidAgentURL = "invalid_agent_url"

// codeInsecureAgentURL is the field error code of an agent URL that is plain http
// for a host that is not this machine while BAYPLAN_AGENT_ALLOW_INSECURE is off:
// agents refuse such an address. It is the same word as the 409 problem type of
// add and rotate (G-02-R4-3), defined once.
const codeInsecureAgentURL = problemInsecureAgentURL

// targetSettings is the audit target of a settings change.
const targetSettings = "settings"

// SettingsAgents is what the settings routes know of the agents: how many are
// enrolled, which of them are connected, and a way to tell the connected ones a new
// address when it changes (D-19). The tunnel hub and the agents service together
// are one.
type SettingsAgents interface {
	// EnrolledAgentIDs is the IDs of the agents that may connect: those not revoked.
	EnrolledAgentIDs(ctx context.Context) ([]string, error)
	// Connected reports whether the agent has a live session now.
	Connected(agentID string) bool
	// PushURLs tells every connected agent to dial urls (the first one first) and
	// to keep pins as the server's. It is best effort and returns at once.
	PushURLs(urls, pins []string) int
	// Pins is the server's certificate pins as a welcome carries them.
	Pins() []string
}

// SettingsDeps are the dependencies of the settings routes.
type SettingsDeps struct {
	Instance *instance.Instance
	Store    *store.Store
	Audit    *audit.Writer
	// Agents counts enrolled agents and tells connected ones a new address. Nil
	// means none: the counts are 0 and nothing is pushed.
	Agents SettingsAgents
	// AgentAllowInsecure is the server's development switch,
	// BAYPLAN_AGENT_ALLOW_INSECURE. Settings uses it only to say whether agents can
	// use the address they are given, and to refuse an agent URL they cannot.
	AgentAllowInsecure bool
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// SettingsBody is the instance's editable settings.
type SettingsBody struct {
	InstanceName         string `json:"instanceName" doc:"The name shown in the header, tab titles and the sign-in page"`
	PublicURL            string `json:"publicUrl" doc:"The URL users reach Bayplan at; drives the Origin check and the cookie Secure flag"`
	PublicURLLocked      bool   `json:"publicUrlLocked" doc:"True when the BAYPLAN_PUBLIC_URL environment variable fixes the public URL"`
	AgentURL             string `json:"agentUrl" doc:"The address agents dial when it differs from the public URL; empty means they dial the public URL"`
	AgentURLLocked       bool   `json:"agentUrlLocked" doc:"True when the BAYPLAN_AGENT_URL environment variable fixes the agent URL"`
	AgentEndpoint        string `json:"agentEndpoint" doc:"The WebSocket address agents are told to dial, built from the agent URL or else the public URL and ending in /agent/connect; empty when neither is set"`
	AgentEndpointRefused bool   `json:"agentEndpointRefused" doc:"True when agentEndpoint is plain ws:// for a host that is not this machine and BAYPLAN_AGENT_ALLOW_INSECURE is off. Agents refuse such an address and Bayplan refuses their plain connections, and adding an agent environment or rotating a join token answers 409 insecure_agent_url until the agent URL or the public URL is https"`
	EnrolledAgents       int    `json:"enrolledAgents" doc:"How many agents are enrolled and not revoked, across all environments. A change of the public URL or the agent URL needs a confirmation while this is above 0"`
	OfflineAgents        int    `json:"offlineAgents" doc:"How many of the enrolled agents have no connection now. They cannot be sent a new address, so after a change of the host or port agents dial they are refused until they are reinstalled with the new address or the change is reverted"`
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
		AgentURL     *string `json:"agentUrl,omitempty" doc:"New agent URL, an absolute http or https URL, or an empty string to clear it so agents dial the public URL. Refused with a 409 while BAYPLAN_AGENT_URL is set. An http agent URL for a host that is not this machine is refused with 422 insecure_agent_url unless BAYPLAN_AGENT_ALLOW_INSECURE is on"`
	}
}

// RegisterSettings registers GET and PATCH /api/settings: the instance name
// (D-03), the public URL (D-04) and the agent URL (D-18). All are global settings.
// A change of the address agents dial is pushed to the connected agents (D-19).
func RegisterSettings(r *httpx.Router, d SettingsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	RegisterShellSettings(r, d)
	RegisterStackHistorySettings(r, d)
	internal := func(what string, err error) error {
		d.Log.Error("settings request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	current := func() SettingsBody {
		endpoint := d.Instance.AgentEndpoint()
		return SettingsBody{
			InstanceName:         d.Instance.Name(),
			PublicURL:            d.Instance.PublicURL(),
			PublicURLLocked:      d.Instance.Locked(),
			AgentURL:             d.Instance.AgentURL(),
			AgentURLLocked:       d.Instance.AgentURLLocked(),
			AgentEndpoint:        endpoint,
			AgentEndpointRefused: tunnel.PlainAddressRefused(endpoint, d.AgentAllowInsecure),
		}
	}
	// withAgents adds how many agents are enrolled and how many of those have no
	// connection. A count that cannot be made is logged and left at zero: the
	// settings page is not worth less because the agents table was busy.
	withAgents := func(ctx context.Context, b SettingsBody) SettingsBody {
		if d.Agents == nil {
			return b
		}
		ids, err := d.Agents.EnrolledAgentIDs(ctx)
		if err != nil {
			d.Log.Warn("could not count the enrolled agents", "error", err)
			return b
		}
		b.EnrolledAgents = len(ids)
		for _, id := range ids {
			if !d.Agents.Connected(id) {
				b.OfflineAgents++
			}
		}
		return b
	}
	// Changes are rare and must reach the cache in the order they reach the
	// database, so one at a time.
	var mu sync.Mutex

	httpx.Register(r, huma.Operation{
		OperationID: "getSettings",
		Method:      http.MethodGet,
		Path:        "/api/settings",
		Summary:     "Get the instance name, public URL and agent URL",
		Description: "publicUrlLocked is true when the BAYPLAN_PUBLIC_URL environment variable fixes the public URL (D-04), " +
			"agentUrlLocked when BAYPLAN_AGENT_URL fixes the agent URL (D-18). agentEndpoint is the address agents dial: " +
			"the agent URL, else the public URL, with its path and /agent/connect. enrolledAgents counts the agents that are " +
			"not revoked and offlineAgents those of them with no connection now (D-19). agentEndpointRefused is true when " +
			"agentEndpoint is plain ws:// for a host that is not this machine and BAYPLAN_AGENT_ALLOW_INSECURE is off: agents " +
			"cannot connect to it (G-02-R4-3).",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(ctx context.Context, _ *struct{}) (*SettingsOutput, error) {
		return &SettingsOutput{CacheControl: "no-store", Body: withAgents(ctx, current())}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateSettings",
		Method:      http.MethodPatch,
		Path:        "/api/settings",
		Summary:     "Change the instance name, the public URL or the agent URL",
		Description: "Only the fields sent are changed. Invalid fields answer 422 with a stable code per field " +
			"(instance_name_required, instance_name_too_long, invalid_public_url, " + codeInvalidAgentURL + ", " + codeInsecureAgentURL + "). " +
			"An agent URL that is plain http for a host that is not this machine answers 422 " + codeInsecureAgentURL + " unless " +
			"BAYPLAN_AGENT_ALLOW_INSECURE is on, because agents refuse such an address; a loopback or https agent URL, clearing it, " +
			"and an http public URL are accepted. A different public URL while " +
			"BAYPLAN_PUBLIC_URL is set answers 409 public_url_locked, and a different agent URL while BAYPLAN_AGENT_URL is set " +
			"answers 409 " + problemAgentURLLocked + "; both change nothing. An empty agentUrl clears it. A change takes effect at once: " +
			"the Origin check and the cookie Secure flag use the new public URL from the next request. When the address agents " +
			"dial changes, every agent connected over a TLS-verified session is sent the new address and the server's pins over its " +
			"tunnel and dials that address from its next connection; an agent takes only a wss:// address this way. From then on " +
			"the server accepts agent handshakes only for the new host and port, with no fallback period: an agent that was " +
			"offline or on plain ws:// at the time is refused auth_rejected until it is reinstalled with the new address or the " +
			"change is reverted. Each change is " +
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
		name, url, agentURL := before.InstanceName, before.PublicURL, before.AgentURL
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

		if in.Body.AgentURL != nil {
			raw := strings.TrimSpace(*in.Body.AgentURL)
			u, err := config.NormalisePublicURL(raw)
			switch {
			case before.AgentURLLocked:
				// Echoing the fixed value back is not a change; anything else is refused.
				if err != nil || u != before.AgentURL {
					return nil, httpx.Problem(http.StatusConflict, problemAgentURLLocked, "The agent URL is set by the BAYPLAN_AGENT_URL environment variable.")
				}
			case err != nil:
				add("body.agentUrl", codeInvalidAgentURL)
			case u != agentURL && u != "" && tunnel.PlainAddressRefused(u, d.AgentAllowInsecure):
				// Agents refuse this address and so does Bayplan; saving it would
				// only hand them one they cannot dial (G-02-R4-3, T-02-148).
				add("body.agentUrl", codeInsecureAgentURL)
			case u != agentURL:
				details["agent_url_from"], details["agent_url_to"] = agentURL, u
				agentURL = u
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
			return &SettingsOutput{CacheControl: "no-store", Body: withAgents(ctx, before)}, nil
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
		if agentURL != before.AgentURL {
			if err := d.Instance.SetAgentURLTx(ctx, tx, d.Store.Q, agentURL); err != nil {
				if errors.Is(err, instance.ErrAgentURLLocked) {
					return nil, httpx.Problem(http.StatusConflict, problemAgentURLLocked, "The agent URL is set by the BAYPLAN_AGENT_URL environment variable.")
				}
				return nil, internal("store agent URL", err)
			}
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
		d.Instance.ApplyAgentURL(agentURL)
		after := current()
		// What agents dial changed, by either URL: every agent connected over a
		// TLS-verified session is sent the new address and the server's current pins
		// now and dials the new address from its next connection (D-19). The hub
		// accepts only the new host and port from now on, so agents that are offline
		// or on plain ws:// are refused until reinstalled or until the change is
		// reverted (option B, UAT G-02-4). A change that leaves the endpoint where it
		// was (the public URL under an agent URL, a name) tells nobody anything.
		if d.Agents != nil && after.AgentEndpoint != "" && after.AgentEndpoint != before.AgentEndpoint {
			n := d.Agents.PushURLs([]string{after.AgentEndpoint}, d.Agents.Pins())
			d.Log.Info("told the connected agents the new server address", "agents", n)
		}
		return &SettingsOutput{CacheControl: "no-store", Body: withAgents(ctx, after)}, nil
	})
}

// ShellSettingsBody is the instance's browser-shell limits (D-29, D-31).
type ShellSettingsBody struct {
	IdleMinutes int `json:"idleMinutes" minimum:"5" maximum:"240" doc:"How many minutes a shell may sit without input or output before it is closed. A warning is shown a minute before"`
	MaxPerUser  int `json:"maxPerUser" minimum:"1" maximum:"50" doc:"The most open shells one person may hold across all environments"`
}

// ShellSettingsOutput wraps ShellSettingsBody for huma.
type ShellSettingsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ShellSettingsBody
}

// UpdateShellSettingsInput changes the limits it names and leaves the other alone.
type UpdateShellSettingsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		IdleMinutes *int `json:"idleMinutes,omitempty" minimum:"5" maximum:"240" doc:"New idle limit in minutes, 5 to 240"`
		MaxPerUser  *int `json:"maxPerUser,omitempty" minimum:"1" maximum:"50" doc:"New per-person cap on open shells, 1 to 50"`
	}
}

// RegisterShellSettings registers GET and PATCH /api/settings/shells: how long an
// idle browser shell lives and how many one person may hold (D-29, D-31). Both are
// read from the instance on every use, so a change reaches open shells at their next
// idle check.
func RegisterShellSettings(r *httpx.Router, d SettingsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	current := func() ShellSettingsBody {
		return ShellSettingsBody{IdleMinutes: int(d.Instance.ShellIdle() / time.Minute), MaxPerUser: d.Instance.ShellCap()}
	}
	var mu sync.Mutex

	httpx.Register(r, huma.Operation{
		OperationID: "getShellSettings",
		Method:      http.MethodGet,
		Path:        "/api/settings/shells",
		Summary:     "Get the browser-shell limits",
		Description: "idleMinutes is how long a shell may sit without input or output (5 to 240, default 30) and maxPerUser how many open shells one person may hold across all environments (1 to 50, default 10).",
		Tags:        []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(_ context.Context, _ *struct{}) (*ShellSettingsOutput, error) {
		return &ShellSettingsOutput{CacheControl: "no-store", Body: current()}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateShellSettings",
		Method:      http.MethodPatch,
		Path:        "/api/settings/shells",
		Summary:     "Change the browser-shell limits",
		Description: "Only the fields sent are changed. A value outside its range answers 422. A change takes effect at once: open shells use the new idle limit at their next check, and the next open uses the new cap. " +
			"A request that changes nothing answers 200 and is not audited. Each change is audited as settings.updated with the old and new value of every changed field.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *UpdateShellSettingsInput) (*ShellSettingsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		mu.Lock()
		defer mu.Unlock()

		before := current()
		idle, maxN := before.IdleMinutes, before.MaxPerUser
		details := map[string]string{}
		if in.Body.IdleMinutes != nil && *in.Body.IdleMinutes != idle {
			details["shell_idle_minutes_from"], details["shell_idle_minutes_to"] = strconv.Itoa(idle), strconv.Itoa(*in.Body.IdleMinutes)
			idle = *in.Body.IdleMinutes
		}
		if in.Body.MaxPerUser != nil && *in.Body.MaxPerUser != maxN {
			details["shell_max_per_user_from"], details["shell_max_per_user_to"] = strconv.Itoa(maxN), strconv.Itoa(*in.Body.MaxPerUser)
			maxN = *in.Body.MaxPerUser
		}
		if len(details) == 0 {
			return &ShellSettingsOutput{CacheControl: "no-store", Body: before}, nil
		}
		fail := func(what string, err error) error {
			d.Log.Error("shell settings request failed", "step", what, "error", err)
			return huma.Error500InternalServerError("internal error")
		}
		tx, err := d.Store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, fail("begin", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed
		if err := d.Instance.SetShellLimitsTx(ctx, tx, d.Store.Q, idle, maxN); err != nil {
			return nil, fail("store shell limits", err)
		}
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionSettingsUpdated, TargetType: targetSettings, TargetID: targetSettings,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent, Details: details,
		}); err != nil {
			return nil, fail("audit shell settings change", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fail("commit", err)
		}
		d.Instance.ApplyShellLimits(idle, maxN)
		return &ShellSettingsOutput{CacheControl: "no-store", Body: current()}, nil
	})
}

// StackHistorySettingsBody is the instance's stack-history retention (D-01).
type StackHistorySettingsBody struct {
	Kept   int `json:"kept" minimum:"5" maximum:"200" doc:"How many revisions of each stack Bayplan keeps. The deployed revision and any revision still deploying are kept besides"`
	Min    int `json:"min" doc:"The lowest value Kept may have"`
	Max    int `json:"max" doc:"The highest value Kept may have"`
	Pruned int `json:"pruned" doc:"How many revisions this request deleted: those beyond a lowered limit, and 0 for a read or a limit that was not lowered"`
}

// StackHistorySettingsOutput wraps StackHistorySettingsBody for huma.
type StackHistorySettingsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackHistorySettingsBody
}

// UpdateStackHistorySettingsInput sets how many revisions are kept.
type UpdateStackHistorySettingsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Kept int `json:"kept" minimum:"5" maximum:"200" doc:"The new number of revisions kept per stack, 5 to 200"`
	}
}

// StackHistoryPreviewInput is the limit a preview counts for.
type StackHistoryPreviewInput struct {
	Keep int `query:"keep" required:"true" minimum:"5" maximum:"200" doc:"The limit to count for, 5 to 200"`
}

// StackHistoryPreviewBody is what a lower limit would delete.
type StackHistoryPreviewBody struct {
	Revisions int `json:"revisions" doc:"How many revisions the limit would delete now, across all stacks"`
	Stacks    int `json:"stacks" doc:"How many stacks those revisions belong to"`
}

// StackHistoryPreviewOutput wraps StackHistoryPreviewBody for huma.
type StackHistoryPreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackHistoryPreviewBody
}

// RegisterStackHistorySettings registers GET and PATCH /api/settings/stack-history and the prune preview: how
// many revisions of each stack Bayplan keeps (D-01). The limit is read at the end of every deploy and at startup,
// so a change reaches the next deploy that ends; lowering it also prunes at once.
func RegisterStackHistorySettings(r *httpx.Router, d SettingsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	current := func() StackHistorySettingsBody {
		return StackHistorySettingsBody{Kept: d.Instance.StackRevisionsKept(), Min: instance.MinStackRevisionsKept, Max: instance.MaxStackRevisionsKept}
	}
	var mu sync.Mutex

	httpx.Register(r, huma.Operation{
		OperationID: "getStackHistorySettings",
		Method:      http.MethodGet,
		Path:        "/api/settings/stack-history",
		Summary:     "Get how many revisions of each stack are kept",
		Description: "kept is how many revisions of each stack Bayplan keeps (5 to 200, default 20). The deployed revision and any revision still deploying are kept besides. Older revisions are deleted, with their variables and secret files.",
		Tags:        []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}, func(_ context.Context, _ *struct{}) (*StackHistorySettingsOutput, error) {
		return &StackHistorySettingsOutput{CacheControl: "no-store", Body: current()}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "updateStackHistorySettings",
		Method:      http.MethodPatch,
		Path:        "/api/settings/stack-history",
		Summary:     "Change how many revisions of each stack are kept",
		Description: "A value outside 5 to 200 answers 422. Lowering the limit deletes the older revisions of every stack at once, in the same transaction as the new value, and the answer's pruned says how many; " +
			"the deployed revision and any revision still deploying are never deleted. Deleted revisions cannot be recovered. A request that changes nothing answers 200 and is not audited. " +
			"Each change is audited as settings.updated with the old and new limit and the number of revisions deleted.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *UpdateStackHistorySettingsInput) (*StackHistorySettingsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		mu.Lock()
		defer mu.Unlock()

		before := current()
		if in.Body.Kept == before.Kept {
			return &StackHistorySettingsOutput{CacheControl: "no-store", Body: before}, nil
		}
		fail := func(what string, err error) error {
			d.Log.Error("stack history settings request failed", "step", what, "error", err)
			return huma.Error500InternalServerError("internal error")
		}
		tx, err := d.Store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, fail("begin", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op once committed
		if err := d.Instance.SetStackRevisionsKeptTx(ctx, tx, d.Store.Q, in.Body.Kept); err != nil {
			return nil, fail("store the limit", err)
		}
		// Only a lowered limit deletes: raising it never reaches for revisions it was not asked about. The
		// revisions go in the same transaction, so the limit and what it deleted stand or fall together.
		pruned := 0
		if in.Body.Kept < before.Kept {
			if pruned, err = (stackops.Repo{Store: d.Store}).EnforceKeepTx(ctx, tx, in.Body.Kept); err != nil {
				return nil, fail("prune the revisions", err)
			}
		}
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionSettingsUpdated, TargetType: targetSettings, TargetID: targetSettings,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{
				"stack_revisions_kept_from": strconv.Itoa(before.Kept),
				"stack_revisions_kept_to":   strconv.Itoa(in.Body.Kept),
				"pruned":                    strconv.Itoa(pruned),
			},
		}); err != nil {
			return nil, fail("audit the stack history settings change", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fail("commit", err)
		}
		// Only after the commit, so a rollback leaves the cache and the database in step.
		d.Instance.ApplyStackRevisionsKept(in.Body.Kept)
		out := current()
		out.Pruned = pruned
		return &StackHistorySettingsOutput{CacheControl: "no-store", Body: out}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewStackHistoryPrune",
		Method:      http.MethodGet,
		Path:        "/api/settings/stack-history/preview",
		Summary:     "Count the revisions a lower limit would delete",
		Description: "Answers how many revisions across how many stacks a limit of keep (5 to 200) would delete now, by the rule pruning applies: the newest keep of each stack, the deployed revision and any revision still deploying stay. " +
			"It only reads. Needs settings.write, as the change it previews does.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *StackHistoryPreviewInput) (*StackHistoryPreviewOutput, error) {
		revisions, stacks, err := (stackops.Repo{Store: d.Store}).PrunePreview(ctx, in.Keep)
		if err != nil {
			d.Log.Error("stack history preview failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &StackHistoryPreviewOutput{CacheControl: "no-store", Body: StackHistoryPreviewBody{Revisions: revisions, Stacks: stacks}}, nil
	})
}
