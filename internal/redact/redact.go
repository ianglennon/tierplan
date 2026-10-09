// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package redact keeps secret-bearing values from reaching a browser that may not
// see them (D-15a, AUTH-07). Masking happens on the server: an environment
// variable's value is never put in a response unless the caller holds
// secrets.reveal on that environment, so no amount of client-side hiding is
// relied on. Every endpoint that returns a container's or service's environment
// (the inspect views of Phase 4 and Phase 6) must send it through Env, with
// CanReveal deciding the second argument.
package redact

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/ianglennon/bayplan/internal/rbac"
)

// EnvVar is one environment variable as a browser receives it. Value is empty
// and omitted while Masked is true, so the masked form carries the name and
// nothing of the value, not even its length.
type EnvVar struct {
	Name   string `json:"name" doc:"The variable's name"`
	Value  string `json:"value,omitempty" doc:"The variable's value; absent while masked"`
	Masked bool   `json:"masked" doc:"True when the value was withheld because the caller may not reveal secrets on this environment"`
}

// Placeholder stands for a value withheld from a document that is sent as it is
// structured, such as the raw inspect of a container.
const Placeholder = "<hidden>"

// OptionVar is one option of a log driver as a browser receives it. The values of
// these options can hold credentials (a syslog address with a password, a Splunk
// token), so they are masked exactly as environment values are.
type OptionVar struct {
	Name   string `json:"name" doc:"The option's name"`
	Value  string `json:"value,omitempty" doc:"The option's value; absent while masked"`
	Masked bool   `json:"masked" doc:"True when the value was withheld because the caller may not reveal secrets on this environment"`
}

// Options turns a map of driver options into OptionVars sorted by name. With reveal
// false every value is withheld.
func Options(opts map[string]string, reveal bool) []OptionVar {
	out := make([]OptionVar, 0, len(opts))
	for _, name := range slices.Sorted(maps.Keys(opts)) {
		if reveal {
			out = append(out, OptionVar{Name: name, Value: opts[name]})
			continue
		}
		out = append(out, OptionVar{Name: name, Masked: true})
	}
	return out
}

// Env turns the "NAME=value" strings Docker reports into EnvVars, in the same
// order. The name is everything before the first "=", the value everything after
// it, so a value that holds "=" stays whole; an entry with no "=" is a name with
// an empty value. With reveal false every value is withheld.
func Env(env []string, reveal bool) []EnvVar {
	out := make([]EnvVar, 0, len(env))
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if reveal {
			out = append(out, EnvVar{Name: name, Value: value})
			continue
		}
		out = append(out, EnvVar{Name: name, Masked: true})
	}
	return out
}

// CanReveal says whether p may see secret values on envID: exactly when the
// Checker allows secrets.reveal there, which an Admin, an Operator granted the
// extra and an instance admin hold. A missing Checker or principal, and any
// failure to decide, is no.
func CanReveal(ctx context.Context, c *rbac.Checker, p *rbac.Principal, envID string) bool {
	if c == nil || p == nil {
		return false
	}
	return c.Check(ctx, p, rbac.Requirement{Perm: rbac.PermSecretsReveal, Scope: rbac.ScopeEnvironment}, envID).Allowed
}
