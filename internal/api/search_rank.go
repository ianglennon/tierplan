// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"slices"
	"strings"
)

// How well a result matches the query, best first (UI-SPEC "Search"): the name is the query, the name starts
// with it, the name holds it, or only the image or the stack of the result holds it.
const (
	rankExact = iota
	rankPrefix
	rankSubstring
	rankRelated
)

// minIDPrefix is the fewest characters of a container or image ID that count as asking for the ID.
const minIDPrefix = 4

// matchRank says how well q matches name, and whether it matches at all. extra are the other texts of the
// result that count (its image, its stack): holding q there is the weakest match. The comparison ignores case.
func matchRank(q, name string, extra ...string) (rank int, ok bool) {
	q = strings.ToLower(q)
	if q == "" {
		return 0, false
	}
	n := strings.ToLower(name)
	switch {
	case n == q:
		return rankExact, true
	case strings.HasPrefix(n, q):
		return rankPrefix, true
	case strings.Contains(n, q):
		return rankSubstring, true
	}
	for _, e := range extra {
		if strings.Contains(strings.ToLower(e), q) {
			return rankRelated, true
		}
	}
	return 0, false
}

// idPrefixMatch says whether q asks for the ID: four or more hexadecimal characters that start it, with the
// "sha256:" of an image ID left out. A shorter query, or one with a character that is not hexadecimal, never does.
func idPrefixMatch(q, id string) bool {
	q = strings.ToLower(q)
	if len(q) < minIDPrefix || strings.Trim(q, "0123456789abcdef") != "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimPrefix(id, "sha256:")), q)
}

// searchHit is a result and how well it matched.
type searchHit struct {
	SearchResult
	rank int
}

// rankResults orders hits best match first; equal matches go by environment name, name, type, and then
// environment ID and ID, so the same hits always come in the same order.
func rankResults(hits []searchHit) {
	slices.SortFunc(hits, func(a, b searchHit) int {
		return cmp.Or(
			cmp.Compare(a.rank, b.rank),
			strings.Compare(a.EnvName, b.EnvName), strings.Compare(a.Name, b.Name), strings.Compare(a.Type, b.Type),
			strings.Compare(a.EnvID, b.EnvID), strings.Compare(a.ID, b.ID),
		)
	})
}
