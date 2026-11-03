// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// TestIsOwnershipLabel: TierPlan's own bookkeeping labels are the ones every strip site drops. tierplan.keep is the
// user's label (JAN-07, D-09), so it is not one of them.
func TestIsOwnershipLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key  string
		want bool
	}{
		{stacks.LabelStackID, true},
		{stacks.LabelStackImage, true},
		{stacks.LabelSecretPrefix + "db", true},
		{stacks.LabelConfigPrefix + "app", true},
		{stacks.LabelStoppedReplicas, true},
		{"tierplan.anything-else", true},
		{stacks.LabelKeep, false},
		{"tierplan.keep", false},
		{"tierplan.keeper", true},
		{"Tierplan.stack.id", false},
		{"com.docker.compose.project", false},
		{stacks.LabelSwarmNamespace, false},
		{"team", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			if got := stacks.IsOwnershipLabel(c.key); got != c.want {
				t.Errorf("IsOwnershipLabel(%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}
