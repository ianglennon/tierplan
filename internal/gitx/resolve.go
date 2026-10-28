// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import "github.com/go-git/go-git/v5"

// resolveCommitPrefix finds the one commit in the mirror whose ID starts with prefix.
func resolveCommitPrefix(repo *git.Repository, prefix, host string) (Commit, error) {
	return Commit{}, &Error{Code: CodeCommitMissing, Host: host}
}
