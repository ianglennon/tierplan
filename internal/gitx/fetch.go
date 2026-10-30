// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/ianglennon/coaming/internal/registry"
)

// fetchDeadline is how long one FetchRef may take in all, waiting for the mirror included.
const fetchDeadline = 10 * time.Minute

// ErrRefInvalid is what a ref ValidRefName refuses is answered with, before any git call.
var ErrRefInvalid = errors.New("gitx: the ref is not a valid name")

// maxRefName is the longest branch or tag name accepted, in bytes.
const maxRefName = 255

// ValidRefName says whether ref can be asked of a mirror as a ref of the kind: a branch or tag name
// by the rules of git check-ref-format (no control characters, space, ~ ^ : ? * [ or backslash, no
// "..", "@{", "//", no part starting with "." or ending in ".lock", no ending in "." or "/") and not
// starting with "-" or "/", at most 255 bytes; a commit by 7 to 40 hex characters. A name that
// passes cannot be read by git as an option or as part of a refspec.
func ValidRefName(kind RefKind, ref string) error {
	switch kind {
	case RefCommit:
		if len(ref) < 7 || len(ref) > 40 || strings.Trim(ref, "0123456789abcdefABCDEF") != "" {
			return ErrRefInvalid
		}
		return nil
	case RefBranch, RefTag:
	default:
		return ErrRefInvalid
	}
	if ref == "" || ref == "@" || len(ref) > maxRefName || !utf8.ValidString(ref) {
		return ErrRefInvalid
	}
	if ref[0] == '-' || ref[0] == '/' || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return ErrRefInvalid
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return ErrRefInvalid
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return ErrRefInvalid
		}
	}
	return nil
}

// FetchRef implements Client. The write lock of the mirror is held for the whole fetch, so a read
// never sees it half done; the commit it answers is read by its ID, which later fetches cannot
// change. Waiting for the lock ends with the context.
func (g *GoGit) FetchRef(ctx context.Context, m MirrorID, r Remote, kind RefKind, ref string) (Commit, error) {
	if err := ValidRefName(kind, ref); err != nil {
		return Commit{}, err
	}
	u, err := ValidateURL(r.URL)
	if err != nil {
		return Commit{}, err
	}
	if err := ValidateCA(r.CAPEM); err != nil {
		return Commit{}, err
	}
	dir, err := g.mirrorPath(m)
	if err != nil {
		return Commit{}, err
	}
	ctx, cancel := context.WithTimeout(registry.WithEgressPolicy(ctx, g.policy), fetchDeadline)
	defer cancel()

	mu := g.lockFor(m)
	if err := mu.lock(ctx); err != nil {
		return Commit{}, Classify(err, u.Host)
	}
	defer mu.unlock()
	// An ssh connection is Coaming's own and is closed when ctx ends (sshdial.go), so the fetch ends
	// with its deadline and the lock cannot be held past it.
	return g.fetchLocked(ctx, m, dir, r, u, kind, ref)
}

// fetchLocked is FetchRef with the write lock held.
func (g *GoGit) fetchLocked(ctx context.Context, m MirrorID, dir string, r Remote, u URL, kind RefKind, ref string) (Commit, error) {
	repo, err := openMirror(dir, originOf(u, r))
	if err != nil {
		g.log.Error("could not open a git mirror", "error", err)
		return Commit{}, &Error{Code: CodeUnreachable, Host: u.Host}
	}
	g.markUsed(m, dir)
	if kind == RefCommit {
		return g.fetchCommit(ctx, repo, r, u, strings.ToLower(ref))
	}
	name := plumbing.NewBranchReferenceName(ref)
	if kind == RefTag {
		name = plumbing.NewTagReferenceName(ref)
	}
	if err := g.fetch(ctx, repo, r, u, []config.RefSpec{trackedSpec(name)}); err != nil {
		return Commit{}, err
	}
	return resolveLocal(repo, name, u.Host)
}

// allRefs are the refspecs of the last resort for a commit: every head and tag.
var allRefs = []config.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// fetchCommit finds a commit in the mirror, fetching it only when the mirror lacks it. A commit is
// the same wherever it is found, so one the mirror holds is never fetched again. A full ID is
// asked for directly into a ref of its own, which servers such as GitHub allow even when no
// branch reaches it; when that fails, and for a prefix that cannot be asked for, every head and
// tag is fetched and the commit looked for among them.
func (g *GoGit) fetchCommit(ctx context.Context, repo *git.Repository, r Remote, u URL, ref string) (Commit, error) {
	full := len(ref) == 40
	find := func() (Commit, error) {
		if full {
			if c, ok := commitOf(repo, plumbing.NewHash(ref)); ok {
				return c, nil
			}
			return Commit{}, &Error{Code: CodeCommitMissing, Host: u.Host}
		}
		return resolveCommitPrefix(repo, ref, u.Host)
	}
	c, err := find()
	if err == nil || !isCode(err, CodeCommitMissing) {
		return c, err
	}
	if full {
		pinned := plumbing.ReferenceName("refs/coaming/pinned/" + ref)
		switch err := g.fetch(ctx, repo, r, u, []config.RefSpec{pinSpec(ref, pinned)}); {
		case err == nil:
			if c, err := find(); err == nil {
				return c, nil
			}
		case isHard(err):
			return Commit{}, err
		}
	}
	if err := g.fetch(ctx, repo, r, u, allRefs); err != nil {
		if isCode(err, CodeRefMissing) {
			// An empty repository has no commits.
			return Commit{}, &Error{Code: CodeCommitMissing, Host: u.Host}
		}
		return Commit{}, err
	}
	return find()
}

// pinSpec is the refspec that fetches the commit sha into the ref dst.
func pinSpec(sha string, dst plumbing.ReferenceName) config.RefSpec {
	return config.RefSpec("+" + sha + ":" + dst.String())
}

// isCode reports whether err is a *Error with the code.
func isCode(err error, c Code) bool {
	var ge *Error
	return errors.As(err, &ge) && ge.Code == c
}

// isHard reports whether err says asking again another way would fail the same way: the server
// cannot be reached or trusted, or refuses us. Anything else may only mean it will not hand out a
// commit by its ID.
func isHard(err error) bool {
	var ge *Error
	if !errors.As(err, &ge) {
		return false
	}
	switch ge.Code {
	case CodeAuthRefused, CodeNotFound, CodeCertUntrusted, CodeHostKeyChanged, CodeHostKeyUntrusted, CodeEgressBlocked, CodeTimeout, CodeTooLarge:
		return true
	}
	return false
}

// trackedSpec is the refspec that fetches exactly one ref into the same name in the mirror,
// forced because a branch may have been rewritten.
func trackedSpec(name plumbing.ReferenceName) config.RefSpec {
	return config.RefSpec("+" + name.String() + ":" + name.String())
}

// fetch runs one fetch of specs into repo from r. The address and the credential are passed in
// memory for this call: neither is written to the mirror's configuration. An mirror that is
// already current is a success.
func (g *GoGit) fetch(ctx context.Context, repo *git.Repository, r Remote, u URL, specs []config.RefSpec) error {
	opts := &git.FetchOptions{
		RemoteName: mirrorRemote, RemoteURL: u.Raw, RefSpecs: specs,
		Force: true, Tags: git.NoTags,
	}
	var probe *hostKeyProbe
	var call *sshCall
	if u.Scheme == "ssh" {
		if g.policy.RefusesHost(ctx, net.JoinHostPort(u.Host, strconv.Itoa(u.Port))) {
			return &Error{Code: CodeEgressBlocked, Host: u.Host}
		}
		var proxyOpts transport.ProxyOptions
		var done func()
		call, proxyOpts, done = newSSHCall(ctx, g.policy, g.packMax, g.handshake)
		defer done()
		method, p, err := g.sshAuth(r, u, call)
		if err != nil {
			return Classify(err, u.Host)
		}
		opts.Auth, opts.ProxyOptions, probe = method, proxyOpts, p
	} else {
		switch a := r.Auth.(type) {
		case nil:
		case TokenAuth:
			if a.Username != "" || a.Token != "" {
				opts.Auth = &githttp.BasicAuth{Username: a.Username, Password: a.Token}
			}
		default:
			// A deploy key does not sign in to an https address.
			return &Error{Code: CodeAuthRefused, Host: u.Host}
		}
	}
	if r.CAPEM != "" {
		opts.CABundle = []byte(r.CAPEM)
	}
	err := repo.FetchContext(ctx, opts)
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if probe != nil {
		if refusal := probe.refusal(); refusal != nil {
			return Classify(refusal, u.Host)
		}
	}
	if errors.Is(err, git.NoMatchingRefSpecError{}) || errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return &Error{Code: CodeRefMissing, Host: u.Host}
	}
	if call != nil {
		err = sshFailure(ctx, call, err)
	}
	return Classify(err, u.Host)
}
