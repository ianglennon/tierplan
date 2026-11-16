// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// TestIsOwnershipLabel: TierPlan's own bookkeeping labels are the ones every strip site drops. tierplan.keep and tierplan.updates are
// the user's labels (JAN-07, D-09; IMG-08, D-16), so they are not among them.
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
		{stacks.LabelUpdates, false},
		{"tierplan.updates", false},
		{"tierplan.keep", false},
		{"tierplan.keeper", true},
		// The ingress labels are the user's (LBL-05, D-22); TierPlan's own ingress bookkeeping is
		// tierplan.component, which is an ownership label outside the user prefix.
		{"tierplan.ingress.host", false},
		{"tierplan.ingress.api.port", false},
		{"tierplan.ingress.api.host", false},
		{ingress.LabelPrefix + "x", false},
		{stacks.LabelIngressPrefix + "x", false},
		{ingress.LabelComponent, true},
		{"tierplan.component", true},
		{"tierplan.ingress", true},
		{"tierplan.ingressfoo", true},
		{"Tierplan.Ingress.host", false},
		{"tierplan.INGRESS.host", true},
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

// TestIngressPrefixIsShared pins D-22: the bookkeeping filter and the label parser share one prefix,
// and TierPlan's own ingress bookkeeping key sits outside it, so it is never mistaken for a user's route.
func TestIngressPrefixIsShared(t *testing.T) {
	t.Parallel()
	if stacks.LabelIngressPrefix != ingress.LabelPrefix {
		t.Errorf("LabelIngressPrefix = %q, want ingress.LabelPrefix %q", stacks.LabelIngressPrefix, ingress.LabelPrefix)
	}
	if strings.HasPrefix(ingress.LabelComponent, ingress.LabelPrefix) {
		t.Errorf("LabelComponent %q starts with the user prefix %q", ingress.LabelComponent, ingress.LabelPrefix)
	}
	if !stacks.IsOwnershipLabel(ingress.LabelComponent) {
		t.Errorf("IsOwnershipLabel(%q) = false, want true", ingress.LabelComponent)
	}
}
