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
	"sync"
	"time"

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

// MirrorID names a server-side mirror. It is the repository's ID (a UUID), never a part of its
// address, so the directory a mirror lives in cannot be steered by what a person types.
type MirrorID string

// RefKind is what a ref names when a mirror is asked for it.
type RefKind string

// The kinds of ref a mirror can be asked for.
const (
	RefBranch RefKind = "branch"
	RefTag    RefKind = "tag"
	// RefCommit is a commit by its ID, in full or by a prefix of 7 to 40 hex characters.
	RefCommit RefKind = "commit"
)

// Commit is a commit a ref resolved to.
type Commit struct {
	// SHA is the full 40-hex commit ID.
	SHA  string
	When time.Time
}

// EntryKind is what a tree entry is.
type EntryKind string

// The kinds of tree entry. A symlink and a submodule are only ever reported, never followed.
const (
	EntryDir       EntryKind = "dir"
	EntryFile      EntryKind = "file"
	EntrySymlink   EntryKind = "symlink"
	EntrySubmodule EntryKind = "submodule"
)

// Entry is one thing in a folder of a commit.
type Entry struct {
	Name string
	Kind EntryKind
	// HasCompose is set on a folder that directly holds compose.yaml, compose.yml,
	// docker-compose.yaml or docker-compose.yml.
	HasCompose bool
}

// Dir is a folder of a commit: its entries, folders first and then by name, at most the limit it
// was read with.
type Dir struct {
	Path    string
	Entries []Entry
	// Total is how many entries the folder has; Capped says Entries holds fewer.
	Total  int
	Capped bool
}

// Client is what Bayplan asks of git.
type Client interface {
	// LsRemote lists the branches and tags of a remote without a clone. An error is a *Error (a
	// fixed Code and the host) or one of ErrURLUnsupported, ErrURLUserinfo, ErrCAInvalid and
	// ErrCATooLarge for input that was refused before any connection was made.
	LsRemote(ctx context.Context, r Remote) (RefList, error)
	// FetchRef brings one ref of r into the mirror m (created on first use) and returns the commit
	// it points at: a branch or tag is fetched alone, never every branch; a commit is looked up by
	// asking for it and then for all heads and tags. ErrRefInvalid for a name ValidRefName refuses;
	// otherwise a *Error (ref_missing, commit_missing, commit_ambiguous or a transport code) or the
	// input errors of LsRemote.
	FetchRef(ctx context.Context, m MirrorID, r Remote, kind RefKind, ref string) (Commit, error)
	// ReadDir lists the folder dir ("" is the root) of the commit sha in the mirror m, reading the
	// object database only. A commit the mirror does not hold is commit_missing and a folder the
	// commit does not hold, or a path that is a file, is folder_missing. limit caps the entries
	// returned (500 when it is not positive).
	ReadDir(ctx context.Context, m MirrorID, sha, dir string, limit int) (Dir, error)
	// FolderTree answers the hash of the tree object of the folder dir of the commit sha in the mirror m
	// (the commit's root tree for the root): it changes when any file under the folder changes and not
	// otherwise. folder_missing and commit_missing as for ReadDir.
	FolderTree(ctx context.Context, m MirrorID, sha, dir string) (string, error)
	// ReadFolder reads the files of the folder dir of the commit sha in the mirror m that want accepts
	// (it is given each file's path relative to the folder), by the commit's ID and from the object
	// database only; the other files are counted and the first ones named. A symlink or submodule is
	// reported and never followed. A *LimitError when a file or the files together break limits.
	ReadFolder(ctx context.Context, m MirrorID, sha, dir string, want func(rel string) bool, limits ReadLimits) (Folder, error)
	// CommitsTouching counts the commits reachable from head and not from base that changed the folder dir, newest
	// first (D-13), walking at most limit commits (DefaultTouchLimit when it is not positive). A base that is not an
	// ancestor of head is Touch.NotAncestor. commit_missing for a head the mirror does not hold.
	CommitsTouching(ctx context.Context, m MirrorID, head, base, dir string, limit int) (Touch, error)
}

// Options builds a GoGit.
type Options struct {
	// Policy is the egress policy every dial of the client goes through: it refuses link-local,
	// unspecified, multicast and cloud-metadata addresses, and the operator's extra ranges. The
	// zero value is the built-in one.
	Policy registry.EgressPolicy
	// Log receives what the client does, without addresses or secrets. Nil is slog.Default().
	Log *slog.Logger
	// Root is the directory the mirrors live in ({data}/git). Without it FetchRef and ReadDir
	// answer ErrNoMirrorRoot.
	Root string
	// Now is the clock the mirrors' last use is recorded on; nil is time.Now. A test passes its own so that the
	// sweep of unused mirrors needs no waiting.
	Now func() time.Time
}

// GoGit is the Client that uses go-git. NewGoGit installs the transport (InstallTransport) when no one has.
type GoGit struct {
	policy registry.EgressPolicy
	log    *slog.Logger
	root   string
	now    func() time.Time

	// The bounds of a call: the most bytes read for a listing and for a fetch, and how long the ssh
	// handshake may take. Fields so that a test can set small ones; the defaults are the constants.
	advertisementMax int64
	packMax          int64
	handshake        time.Duration

	mu    sync.Mutex
	locks map[MirrorID]*mirrorLock

	usedMu sync.Mutex
	// used is when each mirror was last fetched or read, since the process began; MirrorStats falls back to the
	// mirror's bayplan-last-used file for one not used since.
	used map[MirrorID]time.Time
}

// NewGoGit returns a client that dials through o.Policy. It installs the hardened transport (see
// InstallTransport) with o.Policy, so a client built where no one did cannot fall back to go-git's
// own, which has no egress policy, no size bound and speaks http, git and file. Installation happens
// once per process: the policy of the first client built, or of an earlier InstallTransport, is the
// one the https transport keeps (a call can still bring its own through the context).
func NewGoGit(o Options) *GoGit {
	InstallTransport(o.Policy)
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &GoGit{
		policy: o.Policy, log: log, root: o.Root, now: now,
		advertisementMax: advertisementBytesMax, packMax: packBytesMax, handshake: sshHandshakeTimeout,
		locks: map[MirrorID]*mirrorLock{}, used: map[MirrorID]time.Time{},
	}
}

var _ Client = (*GoGit)(nil)
