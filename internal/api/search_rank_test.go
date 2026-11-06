// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"slices"
	"testing"
)

// The order search results come in (plan 12-32, UI-SPEC "Search"): the name is the query, the name starts
// with it, the name holds it, then an image or stack that holds it; equal matches go by environment name,
// then name.
func TestSearchRanking(t *testing.T) {
	t.Parallel()
	t.Run("ranks", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			q, name string
			extra   []string
			rank    int
			ok      bool
		}{
			{"web", "web", nil, rankExact, true},
			{"WEB", "Web", nil, rankExact, true},
			{"web", "webapp", nil, rankPrefix, true},
			{"web", "my-web", nil, rankSubstring, true},
			{"web", "cache", []string{"web:1"}, rankRelated, true},
			{"web", "cache", []string{"redis:7", "WEB-STACK"}, rankRelated, true},
			{"web", "cache", []string{"redis:7"}, 0, false},
			{"", "cache", nil, 0, false},
		} {
			rank, ok := matchRank(tc.q, tc.name, tc.extra...)
			if rank != tc.rank || ok != tc.ok {
				t.Errorf("matchRank(%q, %q, %v) = %d, %v, want %d, %v", tc.q, tc.name, tc.extra, rank, ok, tc.rank, tc.ok)
			}
		}
	})

	hit := func(rank int, env, name, typ string) searchHit {
		return searchHit{SearchResult: SearchResult{EnvName: env, Name: name, Type: typ, EnvID: env, ID: name}, rank: rank}
	}
	rankOf := func(name string, extra ...string) int {
		rank, _ := matchRank("web", name, extra...)
		return rank
	}
	names := func(hits []searchHit) []string {
		out := make([]string, len(hits))
		for i, h := range hits {
			out[i] = h.EnvName + "/" + h.Name + "/" + h.Type
		}
		return out
	}

	t.Run("best match first, then environment, name and type", func(t *testing.T) {
		t.Parallel()
		hits := []searchHit{
			hit(rankOf("svc", "web:1"), "A", "svc", "service"),
			hit(rankOf("my-web"), "A", "my-web", "container"),
			hit(rankOf("webapp"), "B", "webapp", "stack"),
			hit(rankOf("web"), "B", "web", "service"),
			hit(rankOf("webapp"), "A", "webapp", "service"),
			hit(rankOf("webapp"), "A", "webapp", "container"),
			hit(rankOf("web"), "A", "web", "stack"),
		}
		want := []string{
			"A/web/stack", "B/web/service",
			"A/webapp/container", "A/webapp/service", "B/webapp/stack",
			"A/my-web/container",
			"A/svc/service",
		}
		rankResults(hits)
		if got := names(hits); !slices.Equal(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})

	t.Run("the same input gives the same order whatever order it comes in", func(t *testing.T) {
		t.Parallel()
		base := []searchHit{
			hit(1, "A", "x", "stack"), hit(1, "A", "x", "stack"), hit(1, "B", "x", "stack"), hit(0, "B", "y", "image"),
			hit(1, "A", "x", "service"), hit(2, "A", "a", "network"),
		}
		base[1].ID = "other"
		first := slices.Clone(base)
		rankResults(first)
		second := slices.Clone(base)
		slices.Reverse(second)
		rankResults(second)
		if !slices.Equal(names(first), names(second)) || first[1].ID != second[1].ID {
			t.Errorf("orders differ: %v and %v", names(first), names(second))
		}
	})
}

// Four or more hexadecimal characters ask for an ID; fewer, or anything else, never do.
func TestSearchIDPrefixRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		q, id string
		want  bool
	}{
		{"3f2a", "3f2a9c01deadbeef", true},
		{"3F2A", "3f2a9c01deadbeef", true},
		{"3f2", "3f2a9c01deadbeef", false},
		{"zzzz", "zzzz9c01deadbeef", false},
		{"3f2a", "93f2a9c01deadbeef", false},
		{"3f2a", "sha256:3f2a9c01deadbeef", true},
		{"sha2", "sha256:3f2a9c01deadbeef", false},
	} {
		if got := idPrefixMatch(tc.q, tc.id); got != tc.want {
			t.Errorf("idPrefixMatch(%q, %q) = %v, want %v", tc.q, tc.id, got, tc.want)
		}
	}
}
