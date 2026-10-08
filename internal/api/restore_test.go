// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// The restore routes (plan 03-25, D-30): upload, check and apply.

const restorePass = "restore passphrase 123"

// restoreHook stands in for the server: it keeps the upload store and records the
// swaps the restore asked the server to run once it had stopped.
type restoreHook struct {
	uploads *backup.Uploads
	swaps   []func(context.Context) error
	// refuse makes the next Restart report that a restart is already pending.
	refuse bool
}

func (f *authFixture) restoreDeps(log *slog.Logger) api.RestoreDeps {
	if f.restore == nil {
		f.restore = &restoreHook{uploads: backup.NewUploads(f.dataInfo.Path, 0)}
	}
	h := f.restore
	return api.RestoreDeps{
		Uploads: h.uploads, Vault: f.v, Audit: f.aud, Instance: f.inst,
		DataDir: f.dataInfo.Path, AppVersion: "v9.9.9", Now: f.clk.now, Log: log,
		Restart: func(swap func(context.Context) error) bool {
			if h.refuse {
				return false
			}
			h.swaps = append(h.swaps, swap)
			return true
		},
	}
}

// makeBackup builds a backup of a second instance: its own database and master
// key, one instance admin and one audit entry.
func makeBackup(t *testing.T, instanceName, admin, publicURL string) []byte {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := st.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "src-" + admin, Username: admin, PasswordHash: authHash(), CreatedAt: 1, UpdatedAt: 1, PasswordChangedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, st.Q, "src-"+admin); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := audit.NewWriter(st, v, slog.New(slog.DiscardHandler), nil).Record(ctx, audit.Entry{
		Actor: audit.SystemCLI, Action: audit.ActionAuthLogin, Result: audit.ResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := backup.Export(ctx, st, v, t.TempDir(), &buf, []byte(restorePass), backup.ExportOptions{
		AppVersion: "v1.2.3", InstanceName: instanceName, PublicURL: publicURL,
	}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var uploadIDPattern = regexp.MustCompile(`^u-[0-9a-f]{32}$`)

// upload sends the file the way the SPA does: the raw bytes as the body.
func (f *authFixture) upload(t *testing.T, cookie, csrf string, file []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/backup/restore/upload", bytes.NewReader(file))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", authUA)
	req.RemoteAddr = authIP + ":40000"
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if origin := f.inst.PublicOrigin(); origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *authFixture) restoreCall(t *testing.T, cookie, csrf, step string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	return f.call(t, authReq{method: http.MethodPost, path: "/api/backup/restore/" + step, body: string(b), cookie: cookie, csrf: csrf})
}

func uploadedID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got struct {
		UploadID string `json:"uploadId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !uploadIDPattern.MatchString(got.UploadID) {
		t.Fatalf("upload answer = %s (%v)", rec.Body, err)
	}
	return got.UploadID
}

func TestRestoreCheckAnswersWhatIsInTheUploadedBackupAndAuditsIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://new.example.com"})
	cookie, csrf := f.signIn(t, "admin", false)
	id := uploadedID(t, f.upload(t, cookie, csrf, makeBackup(t, "Old lab", "oldadmin", "https://old.example.com")))

	rec := f.restoreCall(t, cookie, csrf, "check", map[string]string{"uploadId": id, "passphrase": restorePass})
	if rec.Code != http.StatusOK {
		t.Fatalf("check status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		InstanceName string `json:"instanceName"`
		AppVersion   string `json:"appVersion"`
		Schema       int64  `json:"schema"`
		Counts       struct {
			Users        int64 `json:"users"`
			AuditEntries int64 `json:"auditEntries"`
		} `json:"counts"`
		PublicURL        string `json:"publicUrl"`
		Older            bool   `json:"older"`
		PublicURLDiffers bool   `json:"publicUrlDiffers"`
		Created          string `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.InstanceName != "Old lab" || got.AppVersion != "v1.2.3" || got.Counts.Users != 1 || got.Counts.AuditEntries < 1 ||
		got.PublicURL != "https://old.example.com" || got.Older || !got.PublicURLDiffers || got.Created == "" || got.Schema == 0 {
		t.Fatalf("summary = %+v", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	d := f.auditDetails(t, "backup.checked")
	if len(d) != 1 || d[0]["app_version"] != "v1.2.3" || d[0]["schema"] == "" || d[0]["format"] == "" {
		t.Fatalf("backup.checked details = %v", d)
	}
	if e := f.audits(t, "backup.checked"); e[0].ActorID != "id-admin" || e[0].ActorType != "user" {
		t.Fatalf("actor = %+v", e[0])
	}
	// The passphrase is in no audit row.
	var rows int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE details LIKE '%' || ? || '%'`, restorePass).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows holding the passphrase = %d, %v", rows, err)
	}
}

func TestRestoreCheckSaysTheBackupAddressIsTheOneInUseWhenItMatches(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://same.example.com"})
	cookie, csrf := f.signIn(t, "admin", false)
	id := uploadedID(t, f.upload(t, cookie, csrf, makeBackup(t, "Old lab", "oldadmin", "https://same.example.com/")))
	rec := f.restoreCall(t, cookie, csrf, "check", map[string]string{"uploadId": id, "passphrase": restorePass})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"publicUrlDiffers":false`) {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
}

func TestRestoreApplyByAnInstanceAdminSwapsInTheBackupOnceTheServerHasStopped(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.inst.Apply("Home lab", "")
	cookie, csrf := f.signIn(t, "admin", false)
	id := uploadedID(t, f.upload(t, cookie, csrf, makeBackup(t, "Old lab", "oldadmin", "")))

	// A confirmation that is not the instance's name changes nothing.
	rec := f.restoreCall(t, cookie, csrf, "apply", map[string]string{"uploadId": id, "passphrase": restorePass, "confirm": "Old lab"})
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "confirm_mismatch" {
		t.Fatalf("apply with the wrong name = %d %s", rec.Code, rec.Body)
	}
	if len(f.restore.swaps) != 0 {
		t.Fatal("a restart was requested for a refused confirmation")
	}
	if _, err := os.Stat(filepath.Join(f.dataInfo.Path, "bayplan.db")); err == nil {
		t.Fatal("a refused confirmation put a database in the data directory")
	}

	rec = f.restoreCall(t, cookie, csrf, "apply", map[string]string{"uploadId": id, "passphrase": restorePass, "confirm": " Home lab "})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"restarting":true`) {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	if len(f.restore.swaps) != 1 {
		t.Fatalf("restarts requested = %d, want 1", len(f.restore.swaps))
	}
	// Nothing is live yet: the swap runs after the server has stopped.
	if _, err := os.Stat(filepath.Join(f.dataInfo.Path, "bayplan.db")); err == nil {
		t.Fatal("the database was swapped before the server stopped")
	}

	if err := f.restore.swaps[0](context.Background()); err != nil {
		t.Fatalf("swap: %v", err)
	}
	m, pending, err := backup.ReadMarker(f.dataInfo.Path)
	if err != nil || !pending {
		t.Fatalf("marker after the swap = %+v, %v, %v", m, pending, err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.dataInfo.Path, "restore-staging", "*")); len(left) != 0 {
		t.Fatalf("the swap left %v in the staging directory", left)
	}

	// The swapped database is the backup's, re-keyed to this instance and verifying
	// under its key alone, with the re-anchor and the restore recorded.
	st, err := store.Open(context.Background(), f.dataInfo.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.RQ.GetUserByUsername(context.Background(), "oldadmin"); err != nil {
		t.Fatalf("the restored admin is missing: %v", err)
	}
	if res, err := audit.Verify(context.Background(), st, f.v); err != nil || res.Broken != nil {
		t.Fatalf("restored chain = %+v, %v", res, err)
	}
	var actions []string
	rows, err := st.Read.Query(`SELECT action FROM audit_log ORDER BY seq DESC LIMIT 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	if strings.Join(actions, ",") != "backup.restored,audit.reanchored" {
		t.Fatalf("last audit actions = %v, want backup.restored after audit.reanchored", actions)
	}
	var source string
	if err := st.Read.QueryRow(`SELECT json_extract(details, '$.source') FROM audit_log WHERE action = 'backup.restored'`).Scan(&source); err != nil || source != "ui" {
		t.Fatalf("backup.restored source = %q, %v", source, err)
	}

	// While the restart is pending no second restore starts.
	rec = f.restoreCall(t, cookie, csrf, "apply", map[string]string{"uploadId": id, "passphrase": restorePass, "confirm": "Home lab"})
	if rec.Code != http.StatusConflict || problemType(t, rec) != "restore_busy" {
		t.Fatalf("second apply = %d %s", rec.Code, rec.Body)
	}
}

func TestRestoreApplyWhenARestartIsAlreadyPendingDiscardsThePreparedCopy(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.inst.Apply("Home lab", "")
	cookie, csrf := f.signIn(t, "admin", false)
	id := uploadedID(t, f.upload(t, cookie, csrf, makeBackup(t, "Old lab", "oldadmin", "")))
	f.restoreDeps(slog.New(slog.DiscardHandler)) // creates the hook
	f.restore.refuse = true

	rec := f.restoreCall(t, cookie, csrf, "apply", map[string]string{"uploadId": id, "passphrase": restorePass, "confirm": "Home lab"})
	if rec.Code != http.StatusConflict || problemType(t, rec) != "restore_busy" {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	left, _ := filepath.Glob(filepath.Join(f.dataInfo.Path, "restore-staging", "r-*"))
	if len(left) != 0 {
		t.Fatalf("a refused restart left a staged database: %v", left)
	}
}

func TestRestoreRoutesAreRefusedForAnyoneButAnInstanceAdmin(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	file := makeBackup(t, "Old lab", "oldadmin", "")

	if rec := f.upload(t, cookie, csrf, file); rec.Code != http.StatusForbidden {
		t.Fatalf("upload by a viewer = %d, want 403: %s", rec.Code, rec.Body)
	}
	if rec := f.upload(t, "", "", file); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload = %d, want 401: %s", rec.Code, rec.Body)
	}
	body := map[string]string{"uploadId": "u-00000000000000000000000000000000", "passphrase": restorePass, "confirm": ""}
	for _, step := range []string{"check", "apply"} {
		if rec := f.restoreCall(t, cookie, csrf, step, body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s by a viewer = %d, want 403: %s", step, rec.Code, rec.Body)
		}
		if rec := f.restoreCall(t, "", "", step, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d, want 401: %s", step, rec.Code, rec.Body)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(f.dataInfo.Path, "restore-staging", "*")); len(left) != 0 {
		t.Fatalf("a refused upload left %v", left)
	}
	if len(f.audits(t, "backup.checked")) != 0 {
		t.Fatal("a refused check was audited as a check")
	}
}

func TestBackupInfoReportsTheLargestAcceptedUpload(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/info", cookie: cookie})
	var got struct {
		MaxUploadBytes int64 `json:"maxUploadBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.MaxUploadBytes != 1<<30 {
		t.Fatalf("info = %s (%v), want maxUploadBytes 1 GiB", rec.Body, err)
	}
}

// liveDatabase puts a migrated database in dir, as a running instance has, and
// returns its SHA-256, to prove that a refused restore changed nothing.
func liveDatabase(t *testing.T, dir string) func() [32]byte {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "fixture", DataDir: dir, SkipBackup: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	sum := func() [32]byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "bayplan.db")) //nolint:gosec // a path the test made
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	return sum
}

// noTrace fails the test when a refused request left a staged database, an upload
// it should have removed, a restore marker or a pre-restore snapshot.
func noTrace(t *testing.T, dir string, wantUpload bool) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(dir, "restore-staging", "r-*")); len(m) != 0 {
		t.Errorf("a staged database was left behind: %v", m)
	}
	uploads, _ := filepath.Glob(filepath.Join(dir, "restore-staging", "u-*"))
	if wantUpload != (len(uploads) == 1) {
		t.Errorf("uploads on disk = %v, want one: %v", uploads, wantUpload)
	}
	if _, err := os.Stat(filepath.Join(dir, backup.PendingMarker)); err == nil {
		t.Error("a restore marker exists")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "backups", "prerestore-*")); len(m) != 0 {
		t.Errorf("a pre-restore snapshot exists: %v", m)
	}
}

// newerBackupFile is a backup whose header says it was made by a Bayplan with a
// database schema this binary does not know.
func newerBackupFile(t *testing.T) []byte {
	t.Helper()
	salt := bytes.Repeat([]byte{1}, 16)
	var buf bytes.Buffer
	w, err := backup.NewWriter(&buf, backup.Header{
		KDF:        backup.KDFParams{Name: "argon2id", T: 1, MKiB: 8 * 1024, P: 1, Salt: salt},
		AppVersion: "v7.7.7", Schema: 9999,
	}, []byte(restorePass))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(backup.EntryManifest, 2, strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(backup.EntryDB, 1, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hostileHeaderFile claims 1 GiB of key-derivation memory. Honouring it would
// cost the server a gigabyte before it could say no.
func hostileHeaderFile(t *testing.T) []byte {
	t.Helper()
	hdr, err := json.Marshal(backup.Header{
		Format: 1, Created: time.Unix(0, 0).UTC(), AppVersion: "v1.0.0", Schema: 1,
		KDF: backup.KDFParams{Name: "argon2id", T: 3, MKiB: 1 << 20, P: 4, Salt: bytes.Repeat([]byte{1}, 16)},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(backup.Magic), 0, 1)
	out = binary.BigEndian.AppendUint32(out, uint32(len(hdr))) //nolint:gosec // a short test header
	out = append(out, hdr...)
	return append(out, bytes.Repeat([]byte{0xAA}, 512)...)
}

func TestRestoreRefusalsAnswerTheirOwnProblemAndLeaveEverythingAsItWas(t *testing.T) {
	good := makeBackup(t, "Old lab", "oldadmin", "")
	damaged := bytes.Clone(good)
	damaged[len(damaged)-20] ^= 0xFF

	cases := []struct {
		name       string
		file       []byte
		passphrase string
		step       string
		status     int
		problem    string
		detail     string
	}{
		{"a wrong passphrase", good, "not the passphrase", "check", http.StatusUnprocessableEntity, "backup_unreadable", ""},
		{"a wrong passphrase on apply", good, "not the passphrase", "apply", http.StatusUnprocessableEntity, "backup_unreadable", ""},
		{"a damaged payload", damaged, restorePass, "apply", http.StatusUnprocessableEntity, "backup_unreadable", ""},
		{"a text file", []byte(strings.Repeat("this is not a backup\n", 50)), restorePass, "check", http.StatusUnprocessableEntity, "not_a_backup", ""},
		{"a text file on apply", []byte(strings.Repeat("this is not a backup\n", 50)), restorePass, "apply", http.StatusUnprocessableEntity, "not_a_backup", ""},
		{"a newer schema", newerBackupFile(t), restorePass, "check", http.StatusConflict, "backup_newer", "v7.7.7"},
		{"a newer schema on apply", newerBackupFile(t), restorePass, "apply", http.StatusConflict, "backup_newer", "v7.7.7"},
		{"a header that asks for a gigabyte of key memory", hostileHeaderFile(t), restorePass, "check", http.StatusUnprocessableEntity, "backup_unreadable", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t, config.Config{})
			f.inst.Apply("Home lab", "")
			sum := liveDatabase(t, f.dataInfo.Path)
			before := sum()
			cookie, csrf := f.signIn(t, "admin", false)
			id := uploadedID(t, f.upload(t, cookie, csrf, tc.file))

			started := time.Now()
			body := map[string]string{"uploadId": id, "passphrase": tc.passphrase}
			if tc.step == "apply" {
				body["confirm"] = "Home lab"
			}
			rec := f.restoreCall(t, cookie, csrf, tc.step, body)
			if rec.Code != tc.status || problemType(t, rec) != tc.problem {
				t.Fatalf("%s = %d %s, want %d %s", tc.step, rec.Code, rec.Body, tc.status, tc.problem)
			}
			if tc.detail != "" && !strings.Contains(rec.Body.String(), tc.detail) {
				t.Errorf("the answer does not name %s: %s", tc.detail, rec.Body)
			}
			if time.Since(started) > 5*time.Second {
				t.Errorf("the refusal took %v: the file must be refused before a key is derived from it", time.Since(started))
			}
			if sum() != before {
				t.Error("bayplan.db changed")
			}
			if len(f.restore.swaps) != 0 {
				t.Error("a restart was requested")
			}
			noTrace(t, f.dataInfo.Path, true)
			if strings.Contains(rec.Body.String(), tc.passphrase) {
				t.Error("the answer echoes the passphrase")
			}
		})
	}
}

func TestRestoreAllowsOneUploadAtATimeAndFreesTheSlotAfterFifteenMinutes(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	now := time.Now()
	f.restore.uploads.Now = func() time.Time { return now }
	file := makeBackup(t, "Old lab", "oldadmin", "")

	first := uploadedID(t, f.upload(t, cookie, csrf, file))
	rec := f.upload(t, cookie, csrf, file)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "restore_busy" {
		t.Fatalf("second upload = %d %s, want 409 restore_busy", rec.Code, rec.Body)
	}
	now = now.Add(16 * time.Minute)
	second := uploadedID(t, f.upload(t, cookie, csrf, file))
	if second == first {
		t.Fatal("the new upload has the old one's ID")
	}
	rec = f.restoreCall(t, cookie, csrf, "check", map[string]string{"uploadId": first, "passphrase": restorePass})
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "upload_not_found" {
		t.Fatalf("check of the expired upload = %d %s, want 404 upload_not_found", rec.Code, rec.Body)
	}
}

func TestRestoreUploadAboveTheCapIs413AndACutOffUploadIs400AndNeitherLeavesAFile(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.restore.uploads.MaxBytes = 2048

	big := bytes.Repeat([]byte("x"), 4096)
	rec := f.upload(t, cookie, csrf, big)
	if rec.Code != http.StatusRequestEntityTooLarge || problemType(t, rec) != "upload_too_large" {
		t.Fatalf("upload above the cap = %d %s, want 413 upload_too_large", rec.Code, rec.Body)
	}
	noTrace(t, f.dataInfo.Path, false)

	// No Content-Length, so the cap is found while reading.
	req := httptest.NewRequest(http.MethodPost, "/api/backup/restore/upload", bytes.NewReader(big))
	req.ContentLength = -1
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
	req.Header.Set("X-CSRF-Token", csrf)
	out := httptest.NewRecorder()
	f.h.ServeHTTP(out, req)
	if out.Code != http.StatusRequestEntityTooLarge || problemType(t, out) != "upload_too_large" {
		t.Fatalf("unannounced upload above the cap = %d %s", out.Code, out.Body)
	}
	noTrace(t, f.dataInfo.Path, false)

	// The body ends before the length it announced: a proxy cut it off.
	short := bytes.Repeat([]byte("x"), 1000)
	req = httptest.NewRequest(http.MethodPost, "/api/backup/restore/upload", bytes.NewReader(short))
	req.ContentLength = 1500
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: cookie}) //nolint:gosec // a request cookie as a browser sends it
	req.Header.Set("X-CSRF-Token", csrf)
	out = httptest.NewRecorder()
	f.h.ServeHTTP(out, req)
	if out.Code != http.StatusBadRequest || problemType(t, out) != "upload_incomplete" {
		t.Fatalf("cut-off upload = %d %s, want 400 upload_incomplete", out.Code, out.Body)
	}
	noTrace(t, f.dataInfo.Path, false)
}

func TestRestoreTwoConcurrentAppliesOfOneUploadGiveOne202AndOne409(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.inst.Apply("Home lab", "")
	cookie, csrf := f.signIn(t, "admin", false)
	id := uploadedID(t, f.upload(t, cookie, csrf, makeBackup(t, "Old lab", "oldadmin", "")))

	codes := make([]int, 2)
	types := make([]string, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := f.restoreCall(t, cookie, csrf, "apply", map[string]string{"uploadId": id, "passphrase": restorePass, "confirm": "Home lab"})
			codes[i] = rec.Code
			if rec.Code != http.StatusAccepted {
				types[i] = problemType(t, rec)
			}
		}()
	}
	close(start)
	wg.Wait()
	accepted, busy := 0, 0
	for i, c := range codes {
		switch {
		case c == http.StatusAccepted:
			accepted++
		case c == http.StatusConflict && types[i] == "restore_busy":
			busy++
		default:
			t.Errorf("apply %d = %d %s", i, c, types[i])
		}
	}
	if accepted != 1 || busy != 1 {
		t.Fatalf("accepted = %d, busy = %d, want one of each", accepted, busy)
	}
	if len(f.restore.swaps) != 1 {
		t.Fatalf("restarts requested = %d, want 1", len(f.restore.swaps))
	}
}

// wizardFixture is a fresh install: no user yet, a setup token, and the restore
// routes the setup wizard calls.
type wizardFixture struct {
	h       http.Handler
	st      *store.Store
	v       *vault.Vault
	token   string
	dataDir string
	restore *restoreHook
	clk     *fakeClock
}

func newWizardFixture(t *testing.T) *wizardFixture {
	t.Helper()
	st := storetest.New(t)
	log := slog.New(slog.DiscardHandler)
	v, err := vault.LoadOrCreate(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, v, log, nil)
	inst, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	gate := auth.NewSetupGate(st, dataDir, log)
	if err := gate.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	token, _ := auth.ReadTokenFile(dataDir)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	hook := &restoreHook{uploads: backup.NewUploads(dataDir, 0)}

	r := httpx.New(httpx.Options{Auditor: aud, Logger: log, PublicOrigin: inst.PublicOrigin})
	api.RegisterRestore(r, api.RestoreDeps{
		Uploads: hook.uploads, Vault: v, Audit: aud, Instance: inst, DataDir: dataDir, AppVersion: "v9.9.9", Now: clk.now, Log: log,
		Setup: api.SetupDeps{
			Gate: gate, Limiter: auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, clk.now),
			Audit: aud, Store: st, Instance: inst, Log: log,
		},
		Restart: func(swap func(context.Context) error) bool {
			hook.swaps = append(hook.swaps, swap)
			return true
		},
	})
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Fatal(err)
	}
	return &wizardFixture{h: r.Handler(), st: st, v: v, token: token, dataDir: dataDir, restore: hook, clk: clk}
}

func (w *wizardFixture) upload(t *testing.T, token string, file []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/setup/restore/upload", bytes.NewReader(file))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Setup-Token", token)
	req.RemoteAddr = testClientAddr
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec
}

func (w *wizardFixture) call(t *testing.T, step string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/setup/restore/"+step, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = testClientAddr
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec
}

func (w *wizardFixture) addUser(t *testing.T) {
	t.Helper()
	now := time.Now().UnixNano()
	if err := w.st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: "x", CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWizardRestoreWithTheSetupTokenUploadsChecksAndRestoresOnAFreshInstall(t *testing.T) {
	w := newWizardFixture(t)
	id := uploadedID(t, w.upload(t, w.token, makeBackup(t, "Old lab", "oldadmin", "")))

	rec := w.call(t, "check", map[string]string{"setupToken": w.token, "uploadId": id, "passphrase": restorePass})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"instanceName":"Old lab"`) {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	rec = w.call(t, "apply", map[string]string{"setupToken": w.token, "uploadId": id, "passphrase": restorePass})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"restarting":true`) {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	if len(w.restore.swaps) != 1 {
		t.Fatalf("restarts requested = %d, want 1", len(w.restore.swaps))
	}
	if err := w.restore.swaps[0](context.Background()); err != nil {
		t.Fatalf("swap: %v", err)
	}
	st, err := store.Open(context.Background(), w.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.RQ.GetUserByUsername(context.Background(), "oldadmin"); err != nil {
		t.Fatalf("the restored admin is missing: %v", err)
	}
	var source string
	if err := st.Read.QueryRow(`SELECT json_extract(details, '$.source') FROM audit_log WHERE action = 'backup.restored'`).Scan(&source); err != nil || source != "wizard" {
		t.Fatalf("backup.restored source = %q, %v, want wizard", source, err)
	}
}

func TestWizardRestoreRoutesAnswer409SetupCompleteOnceAnyUserExists(t *testing.T) {
	w := newWizardFixture(t)
	file := makeBackup(t, "Old lab", "oldadmin", "")
	id := uploadedID(t, w.upload(t, w.token, file))
	w.addUser(t)

	body := map[string]string{"setupToken": w.token, "uploadId": id, "passphrase": restorePass}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"upload": w.upload(t, w.token, file),
		"check":  w.call(t, "check", body),
		"apply":  w.call(t, "apply", body),
	} {
		if rec.Code != http.StatusConflict || problemType(t, rec) != "setup_complete" {
			t.Errorf("%s after setup = %d %s, want 409 setup_complete", name, rec.Code, rec.Body)
		}
	}
	if len(w.restore.swaps) != 0 {
		t.Fatal("a restart was requested after setup was complete")
	}
	if _, err := os.Stat(filepath.Join(w.dataDir, "bayplan.db")); err == nil {
		t.Fatal("a database was swapped in after setup was complete")
	}
}

func TestWizardRestoreWithAWrongTokenIs401RateLimitedAndRecorded(t *testing.T) {
	w := newWizardFixture(t)
	file := makeBackup(t, "Old lab", "oldadmin", "")

	for i := range 5 {
		rec := w.upload(t, "not-the-token", file)
		if rec.Code != http.StatusUnauthorized || problemType(t, rec) != "invalid_setup_token" {
			t.Fatalf("attempt %d = %d %s, want 401 invalid_setup_token", i+1, rec.Code, rec.Body)
		}
	}
	rec := w.call(t, "check", map[string]string{"setupToken": "not-the-token", "uploadId": "u-x", "passphrase": restorePass})
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Fatalf("sixth attempt = %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	// Even the right token waits its turn: the address has used its attempts.
	if rec := w.upload(t, w.token, file); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("right token from a limited address = %d, want 429", rec.Code)
	}
	var failed int
	if err := w.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'setup.token_failed'`).Scan(&failed); err != nil || failed < 6 {
		t.Fatalf("setup.token_failed entries = %d, %v, want the five wrong tokens and the limited attempts", failed, err)
	}
	if left, _ := filepath.Glob(filepath.Join(w.dataDir, "restore-staging", "*")); len(left) != 0 {
		t.Fatalf("refused uploads left %v", left)
	}
}

func TestWizardRestoreRefusesABadFileWithTheSameProblemsAsSettings(t *testing.T) {
	w := newWizardFixture(t)
	id := uploadedID(t, w.upload(t, w.token, makeBackup(t, "Old lab", "oldadmin", "")))
	rec := w.call(t, "check", map[string]string{"setupToken": w.token, "uploadId": id, "passphrase": "not the passphrase"})
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "backup_unreadable" {
		t.Fatalf("wrong passphrase = %d %s", rec.Code, rec.Body)
	}
	rec = w.call(t, "apply", map[string]string{"setupToken": w.token, "uploadId": id, "passphrase": "not the passphrase"})
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "backup_unreadable" {
		t.Fatalf("wrong passphrase on apply = %d %s", rec.Code, rec.Body)
	}
	if len(w.restore.swaps) != 0 {
		t.Fatal("a restart was requested")
	}
}
