// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

// Referenced is the set of files of a repository folder that make up a stack (D-27).
type Referenced struct {
	// Paths are the stack's files, relative to the stack root, sorted and without repeats: the compose
	// files, the root .env and every env_file, secrets file: and configs file: path.
	Paths []string
	// Escapes are the references, as written, that name no path inside the stack (absolute, "..", a
	// drive). They are never in Paths.
	Escapes []string
	// Optional are the Paths that may be absent: the root .env, and an env_file with required: false.
	Optional []string
}

// ReferencedPaths names the files a stack needs.
func ReferencedPaths(loaded *Loaded, c WorkingCopy) Referenced { return Referenced{} }
