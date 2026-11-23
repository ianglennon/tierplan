// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// aliasCase is one pair of the golden table with the alias it must give.
type aliasCase struct {
	name, project, service, want string
}

func aliasTable() []aliasCase {
	a70 := strings.Repeat("a", 70)
	b70 := strings.Repeat("b", 70)
	return []aliasCase{
		{"two stacks, one service name: shop", "shop", "web", "shop-web-8edaa88d78dd11a4"},
		{"two stacks, one service name: blog", "blog", "web", "blog-web-569b37b7c7f0516f"},
		{"underscore and plus swap one way", "a_b+c", "a+b_c", "a-b-c-a-b-c-d098a70e2f2b8a39"},
		{"underscore and plus swap the other way", "a+b_c", "a_b+c", "a-b-c-a-b-c-1ff4599ecdc2f140"},
		{"upper-case project", "Shop", "web", "shop-web-525036b124c90592"},
		{"upper-case service", "shop", "Web", "shop-web-b1e4d4e012e44127"},
		{"service key starting with an underscore", "shop", "_web", "shop--web-5f789eeab5da8b5e"},
		{"service key starting with a hyphen", "shop", "-web", "shop--web-9d33811445397a72"},
		{"70-byte project is cut", a70, "web", strings.Repeat("a", 42) + "-web-4e956a31109efe3e"},
		{"70-byte service is cut", "shop", a70, "shop-" + strings.Repeat("a", 41) + "-aeb15ec13042dff9"},
		{"two 70-byte names share the room", a70, b70, strings.Repeat("a", 22) + "-" + strings.Repeat("b", 23) + "-2760c4193b3441a1"},
		{"71-byte project with the same 70-byte prefix", a70 + "x", b70, strings.Repeat("a", 22) + "-" + strings.Repeat("b", 23) + "-c639960b9853109d"},
		{"ordinary hyphenated names", "my-app", "api", "my-app-api-863ae9cd2f86e699"},
		{"one-byte names", "x", "y", "x-y-ce3890a816f5237a"},
		// A pair found by search to share the first 8 hex characters with the next one (T-25-47): with 16 they differ.
		{"a searched 32-bit collision, attacker", "victim-production-environment-frontend-st30015b038d7", "web-frontend", "victim-production-environment-fro-web-frontend-59acb4c6fe3ce18c"},
		{"a searched 32-bit collision, victim", "victim-production-environment-frontend-stack", "web-frontend", "victim-production-environment-fro-web-frontend-59acb4c658284bb3"},
	}
}

// TestAliasGolden pins D-15: the alias of a pair is a fixed string, because every routed standalone
// container carries it and a change recreates them all.
func TestAliasGolden(t *testing.T) {
	for _, c := range aliasTable() {
		t.Run(c.name, func(t *testing.T) {
			if got := Alias(c.project, c.service); got != c.want {
				t.Errorf("Alias(%q, %q) = %q, want %q", c.project, c.service, got, c.want)
			}
		})
	}
}

// TestAliasProperties checks, for the table and 200 generated pairs from a fixed seed, that every
// alias is a valid DNS label and that distinct pairs give distinct aliases (T-25-01, PROH-25-05).
func TestAliasProperties(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
	type pair struct{ p, s string }
	var pairs []pair
	for _, c := range aliasTable() {
		pairs = append(pairs, pair{c.project, c.service})
	}
	// The generator draws from characters that slug to the same byte or are cut alike, so collisions
	// of the slugs are likely and only the hash can tell the pairs apart.
	const alphabet = "abAB09-_+. "
	rng := rand.New(rand.NewSource(25)) //nolint:gosec // G404: a fixed seed for a reproducible corpus
	gen := func() string {
		n := 1 + rng.Intn(90)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	for range 200 {
		pairs = append(pairs, pair{gen(), gen()})
	}

	seen := map[string]pair{}
	for _, pr := range pairs {
		got := Alias(pr.p, pr.s)
		if len(got) > maxLabelLen {
			t.Errorf("Alias(%q, %q) = %q is %d bytes, over %d", pr.p, pr.s, got, len(got), maxLabelLen)
		}
		if !label.MatchString(got) {
			t.Errorf("Alias(%q, %q) = %q is not a DNS label", pr.p, pr.s, got)
		}
		if again := Alias(pr.p, pr.s); again != got {
			t.Errorf("Alias(%q, %q) is not a pure function: %q then %q", pr.p, pr.s, got, again)
		}
		if prev, dup := seen[got]; dup && prev != pr {
			t.Errorf("pairs %+v and %+v both give %q", prev, pr, got)
		}
		seen[got] = pr
	}
}
