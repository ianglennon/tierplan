// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import "fmt"

// Code is why a git call failed, as one of a fixed set. The screens word each one; no text from
// go-git, the network or the server ever leaves this package (RESEARCH Pitfall 16).
type Code string

// The failure codes.
const (
	CodeUnreachable      Code = "unreachable"
	CodeAuthRefused      Code = "auth_refused"
	CodeNotFound         Code = "not_found"
	CodeCertUntrusted    Code = "cert_untrusted"
	CodeHostKeyChanged   Code = "host_key_changed"
	CodeHostKeyUntrusted Code = "host_key_untrusted"
	CodeEgressBlocked    Code = "egress_blocked"
	CodeRefMissing       Code = "ref_missing"
	CodeTimeout          Code = "timeout"
	CodeTooLarge         Code = "too_large"
	// The next three are for the plans that read a mirror.
	CodeFolderMissing Code = "folder_missing"
	CodeFileMissing   Code = "file_missing"
	CodeCommitMissing Code = "commit_missing"
)

// Error is a failed git call: a code and the host it was made to. Its text carries nothing else.
type Error struct {
	Code Code
	Host string
}

func (e *Error) Error() string { return fmt.Sprintf("git: %s (%s)", e.Code, e.Host) }

// Classify turns what a git call returned into an *Error. err is never copied: only the code it
// maps to and host.
func Classify(err error, host string) error {
	if err == nil {
		return nil
	}
	return &Error{Code: CodeUnreachable, Host: host}
}
