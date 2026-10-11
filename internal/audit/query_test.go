// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package audit_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
)

// Reading the audit log: filters, deterministic paging, the actor list and
// the exports (plan 24, AUTH-09, AUTH-08, D-39, D-43, Pitfall 19).

// recordAs appends one entry by actor, action and user agent at the clock's time.
func (f *fixture) recordAs(t *testing.T, actor, action, ua string) {
	t.Helper()
	e := ent(action)
	e.Actor = audit.Actor{Type: audit.ActorUser, ID: "id-" + actor, Name: actor}
	e.UserAgent = ua
	if err := f.w.Record(context.Background(), e); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestQueryPagesBySequenceWhenEveryEntryShareATimestamp(t *testing.T) {
	f := newFixture(t)
	f.clk.Set(t0)
	for i := range 120 {
		f.recordAs(t, "alice", "test.action", fmt.Sprintf("ua-%d", i))
	}
	ctx := context.Background()
	var seen []int64
	var sizes []int
	var cursor int64
	for range 5 {
		rows, next, err := audit.Query(ctx, f.s, audit.Filter{}, cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(rows))
		for _, r := range rows {
			seen = append(seen, r.Seq)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if fmt.Sprint(sizes) != "[50 50 20]" {
		t.Fatalf("page sizes = %v, want [50 50 20]", sizes)
	}
	if len(seen) != 120 {
		t.Fatalf("saw %d entries, want 120", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]-1 {
			t.Fatalf("seq %d follows %d: paging skipped or repeated an entry", seen[i], seen[i-1])
		}
	}
}

func TestQueryAnExactMultipleOfTheLimitEndsWithoutAnEmptyNextPage(t *testing.T) {
	f := newFixture(t)
	f.record(t, 100)
	_, next, err := audit.Query(context.Background(), f.s, audit.Filter{}, 0, 50)
	if err != nil || next == 0 {
		t.Fatalf("first page next = %d, err = %v; want a cursor", next, err)
	}
	rows, next, err := audit.Query(context.Background(), f.s, audit.Filter{}, next, 50)
	if err != nil || len(rows) != 50 || next != 0 {
		t.Fatalf("second page: %d rows, next %d, err %v; want 50 rows and no cursor", len(rows), next, err)
	}
}

func TestQueryClampsTheLimit(t *testing.T) {
	f := newFixture(t)
	f.record(t, 130)
	ctx := context.Background()
	for _, tc := range []struct{ limit, want int }{{0, 50}, {-3, 50}, {7, 7}, {1000, 100}} {
		rows, _, err := audit.Query(ctx, f.s, audit.Filter{}, 0, tc.limit)
		if err != nil || len(rows) != tc.want {
			t.Errorf("limit %d: %d rows, err %v; want %d", tc.limit, len(rows), err, tc.want)
		}
	}
}

func TestQueryFiltersByActorActionsAndInclusiveTimeRange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	steps := []struct{ actor, action string }{
		{"admin", "auth.login"}, {"alice", "auth.login"}, {"admin", "auth.logout"},
		{"admin", "settings.updated"}, {"alice", "auth.logout"}, {"admin", "auth.login"},
	}
	for i, s := range steps {
		f.clk.Set(t0.Add(time.Duration(i) * time.Minute))
		f.recordAs(t, s.actor, s.action, "ua")
	}
	count := func(flt audit.Filter) int {
		t.Helper()
		rows, _, err := audit.Query(ctx, f.s, flt, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if n := count(audit.Filter{Actor: "admin"}); n != 4 {
		t.Errorf("actor admin = %d, want 4", n)
	}
	if n := count(audit.Filter{Actor: "adm"}); n != 0 {
		t.Errorf("actor adm = %d, want 0 (exact match)", n)
	}
	if n := count(audit.Filter{Actions: []string{"auth.login", "auth.logout"}}); n != 5 {
		t.Errorf("two actions = %d, want 5", n)
	}
	if n := count(audit.Filter{Actions: []string{"settings.updated"}}); n != 1 {
		t.Errorf("one action = %d, want 1", n)
	}
	if n := count(audit.Filter{Actions: []string{"nope"}}); n != 0 {
		t.Errorf("unknown action = %d, want 0", n)
	}
	// Entries sit at minutes 0..5; both bounds are inclusive.
	if n := count(audit.Filter{From: t0.Add(time.Minute), To: t0.Add(3 * time.Minute)}); n != 3 {
		t.Errorf("minutes 1 to 3 = %d, want 3", n)
	}
	if n := count(audit.Filter{From: t0.Add(4 * time.Minute)}); n != 2 {
		t.Errorf("from minute 4 = %d, want 2", n)
	}
	if n := count(audit.Filter{To: t0}); n != 1 {
		t.Errorf("to minute 0 = %d, want 1", n)
	}
	both := audit.Filter{Actor: "admin", Actions: []string{"auth.login"}, From: t0, To: t0.Add(time.Hour)}
	if n := count(both); n != 2 {
		t.Errorf("combined filters = %d, want 2", n)
	}
}

func TestQueryRowsCarryWhoWhatWhereWhenAndNoChainFields(t *testing.T) {
	f := newFixture(t)
	f.clk.Set(t0)
	f.recordAs(t, "alice", "auth.login", "Mozilla/5.0")
	rows, _, err := audit.Query(context.Background(), f.s, audit.Filter{}, 0, 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d, err %v", len(rows), err)
	}
	r := rows[0]
	if r.ActorType != "user" || r.ActorName != "alice" || r.Action != "auth.login" || r.TargetType != "session" ||
		r.TargetID != "s-1" || r.EnvID != "env-1" || r.Result != "success" || r.Reason != "because" ||
		r.IP != "203.0.113.9" || r.UserAgent != "Mozilla/5.0" || r.Details["k"] != "v" || r.ID == "" || r.Seq == 0 {
		t.Errorf("row = %+v", r)
	}
	if !r.Time.Equal(t0) || r.Time.Location() != time.UTC {
		t.Errorf("time = %v, want %v in UTC", r.Time, t0)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"hmac", "HMAC", "key", "Key", "prev"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("row JSON %s mentions %q", b, banned)
		}
	}
}

func TestActorsReturnsDistinctNamesSortedAndCappedAt200(t *testing.T) {
	f := newFixture(t)
	for _, a := range []string{"bob", "alice", "bob", "alice", "carol"} {
		f.recordAs(t, a, "auth.login", "ua")
	}
	got, err := audit.Actors(context.Background(), f.s, audit.Filter{})
	if err != nil || actorNames(got) != "[alice bob carol]" {
		t.Fatalf("actors = %v, err %v", got, err)
	}
	for i := range 250 {
		f.recordAs(t, fmt.Sprintf("user-%03d", i), "auth.login", "ua")
	}
	got, err = audit.Actors(context.Background(), f.s, audit.Filter{})
	if err != nil || len(got) != 200 {
		t.Fatalf("actors = %d, err %v; want 200", len(got), err)
	}
}

func exportCSV(t *testing.T, f *fixture, flt audit.Filter) [][]string {
	t.Helper()
	var buf bytes.Buffer
	if err := audit.ExportCSV(context.Background(), f.s, flt, &buf); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	return recs
}

func TestExportCSVWritesAHeaderThenOneRowPerEntryInEveryBatch(t *testing.T) {
	f := newFixture(t)
	f.record(t, 1230) // three batches of 500
	recs := exportCSV(t, f, audit.Filter{})
	if len(recs) != 1231 {
		t.Fatalf("rows = %d, want 1231", len(recs))
	}
	want := "id,time,actor_type,actor,action,target_type,target,environment,result,reason,ip,user_agent,details,actor_deleted"
	if got := strings.Join(recs[0], ","); got != want {
		t.Errorf("header = %s", got)
	}
	seen := map[string]bool{}
	for _, r := range recs[1:] {
		if len(r) != 14 || seen[r[0]] {
			t.Fatalf("bad or repeated row %v", r)
		}
		seen[r[0]] = true
	}
	first := recs[1]
	if first[4] != "test.action1229" {
		t.Errorf("first data row is %s; the newest entry comes first", first[4])
	}
	if _, err := time.Parse(time.RFC3339Nano, first[1]); err != nil || !strings.HasSuffix(first[1], "Z") {
		t.Errorf("time %q is not RFC 3339 UTC", first[1])
	}
	if first[12] != `{"k":"v"}` {
		t.Errorf("details = %q", first[12])
	}
}

func TestExportCSVHonoursTheSameFiltersAsQuery(t *testing.T) {
	f := newFixture(t)
	f.recordAs(t, "alice", "auth.login", "ua")
	f.recordAs(t, "bob", "auth.login", "ua")
	f.recordAs(t, "alice", "auth.logout", "ua")
	recs := exportCSV(t, f, audit.Filter{Actor: "alice", Actions: []string{"auth.logout"}})
	if len(recs) != 2 || recs[1][3] != "alice" || recs[1][4] != "auth.logout" {
		t.Errorf("filtered export = %v", recs)
	}
	if recs := exportCSV(t, f, audit.Filter{Actor: "nobody"}); len(recs) != 1 {
		t.Errorf("an export matching nothing is just the header, got %d rows", len(recs))
	}
}

func TestExportCSVNeutralisesFormulaInjectionInEveryCell(t *testing.T) {
	f := newFixture(t)
	for _, ua := range []string{"=HYPERLINK(1)", "+1+1", "-2+3", "@SUM(A1)", "TABCMD", "CRCMD", "safe"} {
		f.recordAs(t, "alice", "auth.login", ua)
	}
	// The writer replaces control characters, so a leading tab or carriage
	// return can only come from a row written by something else.
	f.exec(t, `UPDATE audit_log SET user_agent = char(9) || 'cmd' WHERE user_agent = 'TABCMD'`)
	f.exec(t, `UPDATE audit_log SET user_agent = char(13) || 'cmd' WHERE user_agent = 'CRCMD'`)
	// The attempted username is the target of an anonymous failed sign-in.
	e := ent("auth.login_failed")
	e.Actor = audit.Actor{Type: audit.ActorAnonymous, ID: "anon", Name: "=cmd|' /C calc'!A0"}
	e.TargetID = "@evil"
	if err := f.w.Record(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	recs := exportCSV(t, f, audit.Filter{})
	uas := map[string]bool{}
	for _, r := range recs[1:] {
		uas[r[11]] = true
		for i, cell := range r {
			if cell != "" && strings.ContainsAny(cell[:1], "=+-@\t\r") {
				t.Errorf("cell %d %q starts with a formula character", i, cell)
			}
		}
	}
	for _, want := range []string{"'=HYPERLINK(1)", "'+1+1", "'-2+3", "'@SUM(A1)", "'\tcmd", "'\rcmd", "safe"} {
		if !uas[want] {
			t.Errorf("user agent %q missing from %v", want, uas)
		}
	}
	last := recs[1] // newest first: the anonymous entry
	if last[3] != "'=cmd|' /C calc'!A0" || last[6] != "'@evil" {
		t.Errorf("anonymous entry actor/target = %q / %q", last[3], last[6])
	}
}

func TestExportCSVQuotesCommasQuotesAndNewlinesPerRFC4180(t *testing.T) {
	f := newFixture(t)
	f.clk.Set(t0)
	f.recordAs(t, "alice", "auth.login", "ua")
	// The writer strips control characters, so a newline can only arrive through
	// a row written by something else; the export must still keep it in one cell.
	f.exec(t, `UPDATE audit_log SET user_agent = ?, reason = ?`, "line one,\nline \"two\"", "a, b")
	var buf bytes.Buffer
	if err := audit.ExportCSV(context.Background(), f.s, audit.Filter{}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\"line one,\nline \"\"two\"\"\"") {
		t.Errorf("multi-line cell is not quoted per RFC 4180:\n%s", buf.String())
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("parse: %d rows, %v", len(recs), err)
	}
	if recs[1][11] != "line one,\nline \"two\"" || recs[1][9] != "a, b" {
		t.Errorf("round trip = %q / %q", recs[1][11], recs[1][9])
	}
}

func TestExportJSONIsAValidArrayWithTheSameEntriesAndNoChainFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var empty bytes.Buffer
	if err := audit.ExportJSON(ctx, f.s, audit.Filter{}, &empty); err != nil {
		t.Fatal(err)
	}
	var none []map[string]any
	if err := json.Unmarshal(empty.Bytes(), &none); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty export = %q (%v); want []", empty.String(), err)
	}

	f.record(t, 1100)
	var buf bytes.Buffer
	if err := audit.ExportJSON(ctx, f.s, audit.Filter{}, &buf); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("export is not a JSON array: %v", err)
	}
	if len(got) != 1100 {
		t.Fatalf("entries = %d, want 1100", len(got))
	}
	for _, banned := range []string{"hmac", "prevHmac", "prev_hmac", "keyId", "key_id"} {
		if _, ok := got[0][banned]; ok {
			t.Errorf("export carries %s", banned)
		}
	}
	for _, want := range []string{"id", "time", "actorType", "actor", "action", "targetType", "target", "result", "reason", "ip", "userAgent", "details"} {
		if _, ok := got[0][want]; !ok {
			t.Errorf("export lacks %s", want)
		}
	}
	if ts, _ := got[0]["time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("time = %v", got[0]["time"])
	}
}

// actorNames lists the names of an actor list, in order.
func actorNames(actors []audit.NamedActor) string {
	names := make([]string, len(actors))
	for i, a := range actors {
		names[i] = a.Name
	}
	return fmt.Sprint(names)
}

// recordOn appends one entry by actor on an environment ("" for an
// instance-level event).
func (f *fixture) recordOn(t *testing.T, actor, action, envID string) {
	t.Helper()
	e := ent(action)
	e.Actor = audit.Actor{Type: audit.ActorUser, ID: "id-" + actor, Name: actor}
	e.EnvID = envID
	if err := f.w.Record(context.Background(), e); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// scopedFixture holds 120 entries on env-a interleaved with 120 on env-b, and 30
// instance-level events with no environment.
func scopedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.clk.Set(t0)
	for i := range 120 {
		f.recordOn(t, "alice", fmt.Sprintf("on.a%d", i), "env-a")
		f.recordOn(t, "bob", fmt.Sprintf("on.b%d", i), "env-b")
		if i%4 == 0 {
			f.recordOn(t, "carol", fmt.Sprintf("instance.e%d", i), "")
		}
	}
	return f
}

func TestQueryScopedPagesExactlyTheEntriesOfTheListedEnvironmentsOnceEachNewestFirst(t *testing.T) {
	f := scopedFixture(t)
	scope := audit.Filter{Scoped: true, Envs: []string{"env-a"}}
	var seen []int64
	var sizes []int
	var cursor int64
	for range 5 {
		rows, next, err := audit.Query(context.Background(), f.s, scope, cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(rows))
		for _, r := range rows {
			if r.EnvID != "env-a" {
				t.Fatalf("a scoped page holds an entry on %q (%s)", r.EnvID, r.Action)
			}
			seen = append(seen, r.Seq)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	// The scope is applied before the page is cut: every page is full until the last.
	if fmt.Sprint(sizes) != "[50 50 20]" || len(seen) != 120 {
		t.Fatalf("page sizes = %v with %d entries, want [50 50 20] with the 120 on env-a", sizes, len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("seq %d follows %d: the order is not newest first", seen[i], seen[i-1])
		}
	}

	both := audit.Filter{Scoped: true, Envs: []string{"env-a", "env-b"}}
	rows, _, err := audit.Query(context.Background(), f.s, both, 0, 100)
	if err != nil || len(rows) != 100 {
		t.Fatalf("two environments: %d rows, err %v", len(rows), err)
	}
}

func TestQueryScopedForAnAdminOfEveryEnvironmentHoldsEveryEntryWithAnEnvironmentAndNoneWithout(t *testing.T) {
	f := scopedFixture(t)
	ctx := context.Background()
	all, _, err := audit.Query(ctx, f.s, audit.Filter{}, 0, 100)
	if err != nil || len(all) != 100 {
		t.Fatal(len(all), err)
	}
	var n, instance int
	var cursor int64
	for {
		rows, next, err := audit.Query(ctx, f.s, audit.Filter{Scoped: true, AnyEnv: true}, cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.EnvID == "" {
				instance++
			}
		}
		n += len(rows)
		if next == 0 {
			break
		}
		cursor = next
	}
	if n != 240 || instance != 0 {
		t.Errorf("an admin of every environment reads %d entries, %d without an environment; want 240 and 0", n, instance)
	}
}

func TestQueryScopedWithNothingInScopeMatchesNothingAndStillHonoursTheOtherFilters(t *testing.T) {
	f := scopedFixture(t)
	ctx := context.Background()
	rows, next, err := audit.Query(ctx, f.s, audit.Filter{Scoped: true}, 0, 50)
	if err != nil || len(rows) != 0 || next != 0 {
		t.Errorf("an empty scope = %d rows, next %d, err %v; want none", len(rows), next, err)
	}
	// An environment the log has never seen, and an environment ID that is
	// really a quote or a bracket, match nothing and break nothing.
	for _, id := range []string{"env-none", `x"]`, "' OR 1=1 --"} {
		rows, _, err = audit.Query(ctx, f.s, audit.Filter{Scoped: true, Envs: []string{id}}, 0, 50)
		if err != nil || len(rows) != 0 {
			t.Errorf("scope %q = %d rows, err %v; want none", id, len(rows), err)
		}
	}
	// The scope narrows; the actor and action filters narrow further.
	rows, _, err = audit.Query(ctx, f.s, audit.Filter{Scoped: true, Envs: []string{"env-a"}, Actor: "bob"}, 0, 50)
	if err != nil || len(rows) != 0 {
		t.Errorf("bob acted on env-b only: %d rows on env-a, err %v", len(rows), err)
	}
	rows, _, err = audit.Query(ctx, f.s, audit.Filter{Scoped: true, Envs: []string{"env-a"}, Actions: []string{"on.a7"}}, 0, 50)
	if err != nil || len(rows) != 1 {
		t.Errorf("one action on env-a: %d rows, err %v", len(rows), err)
	}
}

func TestScopedActorsAndBothExportsHoldOnlyTheScopedEntries(t *testing.T) {
	f := scopedFixture(t)
	ctx := context.Background()
	scope := audit.Filter{Scoped: true, Envs: []string{"env-a"}}

	actors, err := audit.Actors(ctx, f.s, scope)
	if err != nil || actorNames(actors) != "[alice]" {
		t.Errorf("actors under a scope = %s, err %v; want only alice", actorNames(actors), err)
	}
	actors, err = audit.Actors(ctx, f.s, audit.Filter{Scoped: true, AnyEnv: true})
	if err != nil || actorNames(actors) != "[alice bob]" {
		t.Errorf("actors of an admin of every environment = %s, err %v; want alice and bob, not carol", actorNames(actors), err)
	}
	actors, err = audit.Actors(ctx, f.s, audit.Filter{})
	if err != nil || actorNames(actors) != "[alice bob carol]" {
		t.Errorf("unscoped actors = %s, err %v", actorNames(actors), err)
	}

	recs := exportCSV(t, f, scope)
	if len(recs) != 121 {
		t.Fatalf("csv export under a scope = %d rows, want a header and 120", len(recs))
	}
	for _, r := range recs[1:] {
		if r[7] != "env-a" {
			t.Fatalf("csv export holds %v", r)
		}
	}
	var buf bytes.Buffer
	if err := audit.ExportJSON(ctx, f.s, scope, &buf); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || len(got) != 120 {
		t.Fatalf("json export under a scope = %d entries, err %v; want 120", len(got), err)
	}
	for _, e := range got {
		if e["environment"] != "env-a" {
			t.Fatalf("json export holds %v", e)
		}
	}
}

// deletedFixture has alice, who is deleted after she acted, and bob, who is not.
func deletedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.clk.Set(t0)
	for _, u := range []string{"alice", "bob"} {
		f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at)
			VALUES (?, ?, 'x', 1, 1, 1)`, "id-"+u, u)
	}
	f.recordAs(t, "alice", "auth.login", "ua")
	f.recordAs(t, "bob", "auth.login", "ua")
	f.recordAs(t, "alice", "auth.logout", "ua")
	return f
}

func chainOf(t *testing.T, f *fixture) []string {
	t.Helper()
	rows, err := f.s.Read.QueryContext(context.Background(), `SELECT hex(hmac) || hex(prev_hmac) || actor_name || actor_id FROM audit_log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQueryMarksTheEntriesOfADeletedUserWithoutRewritingTheSignedRows(t *testing.T) {
	f := deletedFixture(t)
	ctx := context.Background()
	before := chainOf(t, f)
	rows, _, err := audit.Query(ctx, f.s, audit.Filter{}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ActorDeleted {
			t.Fatalf("%s by %s reads as deleted before anyone is", r.Action, r.ActorName)
		}
	}

	f.exec(t, `UPDATE users SET deleted_at = 5 WHERE id = 'id-alice'`)
	rows, _, err = audit.Query(ctx, f.s, audit.Filter{}, 0, 50)
	if err != nil || len(rows) != 3 {
		t.Fatal(len(rows), err)
	}
	for _, r := range rows {
		if want := r.ActorName == "alice"; r.ActorDeleted != want {
			t.Errorf("%s by %s: actorDeleted = %v, want %v", r.Action, r.ActorName, r.ActorDeleted, want)
		}
	}
	if b, _ := json.Marshal(rows[0]); !strings.Contains(string(b), `"actorDeleted":true`) {
		t.Errorf("row JSON %s does not carry actorDeleted", b)
	}

	if after := chainOf(t, f); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("deleting a user rewrote a signed row")
	}
	if got := f.verify(t); got.Broken != nil || got.Entries != 3 {
		t.Errorf("verify after a delete = %+v", got)
	}
}

func TestActorsMarkADeletedUserAndTheExportsCarryActorDeleted(t *testing.T) {
	f := deletedFixture(t)
	f.exec(t, `UPDATE users SET deleted_at = 5 WHERE id = 'id-alice'`)
	ctx := context.Background()

	actors, err := audit.Actors(ctx, f.s, audit.Filter{})
	if err != nil || fmt.Sprint(actors) != "[{alice true} {bob false}]" {
		t.Errorf("actors = %v, err %v; want alice deleted and bob not", actors, err)
	}

	recs := exportCSV(t, f, audit.Filter{})
	col := len(recs[0]) - 1
	if recs[0][col] != "actor_deleted" {
		t.Fatalf("csv header = %v, want actor_deleted last", recs[0])
	}
	for _, r := range recs[1:] {
		if want := map[bool]string{true: "true", false: "false"}[r[3] == "alice"]; r[col] != want {
			t.Errorf("csv row by %s: actor_deleted = %q, want %q", r[3], r[col], want)
		}
	}
	var buf bytes.Buffer
	if err := audit.ExportJSON(ctx, f.s, audit.Filter{}, &buf); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || len(got) != 3 {
		t.Fatal(len(got), err)
	}
	for _, e := range got {
		if want := e["actor"] == "alice"; e["actorDeleted"] != want {
			t.Errorf("json entry by %v: actorDeleted = %v, want %v", e["actor"], e["actorDeleted"], want)
		}
	}
}

func TestQueryFiltersByBulkAndKeepsTheOtherFiltersAndTheScope(t *testing.T) {
	f := newFixture(t)
	f.clk.Set(t0)
	rec := func(actor, target, env, bulk string) {
		e := ent("container.stopped")
		e.Actor = audit.Actor{Type: audit.ActorUser, ID: "id-" + actor, Name: actor}
		e.TargetID, e.EnvID = target, env
		e.Details = map[string]string{}
		if bulk != "" {
			e.Details["bulk"] = bulk
		}
		if err := f.w.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	rec("alice", "a1", "env-a", "bulk-1")
	rec("bob", "a2", "env-a", "bulk-2")
	rec("alice", "b1", "env-b", "bulk-1")
	rec("alice", "a3", "env-a", "")
	targets := func(flt audit.Filter) string {
		rows, _, err := audit.Query(context.Background(), f.s, flt, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.TargetID)
		}
		return strings.Join(out, ",")
	}
	if got := targets(audit.Filter{Bulk: "bulk-1"}); got != "b1,a1" {
		t.Errorf("bulk-1 = %q, want b1,a1", got)
	}
	if got := targets(audit.Filter{Bulk: "nope"}); got != "" {
		t.Errorf("an unknown bulk = %q, want none", got)
	}
	if got := targets(audit.Filter{}); got != "a3,b1,a2,a1" {
		t.Errorf("no bulk = %q, want every row", got)
	}
	if got := targets(audit.Filter{Bulk: "bulk-1", Actor: "bob"}); got != "" {
		t.Errorf("bulk-1 by bob = %q, want none", got)
	}
	if got := targets(audit.Filter{Bulk: "bulk-1", Scoped: true, Envs: []string{"env-a"}}); got != "a1" {
		t.Errorf("bulk-1 scoped to env-a = %q, want a1 only", got)
	}
}
