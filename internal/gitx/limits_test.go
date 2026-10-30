// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/coaming/internal/gitx"
	"github.com/ianglennon/coaming/internal/gitx/gittest"
)

// What a server may stream is bounded on every path (WR-A06): a listing is read into memory and a fetch goes to the
// disk the database lives on, so a server cannot keep sending. The bounds are set small here.

func TestSSHListingStopsAtItsLimit(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{})
	g.SetAdvertisementMax(100)

	_, err := g.LsRemote(context.Background(), f.remote(f.srv.HostKeyLine))
	if ge := asError(t, err); ge.Code != gitx.CodeTooLarge {
		t.Errorf("error = %v, want too_large", err)
	}
}

func TestSSHFetchStopsAtItsLimit(t *testing.T) {
	t.Parallel()
	f := newSSHFixture(t)
	g := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	g.SetPackMax(100)

	_, err := g.FetchRef(context.Background(), gitx.MirrorID(uuid.NewString()), f.remote(f.srv.HostKeyLine), gitx.RefBranch, "main")
	if ge := asError(t, err); ge.Code != gitx.CodeTooLarge {
		t.Errorf("error = %v, want too_large", err)
	}
}

// The bound of a listing over https is its own, below the one a packfile has.
func TestHTTPSListingStopsAtItsOwnLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	r := gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM}

	g := gitx.NewGoGit(gitx.Options{})
	if _, err := g.LsRemote(context.Background(), r); err != nil {
		t.Fatalf("LsRemote with the default limit: %v", err)
	}
	g.SetAdvertisementMax(100)
	_, err := g.LsRemote(context.Background(), r)
	if ge := asError(t, err); ge.Code != gitx.CodeTooLarge {
		t.Errorf("error = %v, want too_large", err)
	}
}
