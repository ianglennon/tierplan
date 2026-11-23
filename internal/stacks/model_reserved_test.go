// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import "testing"

func TestNameReserved(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		target Target
		stack  string
		want   bool
	}{
		{"the proxy project on a standalone host", TargetStandalone, "tierplan-ingress", true},
		{"the same name on a swarm", TargetSwarm, "tierplan-ingress", false},
		{"a longer name on a standalone host", TargetStandalone, "tierplan-ingress-2", false},
		{"a prefix on a standalone host", TargetStandalone, "tierplan", false},
		{"a case variant is not the project (it is not a valid name either)", TargetStandalone, "Tierplan-Ingress", false},
		{"empty", TargetStandalone, "", false},
	} {
		if got := NameReserved(c.target, c.stack); got != c.want {
			t.Errorf("%s: NameReserved(%q, %q) = %v, want %v", c.name, c.target, c.stack, got, c.want)
		}
	}
	// The reserved name is itself a valid standalone name; reserving is a separate rule from NameValid.
	if !NameValid(TargetStandalone, "tierplan-ingress") {
		t.Error("tierplan-ingress should satisfy the standalone naming rule, so only NameReserved refuses it")
	}
}
