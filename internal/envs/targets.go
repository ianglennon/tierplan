// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

// Targets returns the monitor keys an environment is read through.
func Targets(in RollupInput) []string {
	_ = in
	return nil
}

// Primary is the one target a read of a single engine goes to.
func Primary(in RollupInput) (string, bool) {
	_ = in
	return "", false
}
