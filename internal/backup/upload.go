// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// uploadPrefix starts the name of every upload directory, so the sweep and
	// the "one at a time" rule never confuse an upload with the staged database
	// of a restore in progress (which Stage names "r-...").
	uploadPrefix = "u-"
	// uploadFile is the encrypted backup, exactly as it was uploaded.
	uploadFile = "upload" + FileExtension
	// DefaultMaxUpload is the cap on an uploaded backup when none is configured.
	DefaultMaxUpload int64 = 1 << 30
)

// uploadID is what NewID produces: the prefix and 32 hex digits. Open and Remove
// refuse anything else, so an ID from a request can never name another path.
var uploadID = regexp.MustCompile(`^` + uploadPrefix + `[0-9a-f]{32}$`)

// Errors returned by Uploads.
var (
	// ErrRestoreBusy means another upload is waiting or a restore is running; only
	// one is handled at a time.
	ErrRestoreBusy = errors.New("another restore is in progress")
	// ErrTooLarge means the upload is above the configured cap.
	ErrTooLarge = errors.New("the upload is larger than the allowed size")
	// ErrIncomplete means the upload ended before the length it announced, which
	// is what a proxy's body-size limit or a dropped connection looks like.
	ErrIncomplete = errors.New("the upload was cut off")
	// ErrUploadNotFound means the upload does not exist or has expired.
	ErrUploadNotFound = errors.New("no such upload")
)

// Uploads keeps one encrypted backup at a time, between the request that
// uploads it and the ones that check and apply it. The file stays exactly as it
// was sent (still encrypted), sits in a 0700 directory and is removed after the
// restore or after the TTL (T-03-107).
type Uploads struct {
	// Dir holds the upload directories, normally <data>/restore-staging beside
	// the staged databases of Stage.
	Dir string
	// MaxBytes caps one upload. Zero or less means DefaultMaxUpload.
	MaxBytes int64
	// TTL is how long an upload may wait. Zero or less means 15 minutes.
	TTL time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu sync.Mutex
}

// NewUploads returns the uploads of a data directory.
func NewUploads(dataDir string, maxBytes int64) *Uploads {
	return &Uploads{Dir: filepath.Join(dataDir, stagingDir), MaxBytes: maxBytes, TTL: stagingMaxAge}
}

func (u *Uploads) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Uploads) ttl() time.Duration {
	if u.TTL > 0 {
		return u.TTL
	}
	return stagingMaxAge
}

// Lifetime is how long an upload may wait before it expires.
func (u *Uploads) Lifetime() time.Duration { return u.ttl() }

// Max is the cap on one upload in bytes.
func (u *Uploads) Max() int64 {
	if u.MaxBytes > 0 {
		return u.MaxBytes
	}
	return DefaultMaxUpload
}

// sweepLocked removes expired uploads and reports whether a live one remains.
func (u *Uploads) sweepLocked() (live bool, err error) {
	entries, err := os.ReadDir(u.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	now := u.now()
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), uploadPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) >= u.ttl() {
			_ = os.RemoveAll(filepath.Join(u.Dir, e.Name()))
			continue
		}
		live = true
	}
	return live, nil
}

// Save streams r into a new upload and returns its ID. It refuses (ErrRestoreBusy)
// while an unexpired upload exists. contentLength is the announced length, or a
// negative number when it is not known; a body that ends before it is
// ErrIncomplete and one above the cap is ErrTooLarge. A failed Save leaves
// nothing behind.
func (u *Uploads) Save(r io.Reader, contentLength int64) (id string, err error) {
	limit := u.Max()
	if contentLength > limit {
		return "", ErrTooLarge
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return "", fmt.Errorf("create the restore staging directory: %w", err)
	}
	live, err := u.sweepLocked()
	if err != nil {
		return "", fmt.Errorf("sweep the restore staging directory: %w", err)
	}
	if live {
		return "", ErrRestoreBusy
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate an upload ID: %w", err)
	}
	id = uploadPrefix + hex.EncodeToString(raw)
	dir := filepath.Join(u.Dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the upload directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
			id = ""
		}
	}()
	f, err := os.OpenFile(filepath.Join(dir, uploadFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a file in the private upload directory
	if err != nil {
		return "", fmt.Errorf("create the upload file: %w", err)
	}
	// One byte more than the cap is read, so a body at the cap passes and one
	// above it is told apart from one that is merely long.
	n, copyErr := io.Copy(f, io.LimitReader(r, limit+1))
	if copyErr == nil {
		copyErr = f.Sync()
	}
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(copyErr, &tooBig):
		return "", ErrTooLarge
	case errors.Is(copyErr, io.ErrUnexpectedEOF):
		return "", ErrIncomplete
	case copyErr != nil:
		return "", fmt.Errorf("write the upload: %w", copyErr)
	case n > limit:
		return "", ErrTooLarge
	case contentLength >= 0 && n < contentLength:
		return "", ErrIncomplete
	}
	return id, nil
}

// dir is the directory of a live upload, or ErrUploadNotFound for an ID that is
// malformed, unknown or expired (an expired one is removed on the way).
func (u *Uploads) dir(id string) (string, error) {
	if !uploadID.MatchString(id) {
		return "", ErrUploadNotFound
	}
	dir := filepath.Join(u.Dir, id)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", ErrUploadNotFound
	}
	if u.now().Sub(info.ModTime()) >= u.ttl() {
		_ = os.RemoveAll(dir)
		return "", ErrUploadNotFound
	}
	return dir, nil
}

// Open returns the uploaded file for reading.
func (u *Uploads) Open(id string) (*os.File, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	dir, err := u.dir(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, uploadFile)) //nolint:gosec // inside the private upload directory, named by a validated ID
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrUploadNotFound
		}
		return nil, err
	}
	return f, nil
}

// Remove deletes an upload. An unknown ID is not an error.
func (u *Uploads) Remove(id string) {
	if !uploadID.MatchString(id) {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(u.Dir, id))
	// The staging directory goes too when nothing else is in it.
	_ = os.Remove(u.Dir)
}

// ClearStaging removes everything under the data directory's restore staging
// area. The server calls it at start, with the data-directory lock held: no
// restore can be running then, and an upload or staged database from before the
// restart is of no use to anything.
func ClearStaging(dataDir string) error {
	return os.RemoveAll(filepath.Join(dataDir, stagingDir))
}
