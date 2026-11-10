// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

// ChallengeHTTP01 is the challenge type this phase issues through.
const ChallengeHTTP01 = "http-01"

// Refusal codes: each is one reason an issue is refused before any order.
const (
	RefuseNamesIP        = "names_ip"
	RefuseAgentURLIP     = "agent_url_ip"
	RefuseNamesWildcard  = "names_wildcard"
	RefuseNamesNotPublic = "names_not_public"
	RefuseBasePath       = "base_path"
	RefuseAgentDialHost  = "agent_dial_host"
)

// Refusal is one reason an issue is refused.
type Refusal struct {
	Code  string   `json:"code"`
	Host  string   `json:"host,omitempty"`
	Path  string   `json:"path,omitempty"`
	Count int      `json:"count,omitempty"`
	Hosts []string `json:"hosts,omitempty"`
}

// PreflightInput is everything Preflight decides from.
type PreflightInput struct {
	PublicURL, AgentURL, BasePath, ChallengeType string
	DirectoryOverridden                          bool
	DialHosts                                    []string
}

// Preflight is not implemented yet.
func Preflight(in PreflightInput) (NameSet, []Refusal) { return NameSet{}, nil }
