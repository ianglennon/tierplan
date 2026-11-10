// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"slices"
	"strings"
)

// ChallengeHTTP01 and ChallengeDNS01 are the challenge types an install can issue through, declared
// here in one place. The values are the ones ACME defines, so they match Challenge.Type in a CA's
// authorization.
const ChallengeHTTP01 = "http-01"

// ChallengeDNS01 is the DNS-01 challenge: a TXT record at _acme-challenge.<name>. A public URL with
// a base path is refused for HTTP-01 only, never for this one (D-04).
const ChallengeDNS01 = "dns-01"

// Refusal codes: each is one reason an issue is refused before any order is made.
const (
	RefuseNamesIP        = "names_ip"
	RefuseAgentURLIP     = "agent_url_ip"
	RefuseNamesWildcard  = "names_wildcard"
	RefuseNamesNotPublic = "names_not_public"
	RefuseBasePath       = "base_path"
	RefuseAgentDialHost  = "agent_dial_host"
)

// refusalOrder is the stable order Preflight reports refusals in.
var refusalOrder = []string{
	RefuseNamesIP, RefuseAgentURLIP, RefuseNamesWildcard,
	RefuseNamesNotPublic, RefuseBasePath, RefuseAgentDialHost,
}

// Refusal is one reason an issue is refused. It carries data only; the English and the i18n
// copy live in the API problem detail and the SPA.
type Refusal struct {
	Code  string   `json:"code"`
	Host  string   `json:"host,omitempty"`
	Path  string   `json:"path,omitempty"`
	Count int      `json:"count,omitempty"`
	Hosts []string `json:"hosts,omitempty"`
}

// PreflightInput is everything Preflight decides from.
type PreflightInput struct {
	PublicURL, AgentURL string
	// BasePath is the path the public URL is served under, "" for none.
	BasePath      string
	ChallengeType string
	// DirectoryOverridden is set when the test-only directory override is in use, which skips the
	// names_not_public screen so Pebble can issue for a .test name (D-19).
	DirectoryOverridden bool
	// DialHosts is the host each connected agent session dialed (tunnel.Session.DialHost), one
	// entry per session.
	DialHosts []string
}

// Preflight decides, before anything is sent to a CA, the names the certificate would carry and
// every reason an issue must be refused. It returns all the refusals at once, in the fixed order
// of refusalOrder, so the UI can show the whole list. The returned NameSet carries the same list
// in its Refusals.
func Preflight(in PreflightInput) (NameSet, []Refusal) {
	ns := WantedNames(in.PublicURL, in.AgentURL, !in.DirectoryOverridden)
	refusals := slices.Clone(ns.Refusals)

	if p := strings.TrimRight(in.BasePath, "/"); p != "" && in.ChallengeType == ChallengeHTTP01 {
		refusals = append(refusals, Refusal{Code: RefuseBasePath, Path: in.BasePath})
	}

	uncovered := map[string]bool{}
	count := 0
	for _, h := range in.DialHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || slices.Contains(ns.Names, h) {
			continue
		}
		uncovered[h] = true
		count++
	}
	if count > 0 {
		hosts := make([]string, 0, len(uncovered))
		for h := range uncovered {
			hosts = append(hosts, h)
		}
		slices.Sort(hosts)
		refusals = append(refusals, Refusal{Code: RefuseAgentDialHost, Count: count, Hosts: hosts})
	}

	slices.SortStableFunc(refusals, func(a, b Refusal) int {
		return slices.Index(refusalOrder, a.Code) - slices.Index(refusalOrder, b.Code)
	})
	if len(refusals) == 0 {
		refusals = nil
	}
	ns.Refusals = refusals
	return ns, refusals
}
