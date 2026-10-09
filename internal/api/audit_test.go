// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
)

// The audit log endpoints (plan 24, AUTH-09, AUTH-08, D-39, D-43, D-49,
// Pitfall 19). Negative cases are mandatory (D-81).

type auditListResp struct {
	Items []struct {
		ID, Time, ActorType, Actor, Action, TargetType, Target, Result, Reason, IP, UserAgent string
		Seq                                                                                   int64
		ActorDeleted                                                                          bool
		Environment                                                                           *string
		Details                                                                               map[string]string
	} `json:"items"`
	NextCursor    *int64 `json:"nextCursor"`
	RetentionDays int    `json:"retentionDays"`
}

// auditFix is an admin session over a store with some history.
type auditFix struct {
	*authFixture
	cookie, csrf string
}

func newAuditFix(t *testing.T) *auditFix {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	f.auditRetention = 90
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	return &auditFix{authFixture: f, cookie: cookie, csrf: csrf}
}

// log appends one entry at the given time.
func (f *auditFix) log(t *testing.T, at time.Time, actor, action, ua string) {
	t.Helper()
	// A writer of its own, so the session clock does not jump and expire the
	// admin's session.
	w := audit.NewWriter(f.st, f.v, slog.New(slog.DiscardHandler), func() time.Time { return at })
	err := w.Record(context.Background(), audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "id-" + actor, Name: actor},
		Action: action, TargetType: "session", TargetID: "s-1", EnvID: "env-1",
		Result: audit.ResultSuccess, Reason: "r", IP: "198.51.100.7", UserAgent: ua,
		Details: map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *auditFix) get(t *testing.T, path, cookie string) *authRecorder {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: f.csrf})
	return &authRecorder{rec.Code, rec.Body.String(), rec.Header()}
}

type authRecorder struct {
	code   int
	body   string
	header http.Header
}

func (r *authRecorder) list(t *testing.T) auditListResp {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("status = %d: %s", r.code, r.body)
	}
	var out auditListResp
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return out
}

func TestAuditListPagesNewestFirstWithACursorAndTheRetentionSetting(t *testing.T) {
	f := newAuditFix(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for range 120 {
		f.log(t, base, "alice", "auth.login", "ua") // one identical timestamp
	}
	var sizes []int
	var seen []int64
	path := "/api/audit"
	for range 5 {
		out := f.get(t, path, f.cookie).list(t)
		if out.RetentionDays != 90 {
			t.Errorf("retentionDays = %d, want 90", out.RetentionDays)
		}
		sizes = append(sizes, len(out.Items))
		for _, it := range out.Items {
			seen = append(seen, it.Seq)
		}
		if out.NextCursor == nil {
			break
		}
		path = fmt.Sprintf("/api/audit?cursor=%d", *out.NextCursor)
	}
	// 120 entries plus the admin's own sign-in.
	if fmt.Sprint(sizes) != "[50 50 21]" {
		t.Fatalf("page sizes = %v, want [50 50 21]", sizes)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]-1 {
			t.Fatalf("seq %d follows %d: paging repeated or skipped an entry", seen[i], seen[i-1])
		}
	}
}

func TestAuditListEntriesCarryWhoWhatWhereWhenAndNothingSecret(t *testing.T) {
	f := newAuditFix(t)
	f.log(t, time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC), "alice", "settings.updated", "Mozilla/5.0")
	rec := f.get(t, "/api/audit?user=alice", f.cookie)
	out := rec.list(t)
	if len(out.Items) != 1 {
		t.Fatalf("items = %d", len(out.Items))
	}
	it := out.Items[0]
	if it.ActorType != "user" || it.Actor != "alice" || it.Action != "settings.updated" || it.TargetType != "session" ||
		it.Target != "s-1" || it.Environment == nil || *it.Environment != "env-1" || it.Result != "success" ||
		it.Reason != "r" || it.IP != "198.51.100.7" || it.UserAgent != "Mozilla/5.0" || it.Details["k"] != "v" || it.ID == "" {
		t.Errorf("entry = %+v", it)
	}
	if p, err := time.Parse(time.RFC3339, it.Time); err != nil || p.Location() != time.UTC || p.Hour() != 8 {
		t.Errorf("time %q is not RFC 3339 UTC", it.Time)
	}
	lower := strings.ToLower(rec.body)
	for _, banned := range []string{"hmac", "key_id", "keyid", "prev"} {
		if strings.Contains(lower, banned) {
			t.Errorf("response mentions %q", banned)
		}
	}
	// An entry that is not about an environment says null, not an empty string.
	out = f.get(t, "/api/audit?action=auth.login", f.cookie).list(t)
	if len(out.Items) != 1 || out.Items[0].Environment != nil {
		t.Errorf("sign-in entry environment = %+v", out.Items)
	}
}

func TestAuditListFiltersByUserActionsAndInclusiveTimeRange(t *testing.T) {
	f := newAuditFix(t)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i, s := range []struct{ actor, action string }{
		{"carol", "auth.login"}, {"dave", "auth.login"}, {"carol", "auth.logout"},
		{"carol", "settings.updated"}, {"dave", "auth.logout"},
	} {
		f.log(t, base.Add(time.Duration(i)*time.Minute), s.actor, s.action, "ua")
	}
	count := func(q string) int {
		t.Helper()
		return len(f.get(t, "/api/audit?"+q, f.cookie).list(t).Items)
	}
	if n := count("user=carol"); n != 3 {
		t.Errorf("user=carol = %d, want 3", n)
	}
	if n := count("user=carol&action=auth.login&action=auth.logout"); n != 2 {
		t.Errorf("carol login and logout = %d, want 2", n)
	}
	from := url.QueryEscape(base.Add(time.Minute).Format(time.RFC3339))
	to := url.QueryEscape(base.Add(3 * time.Minute).Format(time.RFC3339))
	if n := count("user=carol&from=" + from + "&to=" + to); n != 2 {
		t.Errorf("carol minutes 1 to 3 (inclusive) = %d, want 2", n)
	}
}

func TestAuditListClampsTheLimitAndRejectsAMalformedTimeWith422(t *testing.T) {
	f := newAuditFix(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for range 130 {
		f.log(t, base, "alice", "auth.login", "ua")
	}
	if n := len(f.get(t, "/api/audit?limit=1000", f.cookie).list(t).Items); n != 100 {
		t.Errorf("limit=1000 returned %d items, want 100", n)
	}
	if n := len(f.get(t, "/api/audit?limit=5", f.cookie).list(t).Items); n != 5 {
		t.Errorf("limit=5 returned %d items", n)
	}
	for _, q := range []string{"from=yesterday", "to=2026-13-45T00:00:00Z", "from=2026-03-01"} {
		if rec := f.get(t, "/api/audit?"+q, f.cookie); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", q, rec.code, rec.body)
		}
	}
	if rec := f.get(t, "/api/audit?cursor=-1", f.cookie); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("negative cursor: status = %d, want 422", rec.code)
	}
}

func TestAuditActorsListsTheDistinctNamesSeen(t *testing.T) {
	f := newAuditFix(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, a := range []string{"zed", "bob", "zed", "bob"} {
		f.log(t, base, a, "auth.login", "ua")
	}
	rec := f.get(t, "/api/audit/actors", f.cookie)
	if names, ok := actorsOf(rec); !ok || fmt.Sprint(names) != "[{admin false} {bob false} {zed false}]" {
		t.Errorf("actors = %d %s", rec.code, rec.body)
	}
}

func TestAuditExportCSVIsADownloadThatCannotInjectFormulas(t *testing.T) {
	f := newAuditFix(t)
	// Eight hours after sign-in: inside the 12 hour idle limit.
	at := time.Date(2026, 1, 1, 8, 4, 5, 0, time.UTC)
	f.clk.t = at
	f.log(t, at, "alice", "auth.login", "=HYPERLINK(\"http://evil\")")
	f.log(t, at, "alice", "auth.logout", "plain, with comma")
	rec := f.get(t, "/api/audit/export?format=csv&user=alice", f.cookie)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if ct := rec.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.header.Get("Content-Disposition"); cd != `attachment; filename="bayplan-audit-20260101T080405Z.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	recs, err := csv.NewReader(strings.NewReader(rec.body)).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("csv: %d rows, %v\n%s", len(recs), err, rec.body)
	}
	if recs[2][11] != `'=HYPERLINK("http://evil")` || recs[1][11] != "plain, with comma" {
		t.Errorf("user agents = %q, %q", recs[2][11], recs[1][11])
	}
	// The default format is CSV.
	if rec := f.get(t, "/api/audit/export", f.cookie); !strings.HasPrefix(rec.header.Get("Content-Type"), "text/csv") {
		t.Errorf("default Content-Type = %q", rec.header.Get("Content-Type"))
	}
}

func TestAuditExportJSONStreamsTheFilteredEntriesAndRefusesOtherFormats(t *testing.T) {
	f := newAuditFix(t)
	// Eight hours after sign-in: inside the 12 hour idle limit.
	at := time.Date(2026, 1, 1, 8, 4, 5, 0, time.UTC)
	f.clk.t = at
	f.log(t, at, "alice", "auth.login", "ua")
	f.log(t, at, "bob", "auth.login", "ua")
	rec := f.get(t, "/api/audit/export?format=json&user=alice", f.cookie)
	if rec.code != http.StatusOK || rec.header.Get("Content-Type") != "application/json" ||
		rec.header.Get("Content-Disposition") != `attachment; filename="bayplan-audit-20260101T080405Z.json"` {
		t.Fatalf("status %d, headers %v", rec.code, rec.header)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(rec.body), &got); err != nil || len(got) != 1 || got[0]["actor"] != "alice" {
		t.Errorf("export = %s (%v)", rec.body, err)
	}
	if _, ok := got[0]["hmac"]; ok {
		t.Error("export carries the chain")
	}
	if rec := f.get(t, "/api/audit/export?format=xml", f.cookie); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("format=xml: status = %d, want 422", rec.code)
	}
	if rec := f.get(t, "/api/audit/export?format=csv&from=nope", f.cookie); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("malformed from on export: status = %d, want 422", rec.code)
	}
}

func TestAuditEndpointsRefuseAUserWithoutAuditReadAndAuditTheDenial(t *testing.T) {
	f := newAuditFix(t)
	f.addUser(t, "viewer", false)
	cookie, _ := f.signIn(t, "viewer", false)
	f.log(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "alice", "auth.login", "secret-looking-ua")
	for _, path := range []string{"/api/audit", "/api/audit/actors", "/api/audit/export?format=csv"} {
		rec := f.get(t, path, cookie)
		if rec.code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.code)
		}
		if strings.Contains(rec.body, "secret-looking-ua") || strings.Contains(rec.body, "alice") {
			t.Errorf("%s: a refused response leaked entries: %s", path, rec.body)
		}
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 3 {
		t.Fatalf("permission.denied entries = %d, want 3: %+v", len(denied), denied)
	}
	for _, d := range denied {
		if d.ActorID != "id-viewer" || d.Result != "denied" {
			t.Errorf("denial = %+v", d)
		}
	}
}

func TestAuditEndpointsRefuseAnAnonymousCaller(t *testing.T) {
	f := newAuditFix(t)
	for _, path := range []string{"/api/audit", "/api/audit/actors", "/api/audit/export"} {
		if rec := f.get(t, path, ""); rec.code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, rec.code)
		}
	}
}

func TestAuditReadingAndExportingAreNotAudited(t *testing.T) {
	f := newAuditFix(t)
	before := len(f.audits(t, audit.ActionPermissionDenied)) + len(f.audits(t, "audit.read")) + len(f.audits(t, "audit.exported"))
	f.get(t, "/api/audit", f.cookie)
	f.get(t, "/api/audit/export?format=json", f.cookie)
	after := len(f.audits(t, audit.ActionPermissionDenied)) + len(f.audits(t, "audit.read")) + len(f.audits(t, "audit.exported"))
	if before != after {
		t.Errorf("allowed reads wrote %d entries (D-49)", after-before)
	}
}

// logIn appends one entry on an environment ("" for an instance-level event)
// with a chosen result, for the scoped-read tests.
func (f *auditFix) logIn(t *testing.T, at time.Time, actor, action, envID string, result audit.Result) {
	t.Helper()
	w := audit.NewWriter(f.st, f.v, slog.New(slog.DiscardHandler), func() time.Time { return at })
	err := w.Record(context.Background(), audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "id-" + actor, Name: actor},
		Action: action, TargetType: "route", TargetID: "t-" + action, EnvID: envID,
		Result: result, Reason: "r", IP: "198.51.100.7", UserAgent: "ua",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// auditActor is one name of GET /api/audit/actors.
type auditActor struct {
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

// actorsOf decodes the actor list of a successful answer.
func actorsOf(r *authRecorder) ([]auditActor, bool) {
	var out []auditActor
	if r.code != http.StatusOK || json.Unmarshal([]byte(r.body), &out) != nil {
		return nil, false
	}
	return out, true
}

// scopeFix is the audit log of two environments and instance-level events, read
// by people with different access (D-09).
type scopeFix struct{ *auditFix }

const (
	scopeEnvA = "env-a"
	scopeEnvB = "env-b"
)

func newScopeFix(t *testing.T) *scopeFix {
	t.Helper()
	f := &scopeFix{newAuditFix(t)}
	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	f.logIn(t, at, "bob", "auth.login", "", audit.ResultSuccess)
	f.logIn(t, at, "dana", "user.created", "", audit.ResultSuccess)
	f.logIn(t, at, "alice", "environment.renamed", scopeEnvA, audit.ResultSuccess)
	f.logIn(t, at, "vera", "permission.denied", scopeEnvA, audit.ResultDenied)
	f.logIn(t, at, "erin", "access.granted", scopeEnvB, audit.ResultSuccess)
	f.addEnvUser(t, "carol", scopeEnvA, "admin")
	f.addEnvUser(t, "oscar", scopeEnvA, "operator")
	f.addEnvUser(t, "vince", scopeEnvA, "viewer")
	return f
}

// as signs the user in and reads path.
func (f *scopeFix) as(t *testing.T, user, path string) *authRecorder {
	t.Helper()
	cookie, csrf := f.signIn(t, user, false)
	rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf})
	return &authRecorder{rec.Code, rec.Body.String(), rec.Header()}
}

// actions lists the action keys of a page, newest first, leaving out the
// sign-ins the test itself causes.
func (r auditListResp) actions(skip string) []string {
	var out []string
	for _, it := range r.Items {
		if it.Action != skip {
			out = append(out, it.Action)
		}
	}
	return out
}

func TestAuditScopeAnEnvAdminSeesTheirEnvironmentsEntriesAndNoInstanceLevelEvents(t *testing.T) {
	f := newScopeFix(t)

	carol := f.as(t, "carol", "/api/audit").list(t)
	if got := fmt.Sprint(carol.actions("")); got != "[permission.denied environment.renamed]" {
		t.Errorf("an env Admin of A reads %s, want the refusal and the rename on A and nothing else", got)
	}
	for _, it := range carol.Items {
		if it.Environment == nil || *it.Environment != scopeEnvA {
			t.Errorf("entry %s has environment %v, want only %s", it.Action, it.Environment, scopeEnvA)
		}
	}

	admin := f.get(t, "/api/audit", f.cookie).list(t)
	have := map[string]bool{}
	for _, a := range admin.actions("") {
		have[a] = true
	}
	for _, want := range []string{"auth.login", "user.created", "environment.renamed", "permission.denied", "access.granted"} {
		if !have[want] {
			t.Errorf("an instance admin does not see %s", want)
		}
	}
}

func TestAuditScopeActorsAndBothExportsHoldOnlyTheScopedEntries(t *testing.T) {
	f := newScopeFix(t)

	rec := f.as(t, "carol", "/api/audit/actors")
	if names, ok := actorsOf(rec); !ok || fmt.Sprint(names) != "[{alice false} {vera false}]" {
		t.Errorf("actors of an env Admin of A = %d %s, want alice and vera", rec.code, rec.body)
	}

	rec = f.as(t, "carol", "/api/audit/export?format=json")
	var rows []map[string]any
	if rec.code != http.StatusOK || json.Unmarshal([]byte(rec.body), &rows) != nil || len(rows) != 2 {
		t.Fatalf("json export = %d %s, want the 2 entries on A", rec.code, rec.body)
	}
	for _, r := range rows {
		if r["environment"] != scopeEnvA {
			t.Errorf("json export holds %v, want only entries on %s", r, scopeEnvA)
		}
	}

	rec = f.as(t, "carol", "/api/audit/export?format=csv")
	recs, err := csv.NewReader(strings.NewReader(rec.body)).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("csv export = %d rows, %v\n%s, want a header and the 2 entries on A", len(recs), err, rec.body)
	}
	for _, row := range recs[1:] {
		if row[7] != scopeEnvA {
			t.Errorf("csv export holds %v, want only entries on %s", row, scopeEnvA)
		}
	}
}

func TestAuditScopeAnOperatorAndAViewerAreRefusedAndTheRefusalIsAudited(t *testing.T) {
	f := newScopeFix(t)
	for _, tc := range []struct{ user, path string }{
		{"oscar", "/api/audit"}, {"vince", "/api/audit/actors"}, {"oscar", "/api/audit/export?format=csv"},
	} {
		before := len(f.audits(t, audit.ActionPermissionDenied))
		rec := f.as(t, tc.user, tc.path)
		if rec.code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403: %s", tc.user, tc.path, rec.code, rec.body)
		}
		for _, leak := range []string{"alice", "environment.renamed", "bob"} {
			if strings.Contains(rec.body, leak) {
				t.Errorf("%s %s: the refusal leaked %q: %s", tc.user, tc.path, leak, rec.body)
			}
		}
		denied := f.audits(t, audit.ActionPermissionDenied)
		if len(denied) != before+1 {
			t.Fatalf("%s %s: permission.denied entries went from %d to %d, want one more", tc.user, tc.path, before, len(denied))
		}
		last := denied[len(denied)-1]
		if last.ActorID != "id-"+tc.user || last.Result != "denied" || last.Reason != "no_audit_scope" ||
			!strings.HasPrefix(last.TargetID, "GET /api/audit") {
			t.Errorf("%s %s: refusal entry = %+v, want reason no_audit_scope on the route", tc.user, tc.path, last)
		}
	}
}

func TestAuditScopeCursorsPageExactlyTheScopedEntriesOnceEachNewestFirst(t *testing.T) {
	f := newAuditFix(t)
	f.addEnvUser(t, "carol", scopeEnvA, "admin")
	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	for i := range 120 {
		f.logIn(t, at, "alice", fmt.Sprintf("on.a%d", i), scopeEnvA, audit.ResultSuccess)
		f.logIn(t, at, "bob", fmt.Sprintf("on.b%d", i), scopeEnvB, audit.ResultSuccess)
	}
	cookie, csrf := f.signIn(t, "carol", false)
	var sizes []int
	var seen []int64
	path := "/api/audit"
	for range 5 {
		rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf})
		out := (&authRecorder{rec.Code, rec.Body.String(), rec.Header()}).list(t)
		sizes = append(sizes, len(out.Items))
		for _, it := range out.Items {
			if it.Environment == nil || *it.Environment != scopeEnvA {
				t.Fatalf("a page holds %s on %v", it.Action, it.Environment)
			}
			seen = append(seen, it.Seq)
		}
		if out.NextCursor == nil {
			break
		}
		path = fmt.Sprintf("/api/audit?cursor=%d", *out.NextCursor)
	}
	if fmt.Sprint(sizes) != "[50 50 20]" {
		t.Fatalf("page sizes = %v, want [50 50 20]: the 120 entries on A, every page full", sizes)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("seq %d follows %d: not newest first", seen[i], seen[i-1])
		}
	}
}

func TestAuditScopeAnAdminOfEveryEnvironmentSeesEveryEntryWithAnEnvironmentAndNoOther(t *testing.T) {
	f := newScopeFix(t)
	f.addAllEnvAdmin(t, "gail")
	got := f.as(t, "gail", "/api/audit").list(t)
	if a := fmt.Sprint(got.actions("")); a != "[access.granted permission.denied environment.renamed]" {
		t.Errorf("an Admin of every environment reads %s, want the three entries that have an environment, newest first", a)
	}
	var names []auditActor
	rec := f.as(t, "gail", "/api/audit/actors")
	if names, _ = actorsOf(rec); fmt.Sprint(names) != "[{alice false} {erin false} {vera false}]" {
		t.Errorf("actors of an Admin of every environment = %s", rec.body)
	}
}

func TestAuditEntriesOfADeletedUserReadAsDeletedAndTheSignedRowsAreUntouched(t *testing.T) {
	f := newAuditFix(t)
	f.addUser(t, "alice", false)
	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	f.logIn(t, at, "alice", "environment.renamed", scopeEnvA, audit.ResultSuccess)
	f.logIn(t, at, "bob", "environment.renamed", scopeEnvA, audit.ResultSuccess)
	chain := func() string {
		var s string
		err := f.st.Read.QueryRow(`SELECT group_concat(hex(hmac) || actor_name || actor_id, '|') FROM (SELECT * FROM audit_log ORDER BY seq)`).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := chain()
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = 5 WHERE id = 'id-alice'`); err != nil {
		t.Fatal(err)
	}

	for _, it := range f.get(t, "/api/audit?action=environment.renamed", f.cookie).list(t).Items {
		if want := it.Actor == "alice"; it.ActorDeleted != want {
			t.Errorf("%s by %s: actorDeleted = %v, want %v", it.Action, it.Actor, it.ActorDeleted, want)
		}
	}
	if names, ok := actorsOf(f.get(t, "/api/audit/actors", f.cookie)); !ok || !slices.Contains(names, auditActor{"alice", true}) ||
		!slices.Contains(names, auditActor{"bob", false}) {
		t.Errorf("actors = %v, want alice deleted and bob not", names)
	}

	var rows []map[string]any
	if rec := f.get(t, "/api/audit/export?format=json&user=alice", f.cookie); json.Unmarshal([]byte(rec.body), &rows) != nil ||
		len(rows) != 1 || rows[0]["actorDeleted"] != true {
		t.Errorf("json export = %s, want actorDeleted true", rec.body)
	}
	rec := f.get(t, "/api/audit/export?format=csv&user=alice", f.cookie)
	recs, err := csv.NewReader(strings.NewReader(rec.body)).ReadAll()
	if err != nil || len(recs) != 2 || recs[0][len(recs[0])-1] != "actor_deleted" || recs[1][len(recs[1])-1] != "true" {
		t.Errorf("csv export = %v (%v), want actor_deleted true", recs, err)
	}

	if chain() != before {
		t.Error("a delete rewrote a signed row")
	}
	if res, err := audit.Verify(context.Background(), f.st, f.v); err != nil || res.Broken != nil {
		t.Errorf("verify after a delete = %+v, %v", res, err)
	}
}

const (
	bulkOne = "018f0000-0000-7000-8000-000000000001"
	bulkTwo = "018f0000-0000-7000-8000-000000000002"
)

// logBulk appends one container entry on an environment whose details carry a
// bulk ID ("" for none), like the rows of 04-05.
func (f *auditFix) logBulk(t *testing.T, at time.Time, actor, target, envID, bulk string) {
	t.Helper()
	w := audit.NewWriter(f.st, f.v, slog.New(slog.DiscardHandler), func() time.Time { return at })
	details := map[string]string{"k": "v"}
	if bulk != "" {
		details["bulk"] = bulk
	}
	err := w.Record(context.Background(), audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorUser, ID: "id-" + actor, Name: actor},
		Action: "container.stopped", TargetType: "container", TargetID: target, EnvID: envID,
		Result: audit.ResultSuccess, IP: "198.51.100.7", UserAgent: "ua", Details: details,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// newBulkFix holds three rows of bulk one (two on env-a, one on env-b), two of
// bulk two and one with no bulk, all on env-a unless said.
func newBulkFix(t *testing.T) *scopeFix {
	t.Helper()
	f := newScopeFix(t)
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	f.logBulk(t, at, "alice", "one-a1", scopeEnvA, bulkOne)
	f.logBulk(t, at, "alice", "two-a1", scopeEnvA, bulkTwo)
	f.logBulk(t, at, "alice", "one-a2", scopeEnvA, bulkOne)
	f.logBulk(t, at, "alice", "none-a", scopeEnvA, "")
	f.logBulk(t, at, "erin", "one-b", scopeEnvB, bulkOne)
	f.logBulk(t, at, "alice", "two-a2", scopeEnvA, bulkTwo)
	return f
}

func targetsOf(r auditListResp) []string {
	var out []string
	for _, it := range r.Items {
		out = append(out, it.Target)
	}
	return out
}

func TestAuditBulkListReturnsOnlyThatOperationsRowsNewestFirst(t *testing.T) {
	f := newBulkFix(t)
	got := f.get(t, "/api/audit?bulk="+bulkOne, f.cookie).list(t)
	if fmt.Sprint(targetsOf(got)) != "[one-b one-a2 one-a1]" {
		t.Errorf("bulk one = %v, want its three rows newest first", targetsOf(got))
	}
	for _, it := range got.Items {
		if it.Details["bulk"] != bulkOne {
			t.Errorf("row %s carries bulk %q", it.Target, it.Details["bulk"])
		}
	}
	two := f.get(t, "/api/audit?bulk="+bulkTwo, f.cookie).list(t)
	if fmt.Sprint(targetsOf(two)) != "[two-a2 two-a1]" {
		t.Errorf("bulk two = %v", targetsOf(two))
	}
	unknown := f.get(t, "/api/audit?bulk=018f0000-0000-7000-8000-0000000000ff", f.cookie).list(t)
	if len(unknown.Items) != 0 {
		t.Errorf("an unknown bulk returned %v", targetsOf(unknown))
	}
	all := f.get(t, "/api/audit", f.cookie).list(t)
	if len(all.Items) < 6 {
		t.Errorf("no bulk filter returned %d rows, want the bulk and non-bulk rows alike", len(all.Items))
	}
}

func TestAuditBulkExportCSVAndJSONHoldOnlyThatOperationsRows(t *testing.T) {
	f := newBulkFix(t)
	rec := f.get(t, "/api/audit/export?format=csv&bulk="+bulkOne, f.cookie)
	if rec.code != http.StatusOK {
		t.Fatalf("csv export: status %d: %s", rec.code, rec.body)
	}
	rows, err := csv.NewReader(strings.NewReader(rec.body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, r := range rows[1:] {
		targets = append(targets, r[6])
	}
	if fmt.Sprint(targets) != "[one-b one-a2 one-a1]" {
		t.Errorf("csv export of bulk one = %v, want its three rows newest first", targets)
	}

	rec = f.get(t, "/api/audit/export?format=json&bulk="+bulkTwo, f.cookie)
	var got []struct {
		Target  string            `json:"target"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal([]byte(rec.body), &got); err != nil {
		t.Fatalf("json export: %v (%s)", err, rec.body)
	}
	if len(got) != 2 || got[0].Target != "two-a2" || got[1].Target != "two-a1" {
		t.Errorf("json export of bulk two = %+v, want its two rows", got)
	}
	for _, g := range got {
		if g.Details["bulk"] != bulkTwo {
			t.Errorf("json export holds a row of bulk %q", g.Details["bulk"])
		}
	}
}

func TestAuditBulkIsAndedWithTheEnvAdminScope(t *testing.T) {
	f := newBulkFix(t)
	const bulkOnB = "018f0000-0000-7000-8000-0000000000b3"
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	f.logBulk(t, at, "erin", "only-b1", scopeEnvB, bulkOnB)
	f.logBulk(t, at, "erin", "only-b2", scopeEnvB, bulkOnB)

	// carol administers A: a bulk whose rows are all on B reads as empty.
	if got := f.as(t, "carol", "/api/audit?bulk="+bulkOnB).list(t); len(got.Items) != 0 {
		t.Errorf("an env Admin of A read %v of a bulk on B", targetsOf(got))
	}
	// A bulk spread over A and B shows her only the rows on A.
	if got := f.as(t, "carol", "/api/audit?bulk="+bulkOne).list(t); fmt.Sprint(targetsOf(got)) != "[one-a2 one-a1]" {
		t.Errorf("an env Admin of A read %v of bulk one, want its two rows on A", targetsOf(got))
	}
	// The exports are scoped the same way.
	for _, format := range []string{"csv", "json"} {
		rec := f.as(t, "carol", "/api/audit/export?format="+format+"&bulk="+bulkOnB)
		if rec.code != http.StatusOK || strings.Contains(rec.body, "only-b") {
			t.Errorf("%s export: status %d, body leaks a row of B: %q", format, rec.code, rec.body)
		}
	}
	// An instance admin reads them all.
	if got := f.get(t, "/api/audit?bulk="+bulkOnB, f.cookie).list(t); fmt.Sprint(targetsOf(got)) != "[only-b2 only-b1]" {
		t.Errorf("an instance admin read %v of the bulk on B", targetsOf(got))
	}
}

func TestAuditBulkThatIsNotAUUIDIsRefused422OnTheListAndTheExport(t *testing.T) {
	f := newBulkFix(t)
	for _, bad := range []string{"not-a-uuid", "018f0000", "x' OR '1'='1", "018f0000-0000-7000-8000-00000000000g"} {
		for _, path := range []string{"/api/audit?bulk=", "/api/audit/export?format=csv&bulk=", "/api/audit/export?format=json&bulk="} {
			if rec := f.get(t, path+url.QueryEscape(bad), f.cookie); rec.code != http.StatusUnprocessableEntity {
				t.Errorf("%s%s: status = %d, want 422", path, bad, rec.code)
			}
		}
	}
}
