// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"cmp"
	"slices"
	"strings"
)

// What a credential was matched on (Match.On).
const (
	// OnPrefix is a namespace prefix that covers the image's path.
	OnPrefix = "prefix"
	// OnHost is a credential with no prefix, which covers its whole registry.
	OnHost = "host"
	// OnHubFallback is the Docker Hub fallback credential of a layer.
	OnHubFallback = "hub_fallback"
)

// What became of a candidate on the image's registry (Consideration.Result).
const (
	// ResultUsed is the credential a pull uses.
	ResultUsed = "used"
	// ResultShorterPrefix covers the image too, with a shorter prefix than the one used.
	ResultShorterPrefix = "shorter_prefix"
	// ResultOverriddenByEnv is an instance credential that the environment's credential of
	// the same specificity beat.
	ResultOverriddenByEnv = "overridden_by_environment"
	// ResultHubFallbackOverridden is a Docker Hub fallback that a more specific credential beat.
	ResultHubFallbackOverridden = "hub_fallback_overridden"
	// ResultNoMatch is a credential for the same registry that does not cover the image.
	ResultNoMatch = "no_match"
)

// hubDomain is the domain a reference to Docker Hub normalises to.
const hubDomain = "docker.io"

// Candidate is a stored credential as the resolver sees it: where it is kept and what it
// covers. It carries no secret and no address of one, so a resolver's answer can be shown to
// anyone who may read an environment.
type Candidate struct {
	ID, Name string
	// Layer is LayerInstance or LayerEnvironment.
	Layer string
	// EnvID and EnvName are empty for an instance credential.
	EnvID, EnvName string
	// Host is the registry host, Prefix the namespace below it; empty covers the whole registry.
	Host, Prefix string
	// HubFallback marks the layer's credential for every Docker Hub pull nothing more specific covers.
	HubFallback bool
}

// Consideration is one candidate on the image's registry and what became of it.
type Consideration struct {
	Candidate Candidate
	// Matches is whether the credential covers the image at all.
	Matches bool
	// Result is one of the Result constants.
	Result string
}

// Match is the answer of Select.
type Match struct {
	// Cred is the credential a pull uses; nil means the pull is anonymous.
	Cred *Candidate
	// On is OnPrefix, OnHost or OnHubFallback; empty when Cred is nil.
	On string
	// Considered holds every candidate for the image's registry, the one used first, then those
	// that cover the image from most to least specific, then those that do not.
	Considered []Consideration
}

// fit is a candidate that covers the image: how specific it is and what it matched on.
type fit struct {
	cand Candidate
	// spec is the number of path segments in the prefix: 0 for a whole registry, -1 for the
	// Docker Hub fallback, which loses to every real match.
	spec int
	on   string
}

// Select chooses the credential an image reference is pulled with (D-02, D-03, D-10). It is
// pure: no clock, no I/O, no secret. A credential applies only when its host is the image's
// registry (exact, case-insensitive: ghcr.io never covers ghcr.io.evil.example) and its prefix
// is empty or a whole-segment prefix of the image's path (acme covers acme/app, never
// acmecorp/app). The longest prefix wins across both layers, the environment's credential beats
// the instance's of the same specificity, and the Docker Hub fallback applies last, only to
// docker.io images. A docker.io credential with an empty prefix applies through its fallback
// flag alone. It never reorders cands.
func Select(cands []Candidate, ref Ref) Match {
	var fits []fit
	var misses []Candidate
	for _, c := range cands {
		if !strings.EqualFold(c.Host, ref.Domain) {
			continue
		}
		if spec, on, ok := cover(c, ref); ok {
			fits = append(fits, fit{cand: c, spec: spec, on: on})
		} else {
			misses = append(misses, c)
		}
	}
	slices.SortFunc(fits, func(a, b fit) int {
		return cmp.Or(
			cmp.Compare(b.spec, a.spec),
			cmp.Compare(layerRank(a.cand.Layer), layerRank(b.cand.Layer)),
			cmp.Compare(a.cand.ID, b.cand.ID),
		)
	})
	slices.SortFunc(misses, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	var m Match
	for i, f := range fits {
		res := ResultUsed
		if i == 0 {
			c := f.cand
			m.Cred, m.On = &c, f.on
		} else {
			res = loserResult(fits[0], f)
		}
		m.Considered = append(m.Considered, Consideration{Candidate: f.cand, Matches: true, Result: res})
	}
	for _, c := range misses {
		m.Considered = append(m.Considered, Consideration{Candidate: c, Matches: false, Result: ResultNoMatch})
	}
	return m
}

// cover reports whether a candidate for the image's registry covers the image, and how
// specifically.
func cover(c Candidate, ref Ref) (spec int, on string, ok bool) {
	hub := strings.EqualFold(ref.Domain, hubDomain)
	if c.Prefix != "" && coversPath(c.Prefix, ref.Path) {
		return strings.Count(c.Prefix, "/") + 1, OnPrefix, true
	}
	if hub {
		// On Docker Hub a credential reaches beyond its namespace only through the flag.
		if c.HubFallback {
			return -1, OnHubFallback, true
		}
		return 0, "", false
	}
	if c.Prefix == "" {
		return 0, OnHost, true
	}
	return 0, "", false
}

// coversPath reports whether prefix is the whole path or ends at a segment boundary in it.
func coversPath(prefix, path string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// layerRank orders the environment's credentials before the instance's.
func layerRank(layer string) int {
	if layer == LayerEnvironment {
		return 0
	}
	return 1
}

// loserResult says why a candidate that covers the image was not the one used.
func loserResult(winner, loser fit) string {
	switch {
	case loser.spec == winner.spec && loser.cand.Layer != winner.cand.Layer:
		return ResultOverriddenByEnv
	case loser.on == OnHubFallback:
		return ResultHubFallbackOverridden
	default:
		return ResultShorterPrefix
	}
}
