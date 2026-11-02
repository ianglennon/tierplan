// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitx_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// A server that accepts the connection and says nothing (WR-A01): the handshake cannot be allowed to run without an
// end, and the lock of the mirror cannot be left held by it.

// stalledServer accepts connections on a free loopback port and never sends a byte. accepted gets a value for each.
func stalledServer(t *testing.T) (url string, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{}, 16)
	conns := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
			ch <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for {
			select {
			case c := <-conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", ln.Addr())
	}
	return "ssh://git@127.0.0.1:" + strconv.Itoa(addr.Port) + "/acme/app.git", ch
}

// cancelOnAccept cancels the context once the server has accepted a connection, so the call is stuck in the handshake
// when it is cancelled.
func cancelOnAccept(t *testing.T, accepted <-chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	go func() {
		<-accepted
		cancel()
	}()
}

func TestSSHListingEndsWithItsContextWhenTheServerStallsTheHandshake(t *testing.T) {
	t.Parallel()
	url, accepted := stalledServer(t)
	signer, _ := deployKey(t)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	defer cancel()
	cancelOnAccept(t, accepted, cancel)

	_, err := gitx.NewGoGit(gitx.Options{}).LsRemote(ctx, gitx.Remote{URL: url, Auth: gitx.SSHKeyAuth{Signer: signer}})
	if err == nil {
		t.Fatal("LsRemote of a server that never answers succeeded")
	}
	_ = asError(t, err)
}

func TestSSHHandshakeHasADeadlineOfItsOwn(t *testing.T) {
	t.Parallel()
	url, _ := stalledServer(t)
	signer, _ := deployKey(t)
	g := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	// The context allows far longer than this; the handshake deadline is what ends the call.
	g.SetHandshakeTimeout(100 * time.Millisecond)
	remote := gitx.Remote{URL: url, Auth: gitx.SSHKeyAuth{Signer: signer}}

	_, err := g.LsRemote(testsupport.Context(t), remote)
	if ge := asError(t, err); ge.Code != gitx.CodeTimeout {
		t.Errorf("LsRemote error = %v, want timeout", err)
	}
	// A fetch is bounded the same way.
	_, err = g.FetchRef(testsupport.Context(t), gitx.MirrorID(uuid.NewString()), remote, gitx.RefBranch, "main")
	if ge := asError(t, err); ge.Code != gitx.CodeTimeout {
		t.Errorf("FetchRef error = %v, want timeout", err)
	}
}

// The fetch that was stuck lets go of the mirror when its context ends: every later fetch, read and removal gets
// through (it used to hold the write lock for the life of the process).
func TestSSHFetchStuckInTheHandshakeDoesNotKeepTheMirrorLocked(t *testing.T) {
	t.Parallel()
	url, accepted := stalledServer(t)
	signer, _ := deployKey(t)
	root := t.TempDir()
	g := gitx.NewGoGit(gitx.Options{Root: root})
	m := gitx.MirrorID(uuid.NewString())
	remote := gitx.Remote{URL: url, Auth: gitx.SSHKeyAuth{Signer: signer}}

	ctx, cancel := context.WithCancel(testsupport.Context(t))
	defer cancel()
	cancelOnAccept(t, accepted, cancel)
	if _, err := g.FetchRef(ctx, m, remote, gitx.RefBranch, "main"); err == nil {
		t.Fatal("FetchRef of a server that never answers succeeded")
	}

	done := make(chan error, 1)
	go func() { done <- g.RemoveMirror(m) }()
	if err := testsupport.Await(t, "RemoveMirror to get the lock the stuck fetch held", done); err != nil {
		t.Errorf("RemoveMirror: %v", err)
	}
	reads := make(chan error, 1)
	go func() {
		_, err := g.ReadDir(testsupport.Context(t), m, "0123456789012345678901234567890123456789", "", 0)
		reads <- err
	}()
	if err := testsupport.Await(t, "ReadDir to get the mirror", reads); err == nil {
		t.Error("ReadDir of a mirror that holds nothing succeeded")
	}
}

// Waiting for a mirror that another fetch holds ends with the caller's context.
func TestFetchWaitingForTheMirrorGivesUpWithItsContext(t *testing.T) {
	t.Parallel()
	url, accepted := stalledServer(t)
	signer, _ := deployKey(t)
	g := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	m := gitx.MirrorID(uuid.NewString())
	remote := gitx.Remote{URL: url, Auth: gitx.SSHKeyAuth{Signer: signer}}

	// The first fetch holds the mirror while it waits on the server.
	first, cancelFirst := context.WithCancel(testsupport.Context(t))
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, err := g.FetchRef(first, m, remote, gitx.RefBranch, "main")
		firstDone <- err
	}()
	_ = testsupport.Await(t, "the server to accept the first fetch", accepted)

	second, cancelSecond := context.WithCancel(testsupport.Context(t))
	cancelSecond()
	if _, err := g.FetchRef(second, m, remote, gitx.RefBranch, "main"); err == nil {
		t.Error("a second fetch with a cancelled context succeeded while the first held the mirror")
	}

	cancelFirst()
	_ = testsupport.Await(t, "the first fetch to end", firstDone)
}
