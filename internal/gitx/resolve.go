// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx

import (
	"errors"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// maxTagDepth is how many tags of tags are followed to reach a commit.
const maxTagDepth = 10

// resolveCommitPrefix finds the one commit in the mirror whose ID starts with prefix (lower case
// hex). It looks at IDs only, so a mirror with many commits is not decoded. No match is
// commit_missing and several are commit_ambiguous; neither picks one.
func resolveCommitPrefix(repo *git.Repository, prefix, host string) (Commit, error) {
	iter, err := repo.Storer.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		return Commit{}, &Error{Code: CodeCommitMissing, Host: host}
	}
	defer iter.Close()
	var found plumbing.Hash
	n := 0
	err = iter.ForEach(func(o plumbing.EncodedObject) error {
		if strings.HasPrefix(o.Hash().String(), prefix) {
			found = o.Hash()
			n++
		}
		return nil
	})
	switch {
	case err != nil:
		return Commit{}, &Error{Code: CodeCommitMissing, Host: host}
	case n > 1:
		return Commit{}, &Error{Code: CodeCommitAmbiguous, Host: host}
	case n == 0:
		return Commit{}, &Error{Code: CodeCommitMissing, Host: host}
	}
	c, ok := commitOf(repo, found)
	if !ok {
		return Commit{}, &Error{Code: CodeCommitMissing, Host: host}
	}
	return c, nil
}

// commitOf reads the commit with the ID h, if the mirror holds it.
func commitOf(repo *git.Repository, h plumbing.Hash) (Commit, bool) {
	c, err := repo.CommitObject(h)
	if err != nil {
		return Commit{}, false
	}
	return Commit{SHA: c.Hash.String(), When: c.Committer.When.UTC()}, true
}

// peel follows tag objects from h to the commit at their end.
func peel(repo *git.Repository, h plumbing.Hash) (plumbing.Hash, bool) {
	for range maxTagDepth {
		tag, err := repo.TagObject(h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return h, true
		}
		if err != nil || tag.TargetType == plumbing.InvalidObject {
			return plumbing.ZeroHash, false
		}
		h = tag.Target
	}
	return plumbing.ZeroHash, false
}

// resolveLocal reads the commit a ref of the mirror points at; an annotated tag is peeled to the
// commit it points at, never the tag object.
func resolveLocal(repo *git.Repository, name plumbing.ReferenceName, host string) (Commit, error) {
	ref, err := repo.Reference(name, true)
	if err != nil {
		return Commit{}, &Error{Code: CodeRefMissing, Host: host}
	}
	h, ok := peel(repo, ref.Hash())
	if !ok {
		return Commit{}, &Error{Code: CodeRefMissing, Host: host}
	}
	c, ok := commitOf(repo, h)
	if !ok {
		// A tag of a tree or a blob has no commit to browse.
		return Commit{}, &Error{Code: CodeRefMissing, Host: host}
	}
	return c, nil
}
