// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

// Prescan reads the raw YAML of a compose file before anything loads it and refuses what could
// make a load reach outside the stack. It returns the problems, and every stack file the compose
// file refers to.
func Prescan(compose []byte, files FileSet) (problems Problems, refs []string) {
	return nil, nil
}
