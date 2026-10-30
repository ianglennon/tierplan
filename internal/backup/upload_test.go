// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package backup_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/proto/testsupport"
)

func newUploads(t *testing.T, max int64) (*backup.Uploads, string) {
	t.Helper()
	dir := t.TempDir()
	return backup.NewUploads(dir, max), dir
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func TestUploadsSaveKeepsTheFileEncryptedAsSentUnderAPrivateDirectory(t *testing.T) {
	u, dir := newUploads(t, 1<<20)
	body := bytes.Repeat([]byte("x"), 1000)

	id, err := u.Save(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := u.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, body) {
		t.Fatal("the stored upload is not what was sent")
	}
	info, _ := f.Stat()
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
	dinfo, err := os.Stat(filepath.Join(dir, "restore-staging", id))
	if err != nil || dinfo.Mode().Perm() != 0o700 {
		t.Errorf("upload directory = %v, %v; want 0700", dinfo, err)
	}

	u.Remove(id)
	if _, err := u.Open(id); !errors.Is(err, backup.ErrUploadNotFound) {
		t.Fatalf("Open after Remove = %v, want ErrUploadNotFound", err)
	}
	if left := entries(t, dir); len(left) != 0 {
		t.Fatalf("files left after Remove: %v", left)
	}
}

func TestUploadsAllowOneAtATimeUntilTheFirstExpires(t *testing.T) {
	u, _ := newUploads(t, 1<<20)
	now := time.Now()
	u.Now = func() time.Time { return now }

	first, err := u.Save(strings.NewReader("one"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Save(strings.NewReader("two"), 3); !errors.Is(err, backup.ErrRestoreBusy) {
		t.Fatalf("second Save = %v, want ErrRestoreBusy", err)
	}
	now = now.Add(14 * time.Minute)
	if _, err := u.Save(strings.NewReader("two"), 3); !errors.Is(err, backup.ErrRestoreBusy) {
		t.Fatalf("Save after 14 minutes = %v, want ErrRestoreBusy", err)
	}

	now = now.Add(2 * time.Minute) // 16 minutes in
	if _, err := u.Open(first); !errors.Is(err, backup.ErrUploadNotFound) {
		t.Fatalf("Open of an expired upload = %v, want ErrUploadNotFound", err)
	}
	second, err := u.Save(strings.NewReader("two"), 3)
	if err != nil {
		t.Fatalf("Save after the first expired: %v", err)
	}
	if second == first {
		t.Fatal("the new upload reuses the old ID")
	}
}

func TestUploadsSweepExpiredUploadsBeforeRefusingAsBusy(t *testing.T) {
	u, dir := newUploads(t, 1<<20)
	now := time.Now()
	u.Now = func() time.Time { return now }
	if _, err := u.Save(strings.NewReader("one"), 3); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := u.Save(strings.NewReader("two"), 3); err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, e := range entries(t, dir) {
		if strings.HasPrefix(filepath.Base(e), "u-") {
			dirs++
		}
	}
	if dirs != 1 {
		t.Fatalf("upload directories after the sweep = %d (%v), want 1", dirs, entries(t, dir))
	}
}

func TestUploadsRefuseABodyAboveTheCapAndLeaveNothingBehind(t *testing.T) {
	u, dir := newUploads(t, 1<<20)
	big := bytes.Repeat([]byte("x"), 1<<20+1)

	if _, err := u.Save(bytes.NewReader(big), int64(len(big))); !errors.Is(err, backup.ErrTooLarge) {
		t.Fatalf("Save with a Content-Length above the cap = %v, want ErrTooLarge", err)
	}
	// An unknown length is caught while reading.
	if _, err := u.Save(bytes.NewReader(big), -1); !errors.Is(err, backup.ErrTooLarge) {
		t.Fatalf("Save of an unannounced long body = %v, want ErrTooLarge", err)
	}
	// A MaxBytesReader in front, as the HTTP handler adds, ends the same way.
	rec := httptest.NewRecorder()
	capped := http.MaxBytesReader(rec, io.NopCloser(bytes.NewReader(big)), 1<<20)
	if _, err := u.Save(capped, -1); !errors.Is(err, backup.ErrTooLarge) {
		t.Fatalf("Save through MaxBytesReader = %v, want ErrTooLarge", err)
	}
	for _, e := range entries(t, dir) {
		if strings.Contains(e, "u-") {
			t.Fatalf("a refused upload left %s", e)
		}
	}
	// A body exactly at the cap is fine.
	if _, err := u.Save(bytes.NewReader(big[:1<<20]), 1<<20); err != nil {
		t.Fatalf("Save at the cap = %v", err)
	}
}

func TestUploadsRefuseABodyShorterThanItsContentLength(t *testing.T) {
	u, dir := newUploads(t, 1<<20)

	if _, err := u.Save(strings.NewReader("short"), 100); !errors.Is(err, backup.ErrIncomplete) {
		t.Fatalf("Save of a short body = %v, want ErrIncomplete", err)
	}
	if _, err := u.Save(io.MultiReader(strings.NewReader("part"), errReader{io.ErrUnexpectedEOF}), 100); !errors.Is(err, backup.ErrIncomplete) {
		t.Fatalf("Save of a body that ends with an unexpected EOF = %v, want ErrIncomplete", err)
	}
	for _, e := range entries(t, dir) {
		if strings.Contains(e, "u-") {
			t.Fatalf("a cut-off upload left %s", e)
		}
	}
	// The failure freed the slot.
	if _, err := u.Save(strings.NewReader("whole"), 5); err != nil {
		t.Fatalf("Save after a cut-off upload: %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestUploadsOpenAndRemoveRefuseAnIDThatIsNotOurs(t *testing.T) {
	u, dir := newUploads(t, 1<<20)
	outside := filepath.Join(dir, "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "..", "../secret", "u-../../secret", "u-zz", "r-0123456789abcdef0123456789abcdef"} {
		if _, err := u.Open(id); !errors.Is(err, backup.ErrUploadNotFound) {
			t.Errorf("Open(%q) = %v, want ErrUploadNotFound", id, err)
		}
		u.Remove(id)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("Remove reached outside the staging directory: %v", err)
	}
}

func TestClearStagingRemovesEverythingUnderTheStagingDirectory(t *testing.T) {
	u, dir := newUploads(t, 1<<20)
	if _, err := u.Save(strings.NewReader("one"), 3); err != nil {
		t.Fatal(err)
	}
	if err := backup.ClearStaging(dir); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, dir); len(left) != 0 {
		t.Fatalf("left after ClearStaging: %v", left)
	}
	if err := backup.ClearStaging(dir); err != nil {
		t.Fatalf("ClearStaging with nothing there: %v", err)
	}
}

// slowReader hands out its body in two halves and runs onMid between them, the way
// a slow link holds a request open.
type slowReader struct {
	first, second []byte
	onMid         func()
	stage         int
}

func (r *slowReader) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage = 1
		return copy(p, r.first), nil
	case 1:
		r.stage = 2
		if r.onMid != nil {
			r.onMid()
		}
		return copy(p, r.second), nil
	}
	return 0, io.EOF
}

// WR-B-02: the upload's lifetime is counted from the last byte, not the first.
func TestUploadsLifetimeIsCountedFromTheCompleteFile(t *testing.T) {
	u, _ := newUploads(t, 1<<20)
	start := time.Now()
	now := start
	u.Now = func() time.Time { return now }

	r := &slowReader{first: []byte("abc"), second: []byte("def"), onMid: func() { now = now.Add(12 * time.Minute) }}
	id, err := u.Save(r, 6)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := u.Expires(id)
	if err != nil {
		t.Fatal(err)
	}
	want := start.Add(12*time.Minute + u.Lifetime())
	if d := expires.Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("expires = %v, want %v (15 minutes after the file was complete)", expires, want)
	}
	// 14 minutes after completion the file is still there, though it is 26 minutes
	// since the request began.
	now = now.Add(14 * time.Minute)
	f, err := u.Open(id)
	if err != nil {
		t.Fatalf("Open 14 minutes after completion: %v", err)
	}
	_ = f.Close()
	now = now.Add(2 * time.Minute)
	if _, err := u.Open(id); !errors.Is(err, backup.ErrUploadNotFound) {
		t.Fatalf("Open 16 minutes after completion = %v, want ErrUploadNotFound", err)
	}
}

// WR-B-02: a slow body does not hold the lock, so the other calls answer at once.
func TestUploadsAnArrivingBodyDoesNotBlockOtherCalls(t *testing.T) {
	u, _ := newUploads(t, 1<<20)
	pr, pw := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		_, err := u.Save(pr, -1)
		saved <- err
	}()
	if _, err := pw.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}

	answered := make(chan error, 3)
	go func() { _, err := u.Open("u-00000000000000000000000000000000"); answered <- err }()
	go func() { _, err := u.Save(strings.NewReader("two"), 3); answered <- err }()
	go func() { _, err := u.Discard(); answered <- err }()
	want := []error{backup.ErrUploadNotFound, backup.ErrRestoreBusy, backup.ErrRestoreBusy}
	for range want {
		err := testsupport.Await(t, "a call beside the arriving upload to answer instead of blocking behind it", answered)
		ok := false
		for _, w := range want {
			ok = ok || errors.Is(err, w)
		}
		if !ok {
			t.Errorf("a call beside the arriving upload answered %v", err)
		}
	}
	_ = pw.Close()
	if err := <-saved; err != nil {
		t.Fatalf("the arriving upload failed: %v", err)
	}
}

func TestUploadsDiscardRemovesTheWaitingUploadAndSaysWhetherThereWasOne(t *testing.T) {
	u, dir := newUploads(t, 1<<20)
	if removed, err := u.Discard(); err != nil || removed {
		t.Fatalf("Discard with nothing waiting = %v, %v, want false, nil", removed, err)
	}
	id, err := u.Save(strings.NewReader("one"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := u.Discard(); err != nil || !removed {
		t.Fatalf("Discard = %v, %v, want true, nil", removed, err)
	}
	if _, err := u.Open(id); !errors.Is(err, backup.ErrUploadNotFound) {
		t.Fatalf("Open after Discard = %v, want ErrUploadNotFound", err)
	}
	if left := entries(t, dir); len(left) != 0 {
		t.Fatalf("Discard left %v", left)
	}
	if _, err := u.Save(strings.NewReader("two"), 3); err != nil {
		t.Fatalf("Save after Discard: %v", err)
	}
}
