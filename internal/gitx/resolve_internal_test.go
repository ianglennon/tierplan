// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5"

	"github.com/ianglennon/coaming/internal/gitx/gittest"
)

// A prefix several commits start with is ambiguous, never the first match. Seven hex characters are
// too many to collide on purpose, so the resolver is asked for shorter prefixes, of which twenty
// commits are certain to share one (there are sixteen).
func TestResolveCommitPrefixAmbiguous(t *testing.T) {
	t.Parallel()
	r := gittest.NewRepo(t)
	byFirst := map[byte][]string{}
	for i := range 20 {
		sha := r.Commit(t, map[string]string{"f": fmt.Sprint(i)}, fmt.Sprintf("c%d", i))
		byFirst[sha[0]] = append(byFirst[sha[0]], sha)
	}
	repo, err := git.PlainOpen(r.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var shared, unique string
	for c, shas := range byFirst {
		switch {
		case len(shas) > 1 && shared == "":
			shared = string(c)
		case len(shas) == 1 && unique == "":
			unique = shas[0][:1]
		}
	}
	if shared == "" {
		t.Fatal("twenty commits and sixteen first characters: two must share one")
	}

	_, err = resolveCommitPrefix(repo, shared, "h")
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != CodeCommitAmbiguous {
		t.Errorf("resolveCommitPrefix(%q) = %v, want commit_ambiguous", shared, err)
	}
	if unique != "" {
		c, err := resolveCommitPrefix(repo, unique, "h")
		if err != nil || c.SHA[:1] != unique {
			t.Errorf("resolveCommitPrefix(%q) = %+v, %v, want the one commit", unique, c, err)
		}
	}
	if _, err := resolveCommitPrefix(repo, "ffffffffff", "h"); !errors.As(err, &ge) || ge.Code != CodeCommitMissing {
		t.Errorf("resolveCommitPrefix of nothing = %v, want commit_missing", err)
	}
}
