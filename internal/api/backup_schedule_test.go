// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

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
	"time"

	"github.com/ianglennon/coaming/internal/config"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	for _, r := range []authReq{
		{method: http.MethodGet, path: "/api/backup/schedule", cookie: cookie},
		{method: http.MethodPut, path: "/api/backup/schedule", body: scheduleJSON(true, schedulePass), cookie: cookie, csrf: csrf},
		{method: http.MethodGet, path: "/api/backup/files", cookie: cookie},
		{method: http.MethodGet, path: "/api/backup/files/coaming-backup-20261001-0300.coaming", cookie: cookie},
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
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	if err := os.MkdirAll(f.backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const name = "coaming-backup-20260101-0300.coaming"
	content := bytes.Repeat([]byte("x"), 1234)
	if err := os.WriteFile(filepath.Join(f.backupDir, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.backupDir, "coaming-20260101T030000Z.db"), []byte("snapshot"), 0o600); err != nil {
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
	t.Parallel()
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

// Plan 03-27 task 2: what the schedule routes refuse, the files route's names,
// and the backup_failed warning.

func rawSchedule(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestAScheduleOutOfRangeIsAnswered422AndSavesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	base := func() map[string]any {
		return map[string]any{"enabled": true, "frequency": "daily", "timeUtc": "03:00", "keep": 7, "passphrase": schedulePass}
	}
	for name, mutate := range map[string]func(map[string]any){
		"time 25:00":        func(m map[string]any) { m["timeUtc"] = "25:00" },
		"time 03:60":        func(m map[string]any) { m["timeUtc"] = "03:60" },
		"keep 0":            func(m map[string]any) { m["keep"] = 0 },
		"keep 366":          func(m map[string]any) { m["keep"] = 366 },
		"frequency hourly":  func(m map[string]any) { m["frequency"] = "hourly" },
		"weekday 7":         func(m map[string]any) { m["frequency"], m["weekday"] = "weekly", 7 },
		"a missing keep":    func(m map[string]any) { delete(m, "keep") },
		"a missing time":    func(m map[string]any) { delete(m, "timeUtc") },
		"an overlong value": func(m map[string]any) { m["passphrase"] = strings.Repeat("x", 1025) },
	} {
		m := base()
		mutate(m)
		if rec := f.putSchedule(t, cookie, csrf, rawSchedule(m)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.Code, rec.Body)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM backup_schedule`).Scan(&n); err != nil || n != 0 {
		t.Errorf("backup_schedule rows = %d, %v", n, err)
	}
}

func TestAWeeklyScheduleIsSavedWithItsDayAndFiresOnThatDay(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	// The fixture's clock is Thursday 2026-01-01 00:00 UTC.
	rec := f.putSchedule(t, cookie, csrf, rawSchedule(map[string]any{
		"enabled": true, "frequency": "weekly", "weekday": 0, "timeUtc": "03:00", "keep": 4, "passphrase": schedulePass,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["frequency"] != "weekly" || got["weekday"] != float64(0) || got["nextRun"] != "2026-01-04T03:00:00Z" {
		t.Fatalf("schedule = %v, want weekly on Sunday with the next run on 2026-01-04", got)
	}
}

func TestTurningTheScheduleOnWithoutAPassphraseIs422PassphraseRequiredAndAShortOneIs422(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.putSchedule(t, cookie, csrf, scheduleJSON(true, ""))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "passphrase_required" {
		t.Fatalf("status = %d, type %q: %s", rec.Code, problemType(t, rec), rec.Body)
	}
	rec = f.putSchedule(t, cookie, csrf, scheduleJSON(true, "fifteen chars!!"))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "passphrase_too_short" {
		t.Fatalf("status = %d, type %q: %s", rec.Code, problemType(t, rec), rec.Body)
	}
	// Off with no passphrase is fine.
	if rec := f.putSchedule(t, cookie, csrf, scheduleJSON(false, "")); rec.Code != http.StatusOK {
		t.Fatalf("saving the schedule off: %d %s", rec.Code, rec.Body)
	}
}

func TestSavingTheScheduleWithoutAPassphraseKeepsTheSavedOne(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	if rec := f.putSchedule(t, cookie, csrf, scheduleJSON(true, schedulePass)); rec.Code != http.StatusOK {
		t.Fatalf("first save: %d %s", rec.Code, rec.Body)
	}
	var before string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rec := f.putSchedule(t, cookie, csrf, rawSchedule(map[string]any{"enabled": true, "frequency": "daily", "timeUtc": "04:30", "keep": 3}))
	if rec.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}
	var after string
	if err := f.st.Read.QueryRow(`SELECT passphrase_sealed FROM backup_schedule`).Scan(&after); err != nil || after != before {
		t.Fatalf("the saved passphrase changed: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["passphraseSaved"] != true || got["timeUtc"] != "04:30" || got["keep"] != float64(3) {
		t.Fatalf("schedule = %v", got)
	}
}

func TestAFileNameThatIsNotABackupAnswers404WithoutTouchingTheFileSystem(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	// Files the route must never serve: the database and the key sit in the data
	// directory, and a decoy with the wrong name sits in the backups folder.
	if err := os.WriteFile(filepath.Join(f.dataInfo.Path, "coaming.db"), []byte("the database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dataInfo.Path, "coaming.key"), []byte("the master key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.backupDir, "x.coaming"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/backup/files/../coaming.db",
		"/api/backup/files/..%2Fcoaming.db",
		"/api/backup/files/%2e%2e%2fcoaming.key",
		"/api/backup/files/coaming.key",
		"/api/backup/files/x.coaming",
		"/api/backup/files/coaming-backup-20261001-0300.coaming",
	} {
		rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie})
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "the database") || strings.Contains(rec.Body.String(), "master key") || strings.Contains(rec.Body.String(), "decoy") {
			t.Errorf("GET %s served a file: %s", path, rec.Body)
		}
	}
	if n := len(f.audits(t, "backup.exported")); n != 0 {
		t.Errorf("backup.exported entries = %d after only refused downloads", n)
	}
}

type warningFull struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Since   string `json:"since"`
}

func (f *authFixture) backupFailed(t *testing.T, cookie string) []warningFull {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/warnings", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/warnings = %d: %s", rec.Code, rec.Body)
	}
	var w struct {
		Items []warningFull `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
		t.Fatal(err)
	}
	var out []warningFull
	for _, it := range w.Items {
		if it.ID == "backup_failed" {
			out = append(out, it)
		}
	}
	return out
}

func (f *authFixture) addRun(t *testing.T, id string, ok bool, finished time.Time, reason string) {
	t.Helper()
	okInt := 0
	if ok {
		okInt = 1
	}
	if _, err := f.st.Write.Exec(`INSERT INTO backup_runs (id, source, started_at, finished_at, ok, file_name, error) VALUES (?, 'schedule', ?, ?, ?, ?, ?)`,
		id, finished.UnixNano(), finished.UnixNano(), okInt, map[bool]string{true: "coaming-backup-x.coaming", false: ""}[ok], reason); err != nil {
		t.Fatal(err)
	}
}

func (f *authFixture) turnScheduleOn(t *testing.T, cookie, csrf string) {
	t.Helper()
	if rec := f.putSchedule(t, cookie, csrf, scheduleJSON(true, schedulePass)); rec.Code != http.StatusOK {
		t.Fatalf("turn the schedule on: %d %s", rec.Code, rec.Body)
	}
}

func TestAFailedScheduledBackupRaisesBackupFailedForAnInstanceAdminUntilALaterRunSucceeds(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.turnScheduleOn(t, cookie, csrf)
	good := f.clk.t.Add(-48 * time.Hour)
	bad := f.clk.t.Add(-24 * time.Hour)

	if got := f.backupFailed(t, cookie); len(got) != 0 {
		t.Fatalf("before any run: %+v", got)
	}
	f.addRun(t, "r1", true, good, "")
	if got := f.backupFailed(t, cookie); len(got) != 0 {
		t.Fatalf("after a good run: %+v", got)
	}
	f.addRun(t, "r2", false, bad, "the disk is full")
	got := f.backupFailed(t, cookie)
	if len(got) != 1 || got[0].Subject != "the disk is full" || got[0].Since != good.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("after a failed run: %+v, want one naming the reason and the last good time %s", got, good.UTC().Format(time.RFC3339Nano))
	}
	f.addRun(t, "r3", true, f.clk.t.Add(-time.Hour), "")
	if got := f.backupFailed(t, cookie); len(got) != 0 {
		t.Fatalf("after a later good run: %+v", got)
	}
}

func TestAFailedBackupWithNoGoodRunBeforeItHasNoSinceAndOneThatIsOffRaisesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	f.turnScheduleOn(t, cookie, csrf)
	f.addRun(t, "r1", false, f.clk.t.Add(-time.Hour), "no space left")
	got := f.backupFailed(t, cookie)
	if len(got) != 1 || got[0].Since != "" {
		t.Fatalf("got %+v, want one with no since", got)
	}
	// Switching the schedule off ends the matter: nothing is going to run.
	if rec := f.putSchedule(t, cookie, csrf, scheduleJSON(false, "")); rec.Code != http.StatusOK {
		t.Fatalf("turn off: %d %s", rec.Code, rec.Body)
	}
	if got := f.backupFailed(t, cookie); len(got) != 0 {
		t.Fatalf("with the schedule off: %+v", got)
	}
}

func TestBackupFailedIsNeverShownToAnyoneButAnInstanceAdmin(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	adminCookie, adminCSRF := f.signIn(t, "admin", false)
	f.turnScheduleOn(t, adminCookie, adminCSRF)
	f.addRun(t, "r1", false, f.clk.t.Add(-time.Hour), "boom")
	f.addUser(t, "viewer", false)
	cookie, _ := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/warnings", cookie: cookie})
	if strings.Contains(rec.Body.String(), "backup_failed") || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("a non-admin was told: %d %s", rec.Code, rec.Body)
	}
}
