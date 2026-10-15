// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The page of earlier lines and the merged download of a service's log (plan 06-31, OBS-02,
// D-35, T-06-91, T-06-92). Docker is the fake swarm manager of the stream tests, whose service
// log is grouped task by task and ignores until (RESEARCH E1, E2).

type svcPage struct {
	Lines       []svcLogLine `json:"lines"`
	StartOfLogs bool         `json:"startOfLogs"`
}

func (f *svcLogsFix) pagePath(service, query string) string {
	return "/api/env/" + f.env.ID + "/services/" + service + "/logs?" + query
}

func (f *svcLogsFix) page(t *testing.T, service, query string) svcPage {
	t.Helper()
	rec := f.req(t, http.MethodGet, f.pagePath(service, query), "")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d (%s): %s", rec.code, rec.ptyp, rec.body)
	}
	var out svcPage
	if err := json.Unmarshal(rec.body, &out); err != nil {
		t.Fatalf("page %s: %v", rec.body, err)
	}
	return out
}

func (f *svcLogsFix) downloadPath(service, query string) string {
	return "/api/env/" + f.env.ID + "/services/" + service + "/logs/download?" + query
}

// startDownload runs a download as the signed-in admin and returns a channel closed when the
// handler has returned.
func (f *svcLogsFix) startDownload(service, query string, w http.ResponseWriter) <-chan struct{} {
	req := httptest.NewRequest(http.MethodGet, f.downloadPath(service, query), nil)
	req.Header.Set("User-Agent", authUA)
	req.RemoteAddr = authIP + ":40000"
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.cookie}) //nolint:gosec // a request cookie as a browser sends it
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(w, req)
	}()
	return done
}

func (f *svcLogsFix) download(t *testing.T, service, query string) *dlWriter {
	t.Helper()
	w := newDLWriter(true)
	select {
	case <-f.startDownload(service, query, w):
	case <-time.After(20 * time.Second):
		t.Fatal("the download never finished")
	}
	return w
}

type svcDownloadRow struct {
	TargetType, TargetID, Env, Result string
	Details                           map[string]string
}

func (f *svcLogsFix) downloadRows(t *testing.T) []svcDownloadRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT target_type, target_id, COALESCE(env_id, ''), result, COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, audit.ActionLogsDownloaded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []svcDownloadRow
	for rows.Next() {
		var r svcDownloadRow
		var details string
		if err := rows.Scan(&r.TargetType, &r.TargetID, &r.Env, &r.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServiceLogPageAnswersTheNewestMergedLinesBeforeUntilWithTheirTasksAndSaysWhenThereAreMore(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	// Before 60 ms there are a1 10, b1 20, c1 30, a2 40 and b2 50 (the ended task's line at 5 is
	// left out unless asked for).
	until := svcT0.Add(60 * time.Millisecond).Format(time.RFC3339Nano)
	got := f.page(t, "web", "sid="+sidA+"&tail=4&until="+until)
	if want := []string{"b1", "c1", "a2", "b2"}; !slices.Equal(svcTexts(got.Lines), want) {
		t.Fatalf("lines = %v, want %v", svcTexts(got.Lines), want)
	}
	if got.StartOfLogs {
		t.Error("startOfLogs is true though four of five earlier lines came back")
	}
	first := got.Lines[0]
	if first.T != svcStamp(20) || first.S != "o" || first.Task != "tk-2" || first.Slot != 2 || first.Node != "wrk-ready" {
		t.Errorf("first line = %+v, want b1 of tk-2, slot 2 on wrk-ready, stdout, at 20 ms", first)
	}

	all := f.page(t, "web", "sid="+sidA+"&tail=50&until="+until)
	if want := []string{"a1", "b1", "c1", "a2", "b2"}; !slices.Equal(svcTexts(all.Lines), want) || !all.StartOfLogs {
		t.Fatalf("page = %v, startOfLogs %v, want the five earlier lines in time order and the start", svcTexts(all.Lines), all.StartOfLogs)
	}
	// A page is a bounded read of the whole service: it does not follow, and it asks for a
	// window of time, not for a tail the engine would apply per task before any until.
	qs := f.fake.ServiceLogQueries()
	for _, q := range qs {
		if q.Get("follow") == "1" || q.Get("tail") != "" {
			t.Errorf("the page asked the engine %v, want no follow and no tail", q)
		}
	}
	if qs[0].Get("since") == "" {
		t.Errorf("the first read had no since: %v", qs[0])
	}
}

func TestServiceLogPageWithStoppedTasksIncludesTheLinesOfTasksThatHaveEnded(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	until := svcT0.Add(60 * time.Millisecond).Format(time.RFC3339Nano)
	got := f.page(t, "web", "sid="+sidA+"&tail=50&stopped=1&until="+until)
	if want := []string{"old", "a1", "b1", "c1", "a2", "b2"}; !slices.Equal(svcTexts(got.Lines), want) {
		t.Fatalf("lines = %v, want %v", svcTexts(got.Lines), want)
	}
}

func TestServiceLogPageNarrowsToTheTasksAndNodesAsked(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	until := svcT0.Add(time.Second).Format(time.RFC3339Nano)
	byTask := f.page(t, "web", "sid="+sidA+"&tail=50&task=tk-2&until="+until)
	if want := []string{"b1", "b2"}; !slices.Equal(svcTexts(byTask.Lines), want) {
		t.Errorf("task=tk-2: lines = %v, want %v", svcTexts(byTask.Lines), want)
	}
	byNode := f.page(t, "web", "sid="+sidA+"&tail=50&node=mgr-1&until="+until)
	if want := []string{"a1", "c1", "a2", "a3"}; !slices.Equal(svcTexts(byNode.Lines), want) {
		t.Errorf("node=mgr-1: lines = %v, want %v", svcTexts(byNode.Lines), want)
	}
}

func TestServiceLogPageFindsEarlierLinesFarBackWithoutReadingTheWholeLogFirst(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lines := []dockertest.FakeServiceLogLine{
		{TaskID: "tk-1", NodeID: "node-1", Time: old, Text: "ancient 1"},
		{TaskID: "tk-2", NodeID: "node-3", Time: old.Add(time.Second), Text: "ancient 2"},
		{TaskID: "tk-1", NodeID: "node-1", Time: old.Add(90 * 24 * time.Hour), Text: "recent"},
	}
	f := newSvcLogsFix(t, svcTasks(), lines, nil)
	got := f.page(t, "web", "sid="+sidA+"&tail=2&until="+old.Add(91*24*time.Hour).Format(time.RFC3339Nano))
	if want := []string{"ancient 2", "recent"}; !slices.Equal(svcTexts(got.Lines), want) || got.StartOfLogs {
		t.Fatalf("page = %v, startOfLogs %v, want the last two lines and not the start", svcTexts(got.Lines), got.StartOfLogs)
	}
	again := f.page(t, "web", "sid="+sidA+"&tail=2&until="+got.Lines[0].T)
	if want := []string{"ancient 1"}; !slices.Equal(svcTexts(again.Lines), want) || !again.StartOfLogs {
		t.Fatalf("second page = %v, startOfLogs %v, want ancient 1 and the start", svcTexts(again.Lines), again.StartOfLogs)
	}
	// The first page needed several windows, each bounded by a since; the window widens eightfold.
	qs := f.fake.ServiceLogQueries()
	if len(qs) < 3 {
		t.Fatalf("%d engine reads, want the windows to widen", len(qs))
	}
	if qs[0].Get("since") == "" {
		t.Errorf("the first read had no since: %v", qs[0])
	}
}

func TestServiceLogPageIsEmptyAndTheStartWhenNothingIsEarlier(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	got := f.page(t, "web", "sid="+sidA+"&tail=10&stopped=1&until="+svcT0.Format(time.RFC3339Nano))
	if len(got.Lines) != 0 || !got.StartOfLogs {
		t.Errorf("page = %+v, want no lines and the start of logs", got)
	}
}

func TestServiceLogPageKeepsLinesThatShareAStampOnEitherSideOfThePageBoundary(t *testing.T) {
	at := svcT0.Add(5 * time.Nanosecond)
	lines := []dockertest.FakeServiceLogLine{
		{TaskID: "tk-1", NodeID: "node-1", Time: at, Text: "x1"},
		{TaskID: "tk-2", NodeID: "node-3", Time: at, Text: "x2"},
		{TaskID: "tk-3", NodeID: "node-1", Time: at, Text: "x3"},
		{TaskID: "tk-1", NodeID: "node-1", Time: at.Add(time.Second), Text: "y"},
	}
	f := newSvcLogsFix(t, svcTasks(), lines, nil)
	// The viewer holds x3 and y. To page past x3 without losing x1 and x2 it asks for everything
	// up to one nanosecond after x3's stamp and drops the x3 it already has.
	got := f.page(t, "web", "sid="+sidA+"&tail=2&until="+at.Add(time.Nanosecond).Format(time.RFC3339Nano))
	if want := []string{"x2", "x3"}; !slices.Equal(svcTexts(got.Lines), want) {
		t.Fatalf("lines = %v, want the last two of the three at the stamp, lower slot first", svcTexts(got.Lines))
	}
	if got.StartOfLogs {
		t.Error("x1 is still earlier, so this is not the start")
	}
}

func TestServiceLogPageKeepsTheNewestLinesWhenThePageIsOverItsByteBudget(t *testing.T) {
	var lines []dockertest.FakeServiceLogLine
	for i := range 10 {
		lines = append(lines, svcLine("tk-1", "node-1", i*10, strings.Repeat("z", 100)+string(rune('a'+i))))
	}
	f := newSvcLogsFix(t, svcTasks(), lines, func(f *envFix) { f.serviceLogsMaxPageBytes = 3 * (30 + 101) })
	got := f.page(t, "web", "sid="+sidA+"&tail=50&until="+svcT0.Add(time.Second).Format(time.RFC3339Nano))
	if len(got.Lines) != 3 || !strings.HasSuffix(got.Lines[2].M, "j") || !strings.HasSuffix(got.Lines[0].M, "h") {
		t.Fatalf("lines = %d, want the newest three (h, i, j)", len(got.Lines))
	}
	if got.StartOfLogs {
		t.Error("startOfLogs is true though lines were left out for the size limit")
	}
}

func TestServiceLogPageSharesTheViewersAuditMemoryAndWritesNoRowForTheSameViewer(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	c := f.open(t, f.cookie, "web", "sid="+sidA, "")
	if fr, ok := c.next(5 * time.Second); !ok || fr.event != "ready" {
		t.Fatalf("first frame = %+v, want ready", fr)
	}
	if n := len(f.viewedRows(t)); n != 1 {
		t.Fatalf("%d audit rows after the stream opened, want 1", n)
	}
	until := svcT0.Add(time.Second).Format(time.RFC3339Nano)
	f.page(t, "web", "sid="+sidA+"&tail=5&until="+until)
	f.page(t, "web", "sid="+sidA+"&tail=5&until="+until)
	if n := len(f.viewedRows(t)); n != 1 {
		t.Errorf("%d audit rows after two pages of the same viewer, want still 1", n)
	}
	// Another viewer reading a page is another viewer.
	f.page(t, "web", "sid="+sidB+"&tail=5&until="+until)
	rows := f.viewedRows(t)
	if len(rows) != 2 {
		t.Fatalf("%d audit rows after a second viewer's page, want 2", len(rows))
	}
	if rows[1].TargetType != "service" || rows[1].TargetID != "svc-web" || rows[1].Details["service"] != "web" {
		t.Errorf("row = %+v, want the service as the target", rows[1])
	}
}

func TestServiceLogPageRefusesWhatIsNotAPageAndAuditsNothing(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	for _, query := range []string{
		"", "sid=short&tail=5", "sid=" + sidA + "&tail=0", "sid=" + sidA + "&tail=2001",
		"sid=" + sidA + "&until=yesterday", "sid=" + sidA + "&since=nope", "sid=" + sidA + "&stopped=maybe",
	} {
		rec := f.req(t, http.MethodGet, f.pagePath("web", query), "")
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%q: status = %d, want 422: %s", query, rec.code, rec.body)
		}
	}
	rec := f.req(t, http.MethodGet, f.pagePath("nope", "sid="+sidA+"&tail=5"), "")
	if rec.code != http.StatusNotFound {
		t.Errorf("unknown service: status = %d, want 404: %s", rec.code, rec.body)
	}
	if n := len(f.viewedRows(t)); n != 0 {
		t.Errorf("%d audit rows for pages that read nothing", n)
	}
}

func TestServiceLogPageNeedsServicesRead(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: f.pagePath("web", "sid="+sidA+"&tail=5"), cookie: cookie})
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want a refusal", rec.Code)
	}
}

// line is a line of a download as the server writes it: the task's name, the UTC time, the text.
func dlLine(prefix string, ms int, text string) string {
	return prefix + " " + svcStamp(ms) + " " + text
}

func TestServiceLogDownloadMergesAllTasksInTimeOrderEachStartingWithTheTaskNameAndUTCTime(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	w := f.download(t, "web", "")
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	want := strings.Join([]string{
		dlLine("web.1@mgr-1", 5, "old"),
		dlLine("web.1@mgr-1", 10, "a1"),
		dlLine("web.2@wrk-ready", 20, "b1"),
		dlLine("web.3@mgr-1", 30, "c1"),
		dlLine("web.1@mgr-1", 40, "a2"),
		dlLine("web.2@wrk-ready", 50, "b2"),
		dlLine("web.1@mgr-1", 70, "a3"),
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	h := w.Header()
	wantName := "web-" + f.clk.now().UTC().Format("20060102-1504") + ".log"
	if cd := h.Get("Content-Disposition"); cd != `attachment; filename="`+wantName+`"` {
		t.Errorf("Content-Disposition = %q, want the service name and the UTC minute %s", cd, wantName)
	}
	if h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", h)
	}
	if len(w.deadlns) == 0 || !w.deadlns[0].IsZero() {
		t.Errorf("write deadlines = %v, want the first cleared", w.deadlns)
	}
}

func TestServiceLogDownloadReadsTheRangeFromTheEngineAndCutsAtUntil(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	since, until := svcT0.Add(20*time.Millisecond), svcT0.Add(50*time.Millisecond)
	w := f.download(t, "web", "since="+since.Format(time.RFC3339Nano)+"&until="+until.Format(time.RFC3339Nano))
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	// The engine ignores until, so the download cuts it: 20 and 40 are in, 50 is not.
	want := strings.Join([]string{
		dlLine("web.2@wrk-ready", 20, "b1"),
		dlLine("web.3@mgr-1", 30, "c1"),
		dlLine("web.1@mgr-1", 40, "a2"),
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	qs := f.fake.ServiceLogQueries()
	last := qs[len(qs)-1]
	if last.Get("since") == "" || last.Get("follow") == "1" || last.Get("tail") != "" {
		t.Errorf("the engine was asked %v, want the range by since, no follow and no tail", last)
	}
}

func TestServiceLogDownloadPastItsSizeLeavesOutTheOldestLinesAndSaysSoFirst(t *testing.T) {
	// A line is held as its 30-character stamp and its text; three of them fit.
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), func(f *envFix) { f.serviceLogsMaxDownloadBytes = 3 * 32 })
	w := f.download(t, "web", "")
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	lines := strings.Split(strings.TrimSuffix(w.body.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], "oldest lines") {
		t.Fatalf("body =\n%s\nwant a notice about the oldest lines, then three lines", w.body.String())
	}
	want := []string{dlLine("web.1@mgr-1", 40, "a2"), dlLine("web.2@wrk-ready", 50, "b2"), dlLine("web.1@mgr-1", 70, "a3")}
	if !slices.Equal(lines[1:], want) {
		t.Errorf("lines = %v, want the newest three in time order %v", lines[1:], want)
	}
}

func TestServiceLogDownloadUnderItsSizeHasNoNotice(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	if body := f.download(t, "web", "").body.String(); strings.Contains(body, "oldest lines") {
		t.Errorf("a download that fits has a notice:\n%s", body)
	}
}

func TestServiceLogDownloadAllowsTwoAtOnceForAUserAndRefusesTheThirdWith429(t *testing.T) {
	text := strings.Repeat("y", 60<<10)
	lines := []dockertest.FakeServiceLogLine{svcLine("tk-1", "node-1", 1, text), svcLine("tk-1", "node-1", 2, text)}
	f := newSvcLogsFix(t, svcTasks(), lines, nil)

	gate := make(chan struct{})
	var dones []<-chan struct{}
	for range 2 {
		w := newDLWriter(false)
		w.gate = gate
		dones = append(dones, f.startDownload("web", "", w))
		select {
		case <-w.wrote:
		case <-time.After(10 * time.Second):
			t.Fatal("a download never started writing")
		}
	}
	third := newDLWriter(true)
	select {
	case <-f.startDownload("web", "", third):
	case <-time.After(10 * time.Second):
		t.Fatal("the third download was not answered")
	}
	if third.code() != http.StatusTooManyRequests || !strings.Contains(third.body.String(), `"type":"stream_busy"`) {
		t.Fatalf("the third download = %d %s, want 429 stream_busy", third.code(), third.body.String())
	}
	if n := len(f.downloadRows(t)); n != 2 {
		t.Errorf("%d audit rows, want 2: a refused download is not a download", n)
	}
	close(gate)
	for _, d := range dones {
		<-d
	}
	if w := f.download(t, "web", ""); w.code() != http.StatusOK {
		t.Errorf("a download after the others finished = %d, want 200", w.code())
	}
}

func TestServiceLogDownloadAllowsOnlyTheServersShareAtOnceWhoeverAsks(t *testing.T) {
	text := strings.Repeat("y", 60<<10)
	lines := []dockertest.FakeServiceLogLine{svcLine("tk-1", "node-1", 1, text), svcLine("tk-1", "node-1", 2, text)}
	f := newSvcLogsFix(t, svcTasks(), lines, func(f *envFix) { f.serviceLogsMaxDownloads = 1 })

	gate := make(chan struct{})
	first := newDLWriter(false)
	first.gate = gate
	done := f.startDownload("web", "", first)
	select {
	case <-first.wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the first download never started writing")
	}
	second := newDLWriter(true)
	select {
	case <-f.startDownload("web", "", second):
	case <-time.After(10 * time.Second):
		t.Fatal("the second download was not answered")
	}
	if second.code() != http.StatusTooManyRequests || !strings.Contains(second.body.String(), `"type":"stream_busy"`) {
		t.Fatalf("the second download = %d %s, want 429 stream_busy from the server's limit", second.code(), second.body.String())
	}
	close(gate)
	<-done
	// The slot is given back, and the user's own slot with it.
	if w := f.download(t, "web", ""); w.code() != http.StatusOK {
		t.Errorf("a download after the first finished = %d, want 200", w.code())
	}
}

func TestServiceLogDownloadIsAuditedOncePerDownloadWithItsRangeAndNeverALineOfTheLog(t *testing.T) {
	const secret = "DOWNLOADED-LINE-THAT-MUST-NOT-BE-KEPT-77c1" //nolint:gosec // a marker, not a credential
	lines := append(svcBurstLines(), svcLine("tk-1", "node-1", 80, secret))
	f := newSvcLogsFix(t, svcTasks(), lines, nil)

	f.download(t, "web", "since=2025-01-01T00:00:00Z&until=2027-01-01T00:00:00Z")
	f.download(t, "web", "")
	rows := f.downloadRows(t)
	if len(rows) != 2 {
		t.Fatalf("%d rows for two downloads: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.TargetType != "service" || r.TargetID != "svc-web" || r.Env != f.env.ID || r.Result != "success" {
		t.Errorf("row = %+v, want the service as the target", r)
	}
	d := r.Details
	if d["service"] != "web" || d["since"] != "2025-01-01T00:00:00Z" || d["until"] != "2027-01-01T00:00:00Z" {
		t.Errorf("details = %v, want service, since and until", d)
	}
	d = rows[1].Details
	if _, has := d["since"]; has || d["until"] == "" {
		t.Errorf("details of a download with no since = %v, want an until (now) and no since", d)
	}
	for _, row := range rows {
		for k, v := range row.Details {
			if strings.Contains(v, secret) {
				t.Errorf("detail %s holds a line of the log", k)
			}
		}
	}
}

func TestServiceLogDownloadRefusalsAreAnsweredBeforeAnythingIsSentAndAreNotAudited(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	for _, query := range []string{"since=yesterday", "until=nope", "since=2026-10-14T10:00:01Z&until=2026-10-14T10:00:00Z"} {
		w := f.download(t, "web", query)
		if w.code() != http.StatusUnprocessableEntity || w.body.Len() == 0 || strings.Contains(w.body.String(), "web.1@") {
			t.Errorf("%q: status = %d, body %q, want a 422 problem", query, w.code(), w.body.String())
		}
	}
	w := f.download(t, "nope", "")
	if w.code() != http.StatusNotFound {
		t.Errorf("unknown service: status = %d, want 404: %s", w.code(), w.body.String())
	}
	if n := len(f.downloadRows(t)); n != 0 {
		t.Errorf("%d audit rows for refused downloads", n)
	}
}

func TestServiceLogDownloadNeedsServicesRead(t *testing.T) {
	f := newSvcLogsFix(t, svcTasks(), svcBurstLines(), nil)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: f.downloadPath("web", ""), cookie: cookie})
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want a refusal", rec.Code)
	}
}
