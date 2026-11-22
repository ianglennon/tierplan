// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package fixture

// Marked builds stacks.CheckInput literals that each decide the attach: the guard must pass them.
func Marked() {
	_ = stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm, Ingress: attach}

	// ingress: unattached, a fixture that reads the stack as it is
	_ = stacks.CheckInput{Name: "web", Target: stacks.TargetStandalone}

	// A registry.CheckInput is another type: the guard does not count it.
	_ = registry.CheckInput{Name: "web"}
}
