// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
)

// The backup schedule and the scheduled backups on disk (plan 03-27, D-32,
// PLAT-03).

const schedulePass = "a long scheduled passphrase"

func scheduleJSON(enabled bool, pass string) string {
	m := map[string]any{"enabled": enabled, "frequency": "daily", "timeUtc": "03:00", "keep": 7}
	if pass != "" {
		m["passphrase"] = pass
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (f *authFixture) putSchedule(t *testing.T, cookie, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPut, path: "/api/backup/schedule", body: body, cookie: cookie, csrf: csrf})
}

type notifyCounter struct{ n int }

func (c *notifyCounter) Notify() { c.n++ }

func TestAnInstanceAdminTurnsOnADailyScheduleAndReadsItBackWithoutThePassphrase(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	notified := &notifyCounter{}
	f.scheduler = notified
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.putSchedule(t, cookie, csrf, scheduleJSON(true, schedulePass))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", rec.Code, rec.Body)
	}
	if notified.n != 1 {
		t.Errorf("the scheduler was told %d times, want once", notified.n)
	}

	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/backup/schedule", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["enabled"] != true || got["frequency"] != "daily" || got["timeUtc"] != "03:00" || got["keep"] != float64(7) || got["passphraseSaved"] != true {
		t.Fatalf("schedule = %v", got)
	}
	if got["dir"] != f.backupDir {
		t.Errorf("dir = %v, want %s", got["dir"], f.backupDir)
	}
	// The fixture's clock is 2026-01-01 00:00 UTC.
	if got["nextRun"] != "2026-01-01T03:00:00Z" {
		t.Errorf("nextRun = %v, want 2026-01-01T03:00:00Z", got["nextRun"])
	}
	if got["lastRun"] != nil {
		t.Errorf("lastRun = %v, want none", got["lastRun"])
	}

	if n := len(f.audits(t, "backup.schedule_updated")); n != 1 {
		t.Errorf("backup.schedule_updated entries = %d, want 1", n)
	}
}

func TestScheduleNoPassphraseIsEverReturnedLoggedOrAudited(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	put := f.putSchedule(t, cookie, csrf, scheduleJSON(true, schedulePass))
	get := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/schedule", cookie: cookie})
	for name, rec := range map[string]*httptest.ResponseRecorder{"PUT": put, "GET": get} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", name, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"passphrase", "passphraseSealed", "passphrase_sealed"} {
			if _, ok := body[k]; ok {
				t.Errorf("%s body has %q", name, k)
			}
		}
		if strings.Contains(rec.Body.String(), schedulePass) {
			t.Errorf("%s body holds the passphrase", name)
		}
	}
	if strings.Contains(f.logs.String(), schedulePass) {
		t.Error("the passphrase is in the log")
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT group_concat(details) FROM audit_log`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, schedulePass) {
		t.Error("the passphrase is in an audit entry")
	}
	var sealed string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule`).Scan(&sealed); err != nil || sealed == "" || strings.Contains(sealed, schedulePass) {
		t.Errorf("stored passphrase = %q, %v", sealed, err)
	}
}

func TestTheScheduleRoutesNeedBackupManage(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	for _, r := range []authReq{
		{method: http.MethodGet, path: "/api/backup/schedule", cookie: cookie},
		{method: http.MethodPut, path: "/api/backup/schedule", body: scheduleJSON(true, schedulePass), cookie: cookie, csrf: csrf},
		{method: http.MethodGet, path: "/api/backup/files", cookie: cookie},
		{method: http.MethodGet, path: "/api/backup/files/bayplan-backup-20261001-0300.bayplan", cookie: cookie},
	} {
		if rec := f.call(t, r); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", r.method, r.path, rec.Code)
		}
	}
	if n := len(f.audits(t, "backup.schedule_updated")); n != 0 {
		t.Errorf("backup.schedule_updated entries = %d, want 0", n)
	}
}

func TestSavedBackupsAreListedAndDownloadedByAnInstanceAdmin(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	if err := os.MkdirAll(f.backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const name = "bayplan-backup-20260101-0300.bayplan"
	content := bytes.Repeat([]byte("x"), 1234)
	if err := os.WriteFile(filepath.Join(f.backupDir, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.backupDir, "bayplan-20260101T030000Z.db"), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/files", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Created string `json:"created"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != name || list.Items[0].Size != 1234 || list.Items[0].Created != "2026-01-01T03:00:00Z" {
		t.Fatalf("list = %+v", list.Items)
	}

	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/backup/files/" + name, cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d: %s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Error("the download is not the file")
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="`+name+`"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Errorf("headers = %v", rec.Header())
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'backup.exported'`).Scan(&details); err != nil || !strings.Contains(details, `"source":"saved"`) || !strings.Contains(details, name) {
		t.Errorf("audit details = %q, %v", details, err)
	}
}

func TestAFolderWithNoBackupsYetListsNone(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/backup/files", cookie: cookie})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("list = %d %s, want an empty items array", rec.Code, rec.Body)
	}
	if _, err := os.Stat(f.backupDir); err == nil {
		t.Error("listing made the backups folder")
	}
}
