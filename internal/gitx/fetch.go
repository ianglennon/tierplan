// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/ianglennon/bayplan/internal/registry"
)

// fetchDeadline is how long one FetchRef may take in all, waiting for the mirror included.
const fetchDeadline = 10 * time.Minute

// ErrRefInvalid is what a ref ValidRefName refuses is answered with, before any git call.
var ErrRefInvalid = errors.New("gitx: the ref is not a valid name")

// ValidRefName says whether ref can be asked of a mirror as a ref of the kind.
func ValidRefName(kind RefKind, ref string) error {
	if ref == "" {
		return ErrRefInvalid
	}
	return nil
}

// FetchRef implements Client. The write lock of the mirror is held for the whole fetch, so a read
// never sees it half done; the commit it answers is read by its ID, which later fetches cannot
// change.
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
	mu.Lock()
	if u.Scheme != "ssh" {
		defer mu.Unlock()
		return g.fetchLocked(ctx, dir, r, u, kind, ref)
	}
	// go-git connects over ssh without looking at the context, so the fetch runs on its own
	// goroutine and is abandoned when the deadline passes. It keeps the mirror's lock until it
	// ends, so an abandoned fetch cannot run beside the next one.
	type result struct {
		c   Commit
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer mu.Unlock()
		c, err := g.fetchLocked(ctx, dir, r, u, kind, ref)
		done <- result{c, err}
	}()
	select {
	case res := <-done:
		return res.c, res.err
	case <-ctx.Done():
		return Commit{}, Classify(ctx.Err(), u.Host)
	}
}

// fetchLocked is FetchRef with the write lock held.
func (g *GoGit) fetchLocked(ctx context.Context, dir string, r Remote, u URL, kind RefKind, ref string) (Commit, error) {
	repo, err := openMirror(dir)
	if err != nil {
		g.log.Error("could not open a git mirror", "error", err)
		return Commit{}, &Error{Code: CodeUnreachable, Host: u.Host}
	}
	if kind != RefBranch {
		return Commit{}, ErrRefInvalid
	}
	name := plumbing.NewBranchReferenceName(ref)
	if err := g.fetch(ctx, repo, r, u, []config.RefSpec{trackedSpec(name)}); err != nil {
		return Commit{}, err
	}
	return resolveLocal(repo, name, u.Host)
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
	if u.Scheme == "ssh" {
		if g.policy.RefusesHost(ctx, net.JoinHostPort(u.Host, strconv.Itoa(u.Port))) {
			return &Error{Code: CodeEgressBlocked, Host: u.Host}
		}
		method, p, err := g.sshAuth(r, u)
		if err != nil {
			return Classify(err, u.Host)
		}
		opts.Auth, probe = method, p
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
	return Classify(err, u.Host)
}

// resolveLocal reads the commit a ref of the mirror points at.
func resolveLocal(repo *git.Repository, name plumbing.ReferenceName, host string) (Commit, error) {
	ref, err := repo.Reference(name, true)
	if err != nil {
		return Commit{}, &Error{Code: CodeRefMissing, Host: host}
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return Commit{}, &Error{Code: CodeRefMissing, Host: host}
	}
	return Commit{SHA: c.Hash.String(), When: c.Committer.When.UTC()}, nil
}
