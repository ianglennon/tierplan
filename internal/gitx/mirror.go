// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// ErrNoMirrorRoot is what FetchRef and ReadDir fail with on a client built without Options.Root.
var ErrNoMirrorRoot = errors.New("gitx: no directory for the mirrors")

// ErrMirrorID is what a mirror ID that is not a canonical UUID is refused with, before anything
// is created on disk.
var ErrMirrorID = errors.New("gitx: a mirror is named by a repository ID")

// mirrorRemote is the one remote every mirror has. Its address is a placeholder: the address of the
// repository is passed to each fetch (FetchOptions.RemoteURL), so a mirror holds neither the
// address nor a credential, and moving a repository to another address cannot leave a stale one.
const (
	mirrorRemote    = "origin"
	mirrorRemoteURL = "https://mirror.invalid/"
)

// mirrorRootMode is the permission of the mirrors directory: only the server reads what is fetched.
const mirrorRootMode = 0o700

// lockFor returns the lock of mirror m: a fetch takes it for writing and every read for reading, so
// a read never sees a fetch half done. The locks live as long as the client. Waiting for a lock gives
// up with the caller's context (WR-A01).
func (g *GoGit) lockFor(m MirrorID) *mirrorLock {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[m]
	if !ok {
		l = newMirrorLock()
		g.locks[m] = l
	}
	return l
}

// readDeadline is how long a read of a mirror (waiting for it included) may take. Every reader has one
// of its own, so none is left to the caller: a read holds the mirror's read lock, which a fetch of the
// same repository waits for.
const readDeadline = 30 * time.Second

// readLock takes the read lock of mirror m, waiting no longer than ctx or readDeadline. It returns the
// context the read runs under, and the function that ends it and gives the lock back.
func (g *GoGit) readLock(ctx context.Context, m MirrorID) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, readDeadline)
	mu := g.lockFor(m)
	if err := mu.rlock(ctx); err != nil {
		cancel()
		return ctx, func() {}, Classify(err, "")
	}
	return ctx, func() { mu.runlock(); cancel() }, nil
}

// mirrorPath is {root}/{id}.git. The ID must be a canonical UUID, so no part of it can climb out
// of the root or name another mirror.
func (g *GoGit) mirrorPath(m MirrorID) (string, error) {
	if g.root == "" {
		return "", ErrNoMirrorRoot
	}
	id, err := uuid.Parse(string(m))
	if err != nil || id.String() != string(m) {
		return "", ErrMirrorID
	}
	return filepath.Join(g.root, string(m)+".git"), nil
}

// originFile is the file in a mirror that holds the digest of the destination it was filled from (see
// originOf).
const originFile = "bayplan-origin"

// originOf is a digest of what a mirror's contents come from: the address, the certificate authority
// and who signs in (the user name of a token, the public key of a deploy key; never a secret). A mirror
// keeps every object and ref of the last origin it was fetched from, so one that is fetched from
// another origin would answer a pinned commit, or a short ID, from the old repository (WR-A05). The
// digest, not the address, is what the mirror holds.
func originOf(u URL, r Remote) string {
	h := sha256.New()
	field := func(s string) {
		_, _ = h.Write([]byte(strconv.Itoa(len(s)) + ":" + s))
	}
	field(u.Scheme)
	field(u.Host)
	field(strconv.Itoa(u.Port))
	field(u.Path)
	field(r.CAPEM)
	switch a := r.Auth.(type) {
	case TokenAuth:
		field("token")
		field(a.Username)
	case SSHKeyAuth:
		field("key")
		if a.Signer != nil {
			field(ssh.FingerprintSHA256(a.Signer.PublicKey()))
		}
	default:
		field("none")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// openMirror opens the bare repository at dir for a fetch from the origin with the digest origin,
// creating it (and the remote a fetch needs) when it is not there, which includes a mirror whose
// directory was removed. A mirror that was filled from another origin, or whose origin is not
// recorded, is removed and made again: it is only a cache. The caller holds the write lock of the
// mirror. Nothing is ever checked out: the mirror is only an object database.
func openMirror(dir, origin string) (*git.Repository, error) {
	if _, err := os.Stat(dir); err == nil {
		recorded, _ := os.ReadFile(filepath.Join(dir, originFile)) //nolint:gosec // G304: path is under the mirror's own directory
		if string(recorded) != origin {
			if err := os.RemoveAll(dir); err != nil {
				return nil, fmt.Errorf("gitx: reset the mirror: %w", err)
			}
		}
	}
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		if err := os.MkdirAll(filepath.Dir(dir), mirrorRootMode); err != nil {
			return nil, fmt.Errorf("gitx: create the mirrors directory: %w", err)
		}
		repo, err = git.PlainInit(dir, true)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, originFile), []byte(origin), 0o600)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gitx: open the mirror: %w", err)
	}
	if _, err := repo.Remote(mirrorRemote); errors.Is(err, git.ErrRemoteNotFound) {
		_, err = repo.CreateRemote(&config.RemoteConfig{Name: mirrorRemote, URLs: []string{mirrorRemoteURL}})
		if err != nil {
			return nil, fmt.Errorf("gitx: create the mirror's remote: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("gitx: read the mirror's remote: %w", err)
	}
	return repo, nil
}

// openExisting opens a mirror for reading; a mirror that is not there is ErrMirrorMissing.
func openExisting(dir string) (*git.Repository, error) {
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, errMirrorMissing
	}
	if err != nil {
		return nil, fmt.Errorf("gitx: open the mirror: %w", err)
	}
	return repo, nil
}

var errMirrorMissing = errors.New("gitx: the mirror is not there")

// lastUsedFile is the file in a mirror whose modification time says when it was last fetched or read, so the
// answer survives a restart.
const lastUsedFile = "bayplan-last-used"

// usedWriteEvery is how often a busy mirror's lastUsedFile is written again; the in-memory time is always current.
const usedWriteEvery = time.Hour

// Mirrors is what a sweep of the mirror directory needs of a Client: which mirrors there are, how big and how old
// each is, and a way to remove one. A mirror is only a cache of a repository, so removing it loses nothing: the
// next fetch builds it again.
type Mirrors interface {
	// ListMirrors names the mirrors on disk, by ID.
	ListMirrors() ([]MirrorID, error)
	// MirrorStats is the size in bytes of mirror m and when it was last fetched or read.
	MirrorStats(m MirrorID) (size int64, lastUsed time.Time, err error)
	// RemoveMirror deletes mirror m, waiting for a fetch or read of it to end. One that is not there is not an error.
	RemoveMirror(m MirrorID) error
}

var _ Mirrors = (*GoGit)(nil)

// markUsed records that mirror m (at dir, which exists) was used now. The file is written again only when the
// last write is old, so a read does not touch the disk.
func (g *GoGit) markUsed(m MirrorID, dir string) {
	now := g.now()
	g.usedMu.Lock()
	prev, seen := g.used[m]
	g.used[m] = now
	g.usedMu.Unlock()
	if seen && now.Sub(prev) < usedWriteEvery && now.After(prev) {
		return
	}
	path := filepath.Join(dir, lastUsedFile)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // G304: path is under the mirror's own directory
		_ = f.Close()
	} else {
		return
	}
	_ = os.Chtimes(path, now, now)
}

// ListMirrors implements Mirrors.
func (g *GoGit) ListMirrors() ([]MirrorID, error) {
	if g.root == "" {
		return nil, ErrNoMirrorRoot
	}
	entries, err := os.ReadDir(g.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitx: list the mirrors: %w", err)
	}
	var out []MirrorID
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".git")
		if !ok || !e.IsDir() {
			continue
		}
		if id, err := uuid.Parse(name); err != nil || id.String() != name {
			continue
		}
		out = append(out, MirrorID(name))
	}
	return out, nil
}

// MirrorStats implements Mirrors. The last use is the newer of the time this process saw and the one the mirror's
// file records; a mirror with neither is as old as its directory.
func (g *GoGit) MirrorStats(m MirrorID) (int64, time.Time, error) {
	dir, err := g.mirrorPath(m)
	if err != nil {
		return 0, time.Time{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("gitx: read the mirror: %w", err)
	}
	last := info.ModTime()
	if fi, err := os.Stat(filepath.Join(dir, lastUsedFile)); err == nil {
		last = fi.ModTime()
	}
	g.usedMu.Lock()
	if t, ok := g.used[m]; ok && t.After(last) {
		last = t
	}
	g.usedMu.Unlock()
	var size int64
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that went while the walk ran (a fetch tidying up) is not an error.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("gitx: measure the mirror: %w", err)
	}
	return size, last, nil
}

// RemoveMirror implements Mirrors. It takes the mirror's write lock, so it waits for a fetch or read that is under way
// and no fetch starts on the files while they are removed.
func (g *GoGit) RemoveMirror(m MirrorID) error {
	dir, err := g.mirrorPath(m)
	if err != nil {
		return err
	}
	// Whoever holds the lock ends within fetchDeadline, so this wait is bounded too; the limit is only
	// for a holder that is not, so that a sweep or a delete never hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 2*fetchDeadline)
	defer cancel()
	mu := g.lockFor(m)
	if err := mu.lock(ctx); err != nil {
		return fmt.Errorf("gitx: remove the mirror: it is still in use: %w", err)
	}
	defer mu.unlock()
	g.usedMu.Lock()
	delete(g.used, m)
	g.usedMu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("gitx: remove the mirror: %w", err)
	}
	return nil
}
