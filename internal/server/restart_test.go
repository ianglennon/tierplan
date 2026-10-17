// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Restoring over a running server (plan 03-25, D-30): the process stops serving,
// swaps the database with the lock still held, and a fresh Build serves the
// restored state.

const (
	restoreAdminPassword = "correct horse battery"
	restoreBackupPass    = "restore passphrase 123"
)

var adminHashOnce = sync.OnceValues(func() (string, error) { return auth.HashPassword(restoreAdminPassword) })

func adminHash(t *testing.T) string {
	t.Helper()
	h, err := adminHashOnce()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedAdmin adds an instance admin straight to the store.
func seedAdmin(t *testing.T, srv *Server, name string) {
	t.Helper()
	ctx := context.Background()
	hash := adminHash(t)
	tx, err := srv.Store().Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := srv.Store().Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "id-" + name, Username: name, PasswordHash: hash, CreatedAt: 1, UpdatedAt: 1, PasswordChangedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, srv.Store().Q, "id-"+name); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// backupOf builds a data directory of its own (its own master key) with one
// admin and returns an encrypted backup of it.
func backupOf(t *testing.T, instanceName, admin string) []byte {
	t.Helper()
	dir := t.TempDir() + "/data"
	src, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	seedAdmin(t, src, admin)
	var buf bytes.Buffer
	if _, err := backup.Export(context.Background(), src.Store(), src.Vault(), dir, &buf, []byte(restoreBackupPass),
		backup.ExportOptions{AppVersion: "v1.2.3", InstanceName: instanceName}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// served is a server answering on a loopback address, with the admin signed in.
type served struct {
	t      *testing.T
	srv    *Server
	base   string
	cookie *http.Cookie
	csrf   string
	done   chan error
	cancel context.CancelFunc
	// result is what Run returned, once wait has read it.
	result error
	exited bool
}

// wait returns what Run returned, waiting up to testsupport.Deadline for it.
func (s *served) wait() error {
	s.t.Helper()
	if s.exited {
		return s.result
	}
	select {
	case s.result = <-s.done:
		s.exited = true
	case <-time.After(testsupport.Deadline):
		s.t.Fatal("Run did not return")
	}
	return s.result
}

func serveOn(t *testing.T, dir string) *served {
	t.Helper()
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	seedAdmin(t, srv, "oldadmin")
	srv.Instance().Apply("Home lab", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, srv: srv, base: "http://" + ln.Addr().String(), done: make(chan error, 1), cancel: cancel}
	go func() { s.done <- srv.run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		_ = s.wait()
		_ = srv.Close()
	})
	s.cookie, s.csrf = s.login("oldadmin")
	return s
}

func (s *served) login(user string) (*http.Cookie, string) {
	s.t.Helper()
	code, body, cookies := s.raw(http.MethodPost, "/api/auth/login", "application/json", nil, "",
		strings.NewReader(`{"username":"`+user+`","password":"`+restoreAdminPassword+`"}`))
	if code != http.StatusOK || len(cookies) != 1 {
		s.t.Fatalf("sign in as %s: %d %s", user, code, body)
	}
	c := cookies[0]
	code, body, _ = s.raw(http.MethodGet, "/api/me", "", c, "", nil)
	var me struct {
		CSRFToken string `json:"csrfToken"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &me) != nil {
		s.t.Fatalf("GET /api/me: %d %s", code, body)
	}
	return c, me.CSRFToken
}

func (s *served) raw(method, path, ctype string, cookie *http.Cookie, csrf string, body io.Reader) (int, []byte, []*http.Cookie) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Cookies()
}

func (s *served) postJSON(path string, body map[string]string) (int, []byte) {
	s.t.Helper()
	b, _ := json.Marshal(body)
	code, out, _ := s.raw(http.MethodPost, path, "application/json", s.cookie, s.csrf, bytes.NewReader(b))
	return code, out
}

// uploadAndCheck uploads the file and checks it, returning the upload ID.
func (s *served) uploadAndCheck(file []byte) string {
	s.t.Helper()
	code, body, _ := s.raw(http.MethodPost, "/api/backup/restore/upload", "application/octet-stream", s.cookie, s.csrf, bytes.NewReader(file))
	var up struct {
		UploadID string `json:"uploadId"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &up) != nil {
		s.t.Fatalf("upload: %d %s", code, body)
	}
	code, body = s.postJSON("/api/backup/restore/check", map[string]string{"uploadId": up.UploadID, "passphrase": restoreBackupPass})
	if code != http.StatusOK || !strings.Contains(string(body), `"instanceName":"Restored lab"`) {
		s.t.Fatalf("check: %d %s", code, body)
	}
	return up.UploadID
}

// stubExec replaces the exec of the process image for the test and returns what
// it was called with.
func stubExec(t *testing.T, fn func() error) *int {
	t.Helper()
	calls := new(int)
	prev := execImage
	execImage = func(string, []string, []string) error {
		*calls++
		return fn()
	}
	t.Cleanup(func() { execImage = prev })
	return calls
}

func TestRestoreRestartSwapsTheDatabaseKeepsTheLockUntilExecAndTheNextBuildServesTheRestoredState(t *testing.T) {
	dir := t.TempDir() + "/data"
	file := backupOf(t, "Restored lab", "newadmin")
	s := serveOn(t, dir)
	id := s.uploadAndCheck(file)
	oldCookie := s.cookie

	code, body := s.postJSON("/api/backup/restore/apply", map[string]string{"uploadId": id, "passphrase": restoreBackupPass, "confirm": "Home lab"})
	if code != http.StatusAccepted || !strings.Contains(string(body), `"restarting":true`) {
		t.Fatalf("apply: %d %s", code, body)
	}

	// The server stops serving and Run hands back the restart.
	var re *RestartError
	if err := s.wait(); !errors.Is(err, ErrRestart) || !errors.As(err, &re) {
		t.Fatalf("Run returned %v, want ErrRestart", err)
	}
	if _, err := http.Get(s.base + "/readyz"); err == nil { //nolint:noctx,bodyclose // a refused connection is the answer
		t.Fatal("the server still answers after the restore")
	}
	// Nothing has been swapped yet: the database is the old one, and the lock is
	// held by the stopped server.
	if m, _ := filepath.Glob(filepath.Join(dir, "backups", "prerestore-*.db")); len(m) != 0 {
		t.Fatalf("a pre-restore snapshot exists before the swap: %v", m)
	}

	// Restart runs the swap, then execs. The stub checks what the exec would find.
	calls := stubExec(t, func() error {
		if _, err := datadir.Lock(dir); !errors.Is(err, datadir.ErrLocked) {
			t.Errorf("the data directory lock at exec = %v, want it still held", err)
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "backups", "prerestore-*.db")); len(m) != 1 {
			t.Errorf("pre-restore snapshots at exec = %v, want 1", m)
		}
		if _, pending, _ := backup.ReadMarker(dir); !pending {
			t.Error("no restore-pending marker at exec")
		}
		return nil
	})
	if err := Restart(RestartExec, re.Request); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("exec called %d times, want 1", *calls)
	}
	// The exec closes the close-on-exec lock descriptor; the test does it by hand.
	if err := s.srv.unlock(); err != nil {
		t.Fatal(err)
	}

	// What the re-executed process does: Build on the same directory.
	srv2, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Build after the restore: %v", err)
	}
	t.Cleanup(func() { _ = srv2.Close() })
	if _, pending, _ := backup.ReadMarker(dir); pending {
		t.Fatal("the restore-pending marker survived a start that worked")
	}

	h := srv2.Handler()
	do := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	login := func(user string) *httptest.ResponseRecorder {
		return do(http.MethodPost, "/api/auth/login", `{"username":"`+user+`","password":"`+restoreAdminPassword+`"}`, nil)
	}
	if rec := login("newadmin"); rec.Code != http.StatusOK {
		t.Fatalf("the restored admin cannot sign in: %d %s", rec.Code, rec.Body)
	}
	if rec := login("oldadmin"); rec.Code == http.StatusOK {
		t.Fatal("the admin of the replaced instance still signs in")
	}
	if rec := do(http.MethodGet, "/api/me", "", oldCookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the old session cookie = %d, want 401", rec.Code)
	}
	if srv2.Instance().Name() == "Home lab" {
		t.Fatal("the instance still has the old name in memory")
	}
}

func TestRestoreRestartInExitModeSwapsAndReturnsWithoutExec(t *testing.T) {
	dir := t.TempDir() + "/data"
	file := backupOf(t, "Restored lab", "newadmin")
	s := serveOn(t, dir)
	id := s.uploadAndCheck(file)
	if code, body := s.postJSON("/api/backup/restore/apply", map[string]string{"uploadId": id, "passphrase": restoreBackupPass, "confirm": "Home lab"}); code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", code, body)
	}
	var re *RestartError
	if err := s.wait(); !errors.As(err, &re) {
		t.Fatalf("Run returned %v", err)
	}
	calls := stubExec(t, func() error { return nil })
	if err := Restart(RestartExit, re.Request); err != nil {
		t.Fatalf("Restart in exit mode: %v", err)
	}
	if *calls != 0 {
		t.Fatal("exit mode executed the image")
	}
	if _, pending, _ := backup.ReadMarker(dir); !pending {
		t.Fatal("exit mode did not leave the marker for the next start")
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); err != nil {
		t.Fatalf("no pre-restore snapshot directory: %v", err)
	}
}

func TestRestartFailsBeforeExecWhenTheSwapFails(t *testing.T) {
	calls := stubExec(t, func() error { return nil })
	err := Restart(RestartExec, RestartRequest{Swap: func(context.Context) error { return errors.New("disk full") }})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Restart = %v, want the swap's error", err)
	}
	if *calls != 0 {
		t.Fatal("the image was executed after a failed swap")
	}
}

func TestRestartReportsAnExecThatFails(t *testing.T) {
	stubExec(t, func() error { return errors.New("exec format error") })
	err := Restart(RestartExec, RestartRequest{})
	if err == nil || !strings.Contains(err.Error(), "exec format error") || !strings.Contains(err.Error(), "BAYPLAN_RESTORE_RESTART=exit") {
		t.Fatalf("Restart = %v, want the exec error and the way out", err)
	}
}

func TestOnlyTheFirstRestartRequestIsAccepted(t *testing.T) {
	r := newRestarter()
	if !r.request(RestartRequest{}) {
		t.Fatal("the first request was refused")
	}
	if r.request(RestartRequest{}) {
		t.Fatal("a second request was accepted")
	}
	select {
	case <-r.requested():
	default:
		t.Fatal("requested() is not closed after a request")
	}
}

// pendingRestore sets a data directory up as a swap leaves it: the old database
// saved under backups/, a database that carries a schema newer than this binary
// as bayplan.db, and the restore-pending marker. The old database holds the
// admin "oldadmin"; the new one holds a setting the old one lacks.
func pendingRestore(t *testing.T) (dir, snapshot string) {
	t.Helper()
	ctx := context.Background()
	dir = t.TempDir() + "/data"
	old, err := Build(ctx, testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	seedAdmin(t, old, "oldadmin")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot, err = backup.PreRestoreSnapshot(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}

	// The "restored" database: the same file with a newer schema recorded, so the
	// migration at the next start refuses it.
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO settings (key, value, updated_at) VALUES ('from_the_restore', 'yes', 1)`,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (9999, 1, CURRENT_TIMESTAMP)`,
	} {
		if _, err := st.Write.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteMarker(dir, snapshot, now); err != nil {
		t.Fatal(err)
	}
	return dir, snapshot
}

func TestBootRollbackPutsThePreRestoreSnapshotBackWhenTheRestoredDatabaseCannotStart(t *testing.T) {
	dir, snapshot := pendingRestore(t)

	var logs bytes.Buffer
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("Build with a restore pending whose database cannot start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// The old state is what runs.
	var n int
	if err := srv.Store().Read.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'oldadmin'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the old admin is missing after the rollback: %d, %v", n, err)
	}
	if err := srv.Store().Read.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'from_the_restore'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the restored database is still in use: %d, %v", n, err)
	}
	if _, pending, _ := backup.ReadMarker(dir); pending {
		t.Error("the marker survived the rollback")
	}
	if _, err := os.Stat(snapshot); err == nil {
		t.Error("the snapshot is still in backups/: it should have been moved back")
	}

	// The failure is on the old chain, and the chain verifies.
	var details string
	if err := srv.Store().Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'backup.failed'`).Scan(&details); err != nil {
		t.Fatalf("no backup.failed entry: %v", err)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(details), &d); err != nil || d["stage"] != "restore_boot" || d["error"] == "" {
		t.Fatalf("backup.failed details = %v, %v; want stage restore_boot and the error", d, err)
	}
	if res, err := audit.Verify(context.Background(), srv.Store(), srv.Vault()); err != nil || res.Broken != nil {
		t.Fatalf("the chain after the rollback = %+v, %v", res, err)
	}
	if !strings.Contains(logs.String(), "going back to the pre-restore snapshot") {
		t.Error("the rollback was not logged")
	}
}

func TestBootRollbackDoesNotRunWhenNoRestoreIsPending(t *testing.T) {
	dir, _ := pendingRestore(t)
	if err := backup.RemoveMarker(dir); err != nil {
		t.Fatal(err)
	}
	// Without the marker a database that cannot start is an error to report, not
	// something to quietly replace.
	if _, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Build succeeded on a database newer than the binary")
	}
}

func TestBootRollbackReportsWhenThereIsNoSnapshotToGoBackTo(t *testing.T) {
	dir, snapshot := pendingRestore(t)
	if err := os.Remove(snapshot); err != nil {
		t.Fatal(err)
	}
	_, err := Build(context.Background(), testConfig(t, map[string]string{"BAYPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("Build = %v, want an error that says the snapshot is missing", err)
	}
	// The data directory is released for the next attempt.
	release, lerr := datadir.Lock(dir)
	if lerr != nil {
		t.Fatalf("the lock was kept after a failed start: %v", lerr)
	}
	_ = release()
}

// WR-B-01: a start that fails for a reason that has nothing to do with the
// restored database (here a key variable that is not a key) must not throw the
// restore away. The marker stays, so the next start still has its one try.
func TestBootRollbackIsNotTriggeredByAFailureUnrelatedToTheRestoredData(t *testing.T) {
	dir, snapshot := pendingRestore(t)

	_, err := Build(context.Background(), testConfig(t, map[string]string{
		"BAYPLAN_DATA_DIR":       dir,
		"BAYPLAN_ENCRYPTION_KEY": "this is not a key",
	}), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("Build succeeded with a bad encryption key")
	}
	if errors.Is(err, errRestoredState) {
		t.Fatalf("a key error was classed as a restored-data error: %v", err)
	}
	if _, pending, _ := backup.ReadMarker(dir); !pending {
		t.Error("the restore marker was removed by a start that failed for another reason")
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Errorf("the pre-restore snapshot was consumed: %v", err)
	}
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'from_the_restore'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the restored database was replaced: %d, %v", n, err)
	}
}
