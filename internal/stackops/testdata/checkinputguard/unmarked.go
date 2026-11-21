// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package fixture

// Unmarked builds a stacks.CheckInput that decides nothing about the ingress network: the guard must report it.
func Unmarked() {
	_ = stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm}
}
