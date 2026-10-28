// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/ianglennon/bayplan/internal/registry"
)

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

// ErrCAInvalid is what a certificate authority that is not a PEM certificate is refused with.
var ErrCAInvalid = errors.New("gitx: the certificate authority is not a PEM certificate")

// ErrCATooLarge is what a certificate authority over the registry limit (64 KiB) is refused with.
var ErrCATooLarge = errors.New("gitx: the certificate authority is larger than 64 KiB")

// ValidateCA checks a custom certificate authority bundle with the rule registry credentials use
// (Phase 9 D-05): empty is fine, else at most 64 KiB and at least one PEM certificate. It returns
// ErrCATooLarge or ErrCAInvalid.
func ValidateCA(pem string) error {
	switch {
	case pem == "":
		return nil
	case len(pem) > registry.MaxCAPEMBytes:
		return ErrCATooLarge
	case !registry.ValidCAPEM(pem):
		return ErrCAInvalid
	}
	return nil
}

// PresentedKey is an SSH host key a server presented: its type (ssh-ed25519), its SHA256
// fingerprint (SHA256:...) and the authorized_keys line to save when a person trusts it.
type PresentedKey struct {
	Type        string
	Fingerprint string
	Line        string
}

// HostKeyChangedError is what the host-key callback returns when the host has a trusted key of the
// presented key's type and the presented one differs. It is never accepted automatically.
type HostKeyChangedError struct {
	Host string
	// Saved is the fingerprint of the trusted key of the same type.
	Saved     string
	Presented PresentedKey
}

func (e *HostKeyChangedError) Error() string { return "gitx: the host key of " + e.Host + " changed" }

// HostKeyUntrustedError is what the host-key callback returns for a host with no trusted key.
type HostKeyUntrustedError struct {
	Host      string
	Presented PresentedKey
}

func (e *HostKeyUntrustedError) Error() string {
	return "gitx: the host key of " + e.Host + " is not trusted"
}

// Error is a failed git call: a code and the host it was made to. Its text carries nothing else.
type Error struct {
	Code Code
	Host string
	// Presented is the SSH host key the server showed, for host_key_untrusted and
	// host_key_changed: it is public material, so a person can compare it and trust it.
	Presented *PresentedKey
	// SavedFingerprint is the fingerprint of the trusted key a changed key differs from
	// (host_key_changed only).
	SavedFingerprint string
}

func (e *Error) Error() string { return fmt.Sprintf("git: %s (%s)", e.Code, e.Host) }

// Classify turns what a git call returned into an *Error. err is never copied or wrapped: only the
// code it maps to and host, because go-git's text can carry the address, a token or a response.
// Anything it does not know is CodeUnreachable.
func Classify(err error, host string) error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return already
	}
	var changed *HostKeyChangedError
	var untrusted *HostKeyUntrustedError
	switch {
	case errors.As(err, &changed):
		p := changed.Presented
		return &Error{Code: CodeHostKeyChanged, Host: host, Presented: &p, SavedFingerprint: changed.Saved}
	case errors.As(err, &untrusted):
		p := untrusted.Presented
		return &Error{Code: CodeHostKeyUntrusted, Host: host, Presented: &p}
	}
	return &Error{Code: codeOf(err), Host: host}
}

// codeOf maps an error to its code. The order matters: a blocked dial and a size cap are decided
// before the network errors they are wrapped in, and a certificate error before a timeout.
func codeOf(err error) Code {
	var (
		unknownAuthority x509.UnknownAuthorityError
		invalidCert      x509.CertificateInvalidError
		wrongHost        x509.HostnameError
		verification     *tls.CertificateVerificationError
		netErr           net.Error
	)
	switch {
	case errors.Is(err, registry.ErrEgressBlocked), errors.Is(err, ErrProtocolRefused):
		return CodeEgressBlocked
	case errors.Is(err, registry.ErrResponseTooLarge):
		return CodeTooLarge
	case errors.As(err, &unknownAuthority), errors.As(err, &invalidCert), errors.As(err, &wrongHost), errors.As(err, &verification):
		return CodeCertUntrusted
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed), errors.Is(err, transport.ErrInvalidAuthMethod), isSSHAuthFailure(err):
		return CodeAuthRefused
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return CodeNotFound
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		return CodeRefMissing
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return CodeTimeout
	}
	return CodeUnreachable
}
