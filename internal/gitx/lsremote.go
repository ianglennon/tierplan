// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/ianglennon/bayplan/internal/registry"
)

// lsRemoteDeadline is how long one listing may take in all.
const lsRemoteDeadline = 30 * time.Second

const (
	headsPrefix = "refs/heads/"
	tagsPrefix  = "refs/tags/"
	peeledMark  = "^{}"
)

// LsRemote lists the branches and tags of r. The listing is made without a repository: go-git
// reads the server's advertisement into memory, and everything but heads and tags is dropped.
func (g *GoGit) LsRemote(ctx context.Context, r Remote) (RefList, error) {
	u, err := ValidateURL(r.URL)
	if err != nil {
		return RefList{}, err
	}
	if err := ValidateCA(r.CAPEM); err != nil {
		return RefList{}, err
	}
	if u.Scheme != "https" {
		// ssh is validated but not offered by this client yet: go-git would fall back to the
		// server user's own known_hosts (RESEARCH Pitfall 2), so it is refused until the host-key
		// and egress checks are in place.
		return RefList{}, &Error{Code: CodeUnreachable, Host: u.Host}
	}
	ctx, cancel := context.WithTimeout(registry.WithEgressPolicy(ctx, g.policy), lsRemoteDeadline)
	defer cancel()

	opts := &git.ListOptions{PeelingOption: git.AppendPeeled}
	if t, ok := r.Auth.(TokenAuth); ok && (t.Username != "" || t.Token != "") {
		opts.Auth = &githttp.BasicAuth{Username: t.Username, Password: t.Token}
	}
	if r.CAPEM != "" {
		opts.CABundle = []byte(r.CAPEM)
	}
	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{u.Raw}})
	refs, err := rem.ListContext(ctx, opts)
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return RefList{}, nil
		}
		return RefList{}, Classify(err, u.Host)
	}
	return refList(refs), nil
}

// refList keeps the heads and tags of an advertisement. An annotated tag is advertised twice, as
// refs/tags/v1 (the tag object) and refs/tags/v1^{} (the commit): the commit is kept. HEAD is a
// symbolic reference whose hash is zero; its target is the default branch.
func refList(refs []*plumbing.Reference) RefList {
	var out RefList
	peeled := map[string]string{}
	var headHash string
	for _, ref := range refs {
		name := ref.Name().String()
		switch {
		case name == "HEAD":
			if ref.Type() == plumbing.SymbolicReference {
				out.Default = strings.TrimPrefix(ref.Target().String(), headsPrefix)
			} else {
				headHash = ref.Hash().String()
			}
		case strings.HasPrefix(name, headsPrefix):
			out.Branches = append(out.Branches, Ref{Name: strings.TrimPrefix(name, headsPrefix), Kind: KindBranch, Commit: ref.Hash().String()})
		case strings.HasPrefix(name, tagsPrefix) && strings.HasSuffix(name, peeledMark):
			peeled[strings.TrimSuffix(strings.TrimPrefix(name, tagsPrefix), peeledMark)] = ref.Hash().String()
		case strings.HasPrefix(name, tagsPrefix):
			out.Tags = append(out.Tags, Ref{Name: strings.TrimPrefix(name, tagsPrefix), Kind: KindTag, Commit: ref.Hash().String()})
		}
	}
	for i, t := range out.Tags {
		if c, ok := peeled[t.Name]; ok {
			out.Tags[i].Commit = c
		}
	}
	byName := func(a, b Ref) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(out.Branches, byName)
	slices.SortFunc(out.Tags, byName)
	if out.Default == "" && headHash != "" {
		// A server that does not advertise where HEAD points: name the branch that is at HEAD,
		// preferring the conventional names.
		for _, want := range []string{"main", "master"} {
			if i := slices.IndexFunc(out.Branches, func(b Ref) bool { return b.Name == want && b.Commit == headHash }); i >= 0 {
				out.Default = want
				break
			}
		}
		if out.Default == "" {
			if i := slices.IndexFunc(out.Branches, func(b Ref) bool { return b.Commit == headHash }); i >= 0 {
				out.Default = out.Branches[i].Name
			}
		}
	}
	return out
}
