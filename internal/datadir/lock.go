// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// LockFile is the lock's file name inside the data directory. It holds no
// content: the lock is the advisory flock on the open file, not what is in it.
const LockFile = ".bayplan.lock"

// ErrLocked is returned by Lock when another process, or another holder in this
// one, has the data directory.
var ErrLocked = errors.New("the data directory is in use by another Bayplan process")

// Lock takes the exclusive lock on <dir>/.bayplan.lock without waiting. The
// server holds it from startup to shutdown, and an offline `bayplan restore`
// holds it from staging until the swap, so the two can never write one database
// at the same time and two servers cannot share a volume. The returned release
// func drops the lock and may be called more than once. The lock is also dropped
// when the process exits, however it exits.
//
// It is flock(2) on a file opened close-on-exec, so a re-exec releases it. flock
// is unreliable on some network file systems (NFS before 4, some FUSE mounts);
// keep /data on a local volume.
func Lock(dir string) (release func() error, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, LockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the lock file in the data directory
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s is held)", ErrLocked, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	var once sync.Once
	return func() error {
		var rerr error
		once.Do(func() {
			// Closing the descriptor drops the lock; the explicit unlock only
			// makes the order plain.
			uerr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor fits an int
			rerr = errors.Join(uerr, f.Close())
		})
		return rerr
	}, nil
}
