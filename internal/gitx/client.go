// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package gitx is the one place go-git is imported. Everything else in Bayplan talks to git through
// the Client interface, so a different implementation (the git command line, for a repository
// go-git cannot read) can replace it without touching a caller (STACK.md).
//
// Every go-git, x509 and network error leaves this package as one fixed Code and the host, never
// as the text of the error: that text can carry the address, a token or a response body
// (RESEARCH Pitfall 16).
package gitx

import (
	"context"
	"log/slog"

	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/bayplan/internal/registry"
)

// Ref kinds.
const (
	KindBranch = "branch"
	KindTag    = "tag"
)

// Remote is a repository to talk to: the address, how to sign in and the certificate authority to
// trust in addition to the system's.
type Remote struct {
	// URL is an address that ValidateURL accepts.
	URL string
	// Auth is how to sign in; nil for a public repository.
	Auth Auth
	// CAPEM is a custom certificate authority bundle in PEM, for a self-hosted server. It is
	// added to the trusted roots for this one call; there is no option to skip verification
	// (Phase 9 D-05).
	CAPEM string
	// HostKeys are the SSH host keys a person trusted for this repository's host, one per entry.
	// They are ignored for a host that has built-in keys (BuiltinHostKeys) and for an https
	// address. An ssh address with no built-in and no saved key fails host_key_untrusted before
	// any credential is sent (Phase 9 D-03).
	HostKeys []HostKey
}

// HostKey is a trusted SSH host key in authorized_keys form (`ssh-ed25519 AAAA...`), one line.
type HostKey struct {
	Line string
}

// Auth is how to sign in to a remote. It is sealed: only this package defines the kinds.
type Auth interface {
	isAuth()
}

// TokenAuth is an HTTPS token (or password) with the user name the server expects with it.
type TokenAuth struct {
	Username string
	Token    string
}

func (TokenAuth) isAuth() {}

// String and GoString keep the token out of anything that formats the value (%v, %+v, %#v).
func (TokenAuth) String() string   { return "gitx.TokenAuth{redacted}" }
func (TokenAuth) GoString() string { return "gitx.TokenAuth{redacted}" }

// LogValue keeps the token out of structured logs.
func (TokenAuth) LogValue() slog.Value { return slog.StringValue("redacted") }

// SSHKeyAuth is an SSH deploy key with the user name the server expects (git on every large host).
type SSHKeyAuth struct {
	User   string
	Signer ssh.Signer
}

func (SSHKeyAuth) isAuth() {}

// String and GoString keep the key out of anything that formats the value (%v, %+v, %#v).
func (SSHKeyAuth) String() string   { return "gitx.SSHKeyAuth{redacted}" }
func (SSHKeyAuth) GoString() string { return "gitx.SSHKeyAuth{redacted}" }

// LogValue keeps the key out of structured logs.
func (SSHKeyAuth) LogValue() slog.Value { return slog.StringValue("redacted") }

// Ref is a branch or a tag and the commit it points at. For an annotated tag the commit is the one
// the tag object points at (the peeled commit), never the tag object itself.
type Ref struct {
	// Name is the short name: main, v1.2.
	Name string
	// Kind is KindBranch or KindTag.
	Kind string
	// Commit is the 40-hex commit ID.
	Commit string
}

// RefList is the branches and tags of a remote, each sorted by name. Other refs the server
// advertises (pull requests, notes) are dropped.
type RefList struct {
	Branches []Ref
	Tags     []Ref
	// Default is the short name of the branch HEAD points at; empty when the server does not say.
	Default string
	// HostKey is the SSH host key the server presented and Bayplan verified; nil over https.
	HostKey *PresentedKey
}

// Client is what Bayplan asks of git. Later plans add the mirror and read methods.
type Client interface {
	// LsRemote lists the branches and tags of a remote without a clone. An error is a *Error (a
	// fixed Code and the host) or one of ErrURLUnsupported, ErrURLUserinfo, ErrCAInvalid and
	// ErrCATooLarge for input that was refused before any connection was made.
	LsRemote(ctx context.Context, r Remote) (RefList, error)
}

// Options builds a GoGit.
type Options struct {
	// Policy is the egress policy every dial of the client goes through: it refuses link-local,
	// unspecified, multicast and cloud-metadata addresses, and the operator's extra ranges. The
	// zero value is the built-in one.
	Policy registry.EgressPolicy
	// Log receives what the client does, without addresses or secrets. Nil is slog.Default().
	Log *slog.Logger
}

// GoGit is the Client that uses go-git. InstallTransport must have run once in the process.
type GoGit struct {
	policy registry.EgressPolicy
	log    *slog.Logger
}

// NewGoGit returns a client that dials through o.Policy.
func NewGoGit(o Options) *GoGit {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &GoGit{policy: o.Policy, log: log}
}

var _ Client = (*GoGit)(nil)
