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
		AgentURL     *string `json:"agentUrl,omitempty" doc:"New agent URL, an absolute http or https URL, or an empty string to clear it so agents dial the public URL. Refused with a 409 while BAYPLAN_AGENT_URL is set"`
	}
}

// RegisterSettings registers GET and PATCH /api/settings: the instance name
// (D-03), the public URL (D-04) and the agent URL (D-18). All are global settings.
// A change of the address agents dial is pushed to the connected agents (D-19).
func RegisterSettings(r *httpx.Router, d SettingsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
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
			"(instance_name_required, instance_name_too_long, invalid_public_url, " + codeInvalidAgentURL + "). A different public URL while " +
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
