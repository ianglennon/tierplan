// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"errors"

	"github.com/compose-spec/compose-go/v2/types"
)

var (
	// ErrEnvFileMissing means a required env_file is not one of the stack's files.
	ErrEnvFileMissing = errors.New("stacks: an env_file is not one of the stack's files")
	// ErrEnvFileInvalid means an env_file could not be read as a .env file.
	ErrEnvFileInvalid = errors.New("stacks: an env_file could not be read")
)

// EnvFileError is the env_file that could not be resolved: the service, the path as the compose
// file wrote it and, for a file that could not be read, the 1-based line (0 when none is known).
// It never holds a value from the file.
type EnvFileError struct {
	Service string
	Path    string
	Line    int
	Err     error
}

func (e *EnvFileError) Error() string { return "stacks: service " + e.Service + ": " + e.Err.Error() }

// Unwrap returns ErrEnvFileMissing or ErrEnvFileInvalid.
func (e *EnvFileError) Unwrap() error { return e.Err }

// ResolveEnvFiles is a stub for the RED commit.
func ResolveEnvFiles(_ *types.Project, _ FileSet, _ func(string) (string, bool)) error {
	return nil
}
