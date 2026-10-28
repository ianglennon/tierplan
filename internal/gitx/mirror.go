// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/google/uuid"
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
// a read never sees a fetch half done. The locks live as long as the client.
func (g *GoGit) lockFor(m MirrorID) *sync.RWMutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[m]
	if !ok {
		l = &sync.RWMutex{}
		g.locks[m] = l
	}
	return l
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

// openMirror opens the bare repository at dir, creating it (and the remote a fetch needs) when it
// is not there, which includes a mirror whose directory was removed. The caller holds the write
// lock of the mirror. Nothing is ever checked out: the mirror is only an object database.
func openMirror(dir string) (*git.Repository, error) {
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		if err := os.MkdirAll(filepath.Dir(dir), mirrorRootMode); err != nil {
			return nil, fmt.Errorf("gitx: create the mirrors directory: %w", err)
		}
		repo, err = git.PlainInit(dir, true)
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
