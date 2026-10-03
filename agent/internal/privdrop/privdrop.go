// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package privdrop takes the agent from root to UID and GID 1000.
package privdrop

const (
	// UID and GID are the identity the agent runs as after the drop.
	UID = 1000
	GID = 1000

	// NoGroup is Result.GID when no supplementary group was joined.
	NoGroup = -1
)

// Options carries the inputs and, for tests, the syscalls. A nil function
// defaults to the real one.
type Options struct {
	SocketPath string
	DockerGID  *int

	Geteuid    func() int
	Stat       func(string) (gid int, err error)
	Setgroups  func([]int) error
	Setgid     func(int) error
	Setuid     func(int) error
	NoNewPrivs func() error
}

// Result reports what Drop did.
type Result struct {
	// Dropped is false when the process was not root and nothing changed.
	Dropped bool
	// GID is the supplementary group joined for the Docker socket, or NoGroup.
	GID int
}

// Drop is not implemented yet.
func Drop(o Options) (Result, error) {
	return Result{}, nil
}
