// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitsync_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/gitconn"
	"github.com/ianglennon/coaming/internal/gitsync"
	"github.com/ianglennon/coaming/internal/gitx"
	"github.com/ianglennon/coaming/internal/gitx/gittest"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/store/storetest"
	"github.com/ianglennon/coaming/internal/vault"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The sweep of the mirrors (plan 09-32, D-08): a mirror is only a cache, so the sweep removes the ones whose repository
// is gone, that nothing has used for 14 days, or that are too large; the clock is the test's.

type gcFixture struct {
	svc   *gitsync.Service
	conns *gitconn.Store
	git   *gitx.GoGit
	clock *checkClock
	root  string
	srv   *gittest.HTTPServer
	sha   string
}

func newGCFixture(t *testing.T) *gcFixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	sha := repo.Commit(t, map[string]string{"stacks/web/compose.yaml": webCompose}, "first")
	srv := gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	clock := &checkClock{t: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	g := gitx.NewGoGit(gitx.Options{Root: root, Now: clock.now})
	conns := gitconn.NewStore(st, v, nil)
	return &gcFixture{
		svc:   gitsync.New(gitsync.Deps{Git: g, Conns: conns, Repo: stackops.Repo{Store: st, Vault: v}, Now: clock.now}),
		conns: conns, git: g, clock: clock, root: root, srv: srv, sha: sha,
	}
}

// repo saves a repository and fetches it, which makes its mirror.
func (f *gcFixture) repo(t *testing.T, name string) gitx.MirrorID {
	t.Helper()
	r, err := f.conns.CreateRepo(context.Background(), gitconn.RepoInput{Name: name, URL: f.srv.URL, CAPEM: f.srv.CAPEM}, "admin", nil)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	m := gitx.MirrorID(r.ID)
	f.use(t, m, true)
	return m
}

// use fetches (or only reads) the mirror, which is a use of it.
func (f *gcFixture) use(t *testing.T, m gitx.MirrorID, fetch bool) {
	t.Helper()
	rem := gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM}
	if fetch {
		if _, err := f.git.FetchRef(context.Background(), m, rem, gitx.RefBranch, "main"); err != nil {
			t.Fatalf("FetchRef: %v", err)
		}
		return
	}
	if _, err := f.git.ReadDir(context.Background(), m, f.sha, "", 0); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
}

func (f *gcFixture) has(m gitx.MirrorID) bool {
	_, err := os.Stat(filepath.Join(f.root, string(m)+".git"))
	return err == nil
}

func TestMirrorGC(t *testing.T) {
	t.Parallel()
	f := newGCFixture(t)
	ctx := context.Background()
	gone := f.repo(t, "gone")
	idle := f.repo(t, "idle")
	busy := f.repo(t, "busy")
	if _, err := f.conns.DeleteRepo(ctx, "", string(gone), nil); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	// 13 days on, the busy one is read; the idle one has not been used since it was made.
	f.clock.advance(13 * 24 * time.Hour)
	f.use(t, busy, false)
	d := gitsync.GCDeps{Clock: stackops.Clock(fixedClock{f.clock})}
	removed, err := f.svc.SweepMirrors(ctx, d)
	if err != nil {
		t.Fatalf("SweepMirrors: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != gone || removed[0].Reason != gitsync.MirrorGone {
		t.Fatalf("first sweep removed %+v, want only the mirror whose repository was deleted", removed)
	}
	if f.has(gone) || !f.has(idle) || !f.has(busy) {
		t.Fatalf("on disk after the first sweep: gone=%v idle=%v busy=%v, want only gone removed", f.has(gone), f.has(idle), f.has(busy))
	}

	// Two more days: idle is 15 days unused, busy 2.
	f.clock.advance(2 * 24 * time.Hour)
	removed, err = f.svc.SweepMirrors(ctx, d)
	if err != nil {
		t.Fatalf("SweepMirrors: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != idle || removed[0].Reason != gitsync.MirrorUnused {
		t.Fatalf("second sweep removed %+v, want only the mirror unused for 15 days", removed)
	}
	if f.has(idle) || !f.has(busy) {
		t.Fatalf("on disk after the second sweep: idle=%v busy=%v", f.has(idle), f.has(busy))
	}

	// The mirror that was removed is built again on its next use.
	f.use(t, idle, true)
	if !f.has(idle) {
		t.Fatal("a fetch did not build the removed mirror again")
	}

	// A mirror over the size limit goes whether or not it is in use.
	d.MaxBytes = 1
	removed, err = f.svc.SweepMirrors(ctx, d)
	if err != nil {
		t.Fatalf("SweepMirrors: %v", err)
	}
	if len(removed) != 2 || removed[0].Reason != gitsync.MirrorTooLarge || removed[1].Reason != gitsync.MirrorTooLarge {
		t.Fatalf("third sweep removed %+v, want both mirrors, as too large", removed)
	}
}

// fixedClock is a stackops.Clock whose After is never used by a single sweep.
type fixedClock struct{ c *checkClock }

func (c fixedClock) Now() time.Time                       { return c.c.now() }
func (c fixedClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// The timed loop: it sweeps when it starts and then each period, on the test's clock.
func TestRunMirrorGCLoop(t *testing.T) {
	t.Parallel()
	f := newGCFixture(t)
	idle := f.repo(t, "idle")
	pc := newPollClock(f.clock)
	swept := make(chan []gitsync.MirrorRemoval, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.RunMirrorGC(ctx, gitsync.GCDeps{Clock: pc, Swept: func(r []gitsync.MirrorRemoval) { swept <- r }})
	}()
	t.Cleanup(func() {
		cancel()
		testsupport.Await(t, "the sweep to stop", done)
	})

	if first := testsupport.Await(t, "the first sweep", swept); len(first) != 0 {
		t.Fatalf("the sweep at start removed %+v, want nothing", first)
	}
	pc.Tick(t, gitsync.DefaultGCEvery)
	if next := testsupport.Await(t, "the sweep a day on", swept); len(next) != 0 || !f.has(idle) {
		t.Fatalf("a day on: removed %+v, mirror kept %v, want nothing removed", next, f.has(idle))
	}
	pc.Tick(t, gitsync.DefaultMirrorUnused)
	next := testsupport.Await(t, "the sweep two weeks on", swept)
	if len(next) != 1 || next[0].ID != idle || next[0].Reason != gitsync.MirrorUnused || f.has(idle) {
		t.Fatalf("two weeks on: removed %+v, mirror kept %v, want the idle mirror removed as unused", next, f.has(idle))
	}
}
