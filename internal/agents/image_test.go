// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/agents"
)

// TestImageFor proves the shared tag rule: a release version gives its X.Y.Z (prerelease kept, build
// metadata dropped), anything else gives edge, and AgentImage is the same rule on the agent repository.
func TestImageFor(t *testing.T) {
	const repo = "ghcr.io/example/other"
	cases := []struct {
		server, tag string
	}{
		{"v1.2.3", "1.2.3"},
		{"1.2.3", "1.2.3"},
		{"1.2.3-rc.1+meta", "1.2.3-rc.1"},
		{"  v0.4.0 ", "0.4.0"},
		{"dev", "edge"},
		{"", "edge"},
		{"v1.2", "edge"},
		{"0.1.0-12-gabcdef", "0.1.0-12-gabcdef"},
	}
	for _, c := range cases {
		t.Run(c.server, func(t *testing.T) {
			if got, want := agents.ImageFor(repo, c.server), repo+":"+c.tag; got != want {
				t.Errorf("ImageFor(%q) = %q, want %q", c.server, got, want)
			}
			if got, want := agents.AgentImage(c.server), agents.ImageRepository+":"+c.tag; got != want {
				t.Errorf("AgentImage(%q) = %q, want %q", c.server, got, want)
			}
		})
	}
}

// TestSecretTag pins the exported tag to the one the agent's own secret name uses.
func TestSecretTag(t *testing.T) {
	const env = "0192f3a4-7b1c-7d2e-8f90-abc123def456"
	if got := agents.SecretTag(env); got != "abc123def456" {
		t.Errorf("SecretTag = %q, want abc123def456", got)
	}
	if got, want := agents.SecretName(env, 3), agents.SecretPrefix+"3-"+agents.SecretTag(env); got != want {
		t.Errorf("SecretName = %q, want %q", got, want)
	}
}
