// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/backup"
	"github.com/ianglennon/coaming/internal/config"
)

// Backups: the download route and the facts the Backup page needs (plan 03-09,
// D-28, D-29, D-32, PLAT-03).

const backupPass = "correct horse battery"

func (f *authFixture) exportBackup(t *testing.T, cookie, csrf, passphrase string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"passphrase": passphrase})
	return f.call(t, authReq{method: http.MethodPost, path: "/api/backup/export", body: string(b), cookie: cookie, csrf: csrf})
}

func TestBackupDownloadByAnInstanceAdminOpensWithThePassphrase(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.exportBackup(t, cookie, csrf, backupPass)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="coaming-backup-20260101-0000.coaming"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("no Content-Length")
	}

	r, err := backup.Open(bytes.NewReader(rec.Body.Bytes()), []byte(backupPass))
	if err != nil {
		t.Fatalf("the download does not open with the passphrase: %v", err)
	}
	name, body, err := r.Next()
	if err != nil || name != backup.EntryManifest {
		t.Fatalf("first entry = %q, %v", name, err)
	}
	var m backup.Manifest
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Format != 1 || m.AppVersion != "v9.9.9" || !m.ChainVerified || m.Counts.Users != 1 {
		t.Errorf("manifest = %+v", m)
	}
	name, body, err = r.Next()
	if err != nil || name != backup.EntryDB {
		t.Fatalf("second entry = %q, %v", name, err)
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(body, head); err != nil || string(head) != "SQLite format 3\x00" {
		t.Fatalf("the database entry starts %q, %v", head, err)
	}
}

// A data directory given in the relative form make dev uses (./.devdata) must
// not break the download: SQLite refuses file://.devdata/... as a URI whose
// authority is ".devdata" (UAT gap G-03-4).
//
//nolint:paralleltest // t.Chdir changes the process's working directory, which a parallel test must not do
func TestBackupDownloadWithARelativeDataDirectoryOpensWithThePassphrase(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	t.Chdir(filepath.Dir(f.dataInfo.Path))
	f.dataInfo.Path = "./" + filepath.Base(f.dataInfo.Path)
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.exportBackup(t, cookie, csrf, backupPass)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s\nlog: %s", rec.Code, rec.Body, f.logs)
	}
	r, err := backup.Open(bytes.NewReader(rec.Body.Bytes()), []byte(backupPass))
	if err != nil {
		t.Fatalf("the download does not open with the passphrase: %v", err)
	}
	name, body, err := r.Next()
	if err != nil || name != backup.EntryManifest {
		t.Fatalf("first entry = %q, %v", name, err)
	}
	var m backup.Manifest
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Format != 1 || !m.ChainVerified {
		t.Errorf("manifest = %+v", m)
	}
	name, body, err = r.Next()
	if err != nil || name != backup.EntryDB {
		t.Fatalf("second entry = %q, %v", name, err)
	}
	head := make([]byte, 16)
	if _, err := io.ReadFull(body, head); err != nil || string(head) != "SQLite format 3\x00" {
		t.Fatalf("the database entry starts %q, %v", head, err)
	}
	for _, n := range f.leftovers(t) {
		if strings.HasPrefix(n, ".coaming-backup-") || strings.HasPrefix(n, ".coaming-export-") {
			t.Errorf("the data directory kept %q", n)
		}
	}
}

func TestBackupDownloadIsRefusedWithoutBackupManage(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)

	rec := f.exportBackup(t, cookie, csrf, backupPass)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if bytes.HasPrefix(rec.Body.Bytes(), []byte(backup.Magic)) {
		t.Fatal("a refused request still received a backup")
	}
	if n := len(f.audits(t, "permission.denied")); n != 1 {
		t.Fatalf("permission.denied entries = %d, want 1", n)
	}
	if n := len(f.audits(t, "backup.exported")); n != 0 {
		t.Fatalf("backup.exported entries = %d, want 0", n)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/info", cookie: cookie}); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/backup/info = %d, want 403", rec.Code)
	}
}

func TestBackupInfoReportsThePassphraseRuleAndTheExtension(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/info", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		MinPassphraseLength int    `json:"minPassphraseLength"`
		FileExtension       string `json:"fileExtension"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MinPassphraseLength != 16 || got.FileExtension != ".coaming" {
		t.Fatalf("info = %+v", got)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/info"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/backup/info = %d, want 401", rec.Code)
	}
}

func TestBackupDownloadIsAuditedWithoutThePassphrase(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.exportBackup(t, cookie, csrf, backupPass)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := f.auditDetails(t, "backup.exported")
	if len(got) != 1 {
		t.Fatalf("backup.exported entries = %d, want 1", len(got))
	}
	d := got[0]
	if d["file"] != "coaming-backup-20260101-0000.coaming" || d["source"] != "ui" || d["size"] == "" {
		t.Fatalf("details = %v", d)
	}
	if e := f.audits(t, "backup.exported"); e[0].ActorID != "id-admin" || e[0].ActorType != "user" {
		t.Fatalf("actor = %+v", e[0])
	}
	// The passphrase is in no audit row and no log line.
	rows, err := f.st.Read.Query(`SELECT details || target_id || reason FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, backupPass) {
			t.Fatalf("the passphrase is in an audit row: %s", s)
		}
	}
	if strings.Contains(f.logs.String(), backupPass) {
		t.Fatal("the passphrase is in the log")
	}
}

// leftovers lists what is in the data directory beyond what the fixture put there.
func (f *authFixture) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dataInfo.Path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestBackupDownloadWithAShortPassphraseIsRefusedAndLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.exportBackup(t, cookie, csrf, "fifteen chars!!") // 15 characters
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "passphrase_too_short" {
		t.Fatalf("status = %d, type %q: %s", rec.Code, problemType(t, rec), rec.Body)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("the data directory holds %v after a refused backup", left)
	}
	if n := len(f.audits(t, "backup.exported")); n != 0 {
		t.Fatalf("backup.exported entries = %d, want 0", n)
	}
}

func TestBackupDownloadWithABrokenAuditChainIsRefused(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if _, err := f.st.Write.Exec(`UPDATE audit_log SET action = 'x' || substr(action, 2) WHERE seq = 1`); err != nil {
		t.Fatal(err)
	}

	rec := f.exportBackup(t, cookie, csrf, backupPass)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "audit_chain_broken" {
		t.Fatalf("status = %d, type %q: %s", rec.Code, problemType(t, rec), rec.Body)
	}
	if bytes.HasPrefix(rec.Body.Bytes(), []byte(backup.Magic)) {
		t.Fatal("a refused request still received a backup")
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("the data directory holds %v after a refused backup", left)
	}
}

// slowSink holds the first write until released, so an export stays running.
type slowSink struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return len(p), nil
}

func TestBackupDownloadWhileAnotherIsBeingPreparedAnswersBusyAndLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	sink := &slowSink{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := backup.Export(context.Background(), f.st, f.v, f.dataInfo.Path, sink, []byte(backupPass), backup.ExportOptions{AppVersion: "v9.9.9"})
		done <- err
	}()
	select {
	case <-sink.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first export never started writing")
	}

	rec := f.exportBackup(t, cookie, csrf, backupPass)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "backup_busy" {
		t.Fatalf("status = %d, type %q: %s", rec.Code, problemType(t, rec), rec.Body)
	}
	close(sink.release)
	if err := <-done; err != nil {
		t.Fatalf("the first export: %v", err)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("the data directory holds %v after the busy refusal", left)
	}
	// Once the first has finished the next one goes through, and cleans up after itself.
	if rec := f.exportBackup(t, cookie, csrf, backupPass); rec.Code != http.StatusOK {
		t.Fatalf("a backup after the first finished: %d %s", rec.Code, rec.Body)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Fatalf("the data directory holds %v after a finished download", left)
	}
}
