// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/config"
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
