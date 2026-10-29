// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"errors"

	"github.com/ianglennon/bayplan/internal/gitx"
)

// ReasonCode says why a git stack's sync is in error, or why it is behind with a note (D-16, D-19). The set is fixed
// here, once, for every plan that reads or produces one (the sync transitions, the auto-deploy and the screens map
// each to a sentence); a plan that needs a new code adds it to ReasonCodes, and the API's enum is made from that list.
type ReasonCode string

// The reason codes.
const (
	// ReasonFetchFailed: the repository could not be reached or refused the credential.
	ReasonFetchFailed ReasonCode = "fetch_failed"
	// ReasonHostKeyChanged: an ssh server presented a host key other than the trusted one.
	ReasonHostKeyChanged ReasonCode = "host_key_changed"
	// ReasonHostKeyUntrusted: an ssh server's host key has not been trusted.
	ReasonHostKeyUntrusted ReasonCode = "host_key_untrusted"
	// ReasonRefGone: the branch or tag the stack follows no longer exists.
	ReasonRefGone ReasonCode = "ref_gone"
	// ReasonFolderGone: the stack's folder is not in the repository at the head.
	ReasonFolderGone ReasonCode = "folder_gone"
	// ReasonChecksFailed: the new commit fails the lint, render or policy checks.
	ReasonChecksFailed ReasonCode = "checks_failed"
	// ReasonSourceChanged: the stack's repository, ref or folder is not the one its deployed revision came from.
	ReasonSourceChanged ReasonCode = "source_changed"
	// ReasonAutoDeployBlocked: an auto-deploy was skipped because something blocks it, such as unreviewed local edits.
	ReasonAutoDeployBlocked ReasonCode = "auto_deploy_blocked"
	// ReasonAutoDeployFailed: an auto-deploy ran and failed.
	ReasonAutoDeployFailed ReasonCode = "auto_deploy_failed"
)

// ReasonCodes is every reason code, in the order the screens list them.
func ReasonCodes() []ReasonCode {
	return []ReasonCode{
		ReasonFetchFailed, ReasonHostKeyChanged, ReasonHostKeyUntrusted, ReasonRefGone, ReasonFolderGone,
		ReasonChecksFailed, ReasonSourceChanged, ReasonAutoDeployBlocked, ReasonAutoDeployFailed,
	}
}

// autoDeployReason says whether a reason is one an automatic deploy leaves: the repository was read, and a commit waits
// for a person or failed to deploy.
func autoDeployReason(c ReasonCode) bool {
	return c == ReasonAutoDeployBlocked || c == ReasonAutoDeployFailed
}

// reasonOf maps a failed git call to a reason code. Nothing of the error's text is kept: only the code it maps to
// (gitx has already reduced it to a fixed set).
func reasonOf(err error) ReasonCode {
	var ge *gitx.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case gitx.CodeHostKeyChanged:
			return ReasonHostKeyChanged
		case gitx.CodeHostKeyUntrusted:
			return ReasonHostKeyUntrusted
		case gitx.CodeRefMissing:
			return ReasonRefGone
		case gitx.CodeFolderMissing:
			return ReasonFolderGone
		}
	}
	return ReasonFetchFailed
}

// detailOf is the fixed sentence stored with a reason: it names what failed and never carries text from the server, an
// address or a credential.
func detailOf(code ReasonCode, err error) string {
	var ge *gitx.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case gitx.CodeAuthRefused:
			return "The server refused the credential."
		case gitx.CodeNotFound:
			return "The server doesn't have this repository."
		case gitx.CodeCertUntrusted:
			return "The server's certificate isn't trusted."
		case gitx.CodeEgressBlocked:
			return "Bayplan isn't allowed to connect to this address."
		case gitx.CodeUnreachable:
			return "The server couldn't be reached."
		case gitx.CodeTimeout:
			return "The server took too long to answer."
		case gitx.CodeTooLarge:
			return "The repository is too large to fetch."
		case gitx.CodeCommitMissing, gitx.CodeCommitAmbiguous:
			return "The commit isn't in the repository."
		}
	}
	switch code {
	case ReasonHostKeyChanged:
		return "The server's host key changed."
	case ReasonHostKeyUntrusted:
		return "The server's host key isn't trusted yet."
	case ReasonRefGone:
		return "The branch or tag no longer exists."
	case ReasonFolderGone:
		return "The stack's folder isn't in the repository any more."
	case ReasonChecksFailed:
		return "The new commit doesn't pass the checks."
	case ReasonSourceChanged:
		return "The stack's source differs from the one that is deployed."
	case ReasonAutoDeployBlocked:
		return "An automatic deploy was held back."
	case ReasonAutoDeployFailed:
		return "The automatic deploy failed."
	}
	return "Couldn't fetch the repository."
}
