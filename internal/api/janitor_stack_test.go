// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Excluding a whole stack from the janitor (plan 11-28, JAN-07, D-10, D-21). The engines are fake daemons
// behind the real monitor and aggregator; the routes, the planner, the runner and the audit log are the real
// ones.

// stackJanitorURL is the exclusion route of a stack.
func stackJanitorURL(envID, stackID string) string {
	return "/api/env/" + envID + "/stacks/" + stackID + "/janitor"
}

// ownedSlot is exitedSlot where every container carries the labels docker stack deploy and TierPlan put on
// what the stack stk-shop (named shop) owns.
func ownedSlot(n int) []taskBox {
	boxes := exitedSlot(n)
	for i := range boxes {
		boxes[i].labels = map[string]string{"com.docker.stack.namespace": "shop", "tierplan.stack.id": "stk-shop"}
	}
	return boxes
}

type stackJanitorJSON struct {
	Excluded bool `json:"excluded"`
}

// The tracer: a stack someone excludes keeps its five old task containers, whether the janitor is previewed,
// run on a schedule (All) or run by Clean now, and including it again makes them removable once more.
func TestExcludedStackNeverCleaned(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(ownedSlot(5)), nil)
	s.insertStackRow(t, s.env.ID, "stk-shop", "shop")
	removable := []string{"web-x1", "web-x2", "web-x3"}
	refs := func(items []janitorItem) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.Ref)
		}
		return out
	}
	if _, p := s.previewJanitor(t); !slices.Equal(refs(p.Items), removable) {
		t.Fatalf("preview items = %v, want %v removable before the stack is excluded", refs(p.Items), removable)
	}

	rec := s.req(t, http.MethodPut, stackJanitorURL(s.env.ID, "stk-shop"), `{"excluded":true}`)
	if rec.code != http.StatusOK || !decode[stackJanitorJSON](t, rec).Excluded {
		t.Fatalf("exclude: %d %s, want 200 excluded", rec.code, rec.body)
	}
	rec, p := s.previewJanitor(t)
	if rec.code != http.StatusOK || len(p.Items) != 0 || len(p.NotRemoved) != 3 {
		t.Fatalf("preview after the exclusion: %d items %+v notRemoved %+v, want nothing removable and the three protected", rec.code, p.Items, p.NotRemoved)
	}
	for _, it := range p.NotRemoved {
		if it.Class != "protected" || it.Reason.Code != "stack_excluded" || it.Type != "container" {
			t.Errorf("%+v, want a protected container with reason stack_excluded", it)
		}
	}

	runner := &janitor.Runner{
		Engine: api.NewJanitorEngine(s.agg, s.clusters, nil, janitor.KeepStore{St: s.st}, nil), Locks: s.stackLocks, Jobs: s.jobHub,
		Clock: nowClock(s.clk.now), Store: &janitor.SQLStore{St: s.st}, Audit: s.aud, Policy: janitor.PolicyStore{St: s.st}.Effective,
		Nodes: janitor.PolicyStore{St: s.st}.Nodes,
	}
	run := func() {
		t.Helper()
		started, err := runner.Start(testsupport.Context(t), janitor.RunRequest{
			EnvID: s.env.ID, EnvName: "swarm", Trigger: janitor.TriggerSchedule, Actor: "system:janitor", Owner: "system:janitor:" + s.env.ID, All: true,
		})
		if err != nil {
			t.Fatalf("Runner.Start: %v", err)
		}
		s.awaitRun(t, started.JobID)
	}
	run()
	if got := s.containerDeletes(); len(got) != 0 {
		t.Fatalf("the daemons saw %v after a run on an excluded stack, want no delete", got)
	}

	rec = s.req(t, http.MethodPut, stackJanitorURL(s.env.ID, "stk-shop"), `{"excluded":false}`)
	if rec.code != http.StatusOK || decode[stackJanitorJSON](t, rec).Excluded {
		t.Fatalf("include: %d %s, want 200 and no longer excluded", rec.code, rec.body)
	}
	if _, p := s.previewJanitor(t); !slices.Equal(refs(p.Items), removable) || len(p.NotRemoved) != 0 {
		t.Fatalf("preview after including it again: items %v notRemoved %+v, want %v removable", refs(p.Items), p.NotRemoved, removable)
	}
	run()
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/web-x1", "DELETE /containers/web-x2", "DELETE /containers/web-x3"}) {
		t.Fatalf("the daemons saw %v, want the three old containers deleted once the stack is included again", got)
	}

	excluded := janitorKeepAudits(t, s.st.Read, "stack.janitor_excluded")
	included := janitorKeepAudits(t, s.st.Read, "stack.janitor_included")
	if len(excluded) != 1 || excluded[0].TargetType != "stack" || excluded[0].TargetID != "stk-shop" || excluded[0].EnvID != s.env.ID ||
		excluded[0].Result != "success" || excluded[0].Details["name"] != "shop" {
		t.Fatalf("stack.janitor_excluded = %+v, want one row for stk-shop naming shop", excluded)
	}
	if len(included) != 1 || included[0].TargetID != "stk-shop" || included[0].Details["name"] != "shop" {
		t.Fatalf("stack.janitor_included = %+v, want one row for stk-shop naming shop", included)
	}
}

// Only a holder of janitor.policy excludes a stack: an Operator without the extra is refused and the stack is
// left as it was; an Operator with it excludes; a Viewer is refused (AUTH-07, T-11-72).
func TestStackJanitorNeedsPolicy(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	awaitOnline(t, f.mon, e.ID)
	f.insertStackRow(t, e.ID, "stk-shop", "shop")
	path := stackJanitorURL(e.ID, "stk-shop")
	as := func(user, role string, extra bool) (cookie, csrf string) {
		f.addUser(t, user, false)
		f.addEnvRole(t, user, e.ID, role)
		if extra {
			if _, err := f.st.Write.Exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'janitor.policy')`, "ra-"+user+"-"+e.ID); err != nil {
				t.Fatal(err)
			}
		}
		return f.signIn(t, user, false)
	}
	put := func(cookie, csrf string) *httptest.ResponseRecorder {
		return f.call(t, authReq{method: http.MethodPut, path: path, body: `{"excluded":true}`, cookie: cookie, csrf: csrf})
	}
	excluded := func() int {
		var n int
		if err := f.st.Read.QueryRow(`SELECT janitor_excluded FROM stacks WHERE id = 'stk-shop'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	cookie, csrf := as("olive", "operator", false)
	if rec := put(cookie, csrf); rec.Code != http.StatusForbidden {
		t.Fatalf("an operator without the extra: %d %s, want 403", rec.Code, rec.Body)
	}
	vc, vcsrf := as("vera", "viewer", false)
	if rec := put(vc, vcsrf); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	if excluded() != 0 {
		t.Fatal("the stack is excluded after two refusals, want it untouched")
	}
	if rows := janitorKeepAudits(t, f.st.Read, "stack.janitor_excluded"); len(rows) != 0 {
		t.Fatalf("audit rows = %+v after two refusals, want none", rows)
	}

	tc, tcsrf := as("trent", "operator", true)
	rec := put(tc, tcsrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("an operator with janitor.policy: %d %s, want 200", rec.Code, rec.Body)
	}
	if excluded() != 1 {
		t.Fatal("the stack is not excluded after the operator with janitor.policy excluded it")
	}
	var actor string
	if err := f.st.Read.QueryRow(`SELECT actor_id FROM audit_log WHERE action = 'stack.janitor_excluded'`).Scan(&actor); err != nil || actor != "id-trent" {
		t.Errorf("actor = %q (%v), want the person who excluded it, id-trent", actor, err)
	}
}

// A stack the environment does not have is 404 stack_not_found: one that never existed, and one of another
// environment (T-11-72).
func TestStackJanitorUnknownStack(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	awaitOnline(t, f.mon, e.ID)
	f.insertStackRow(t, e.ID, "stk-shop", "shop")

	rec := f.req(t, http.MethodPut, stackJanitorURL(e.ID, "stk-nope"), `{"excluded":true}`)
	if rec.code != http.StatusNotFound || !strings.Contains(string(rec.body), "stack_not_found") {
		t.Fatalf("an unknown stack: %d %s, want 404 stack_not_found", rec.code, rec.body)
	}
	f.addEnvironmentRow(t, "env-other")
	f.insertStackRow(t, "env-other", "stk-away", "away")
	rec = f.req(t, http.MethodPut, stackJanitorURL(e.ID, "stk-away"), `{"excluded":true}`)
	if rec.code != http.StatusNotFound {
		t.Fatalf("a stack of another environment: %d %s, want 404", rec.code, rec.body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stacks WHERE janitor_excluded <> 0`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d stacks excluded (%v), want none", n, err)
	}
	if rows := janitorKeepAudits(t, f.st.Read, "stack.janitor_excluded"); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none", rows)
	}
}

// The same value twice writes nothing and records nothing: a stack that is not excluded and is included again,
// and one that is excluded and is excluded again, both answer 200 with the value and leave the audit log alone.
func TestStackJanitorNoOp(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	awaitOnline(t, f.mon, e.ID)
	f.insertStackRow(t, e.ID, "stk-shop", "shop")
	path := stackJanitorURL(e.ID, "stk-shop")
	count := func() int {
		var n int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action IN ('stack.janitor_excluded', 'stack.janitor_included')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Each step sets the value and says how many audit rows exist afterwards: only a change adds one.
	steps := []struct {
		excluded bool
		rows     int
	}{{false, 0}, {true, 1}, {true, 1}, {false, 2}, {false, 2}}
	for _, st := range steps {
		body := `{"excluded":false}`
		if st.excluded {
			body = `{"excluded":true}`
		}
		rec := f.req(t, http.MethodPut, path, body)
		if rec.code != http.StatusOK || decode[stackJanitorJSON](t, rec).Excluded != st.excluded {
			t.Fatalf("PUT %s: %d %s, want 200 excluded %v", body, rec.code, rec.body, st.excluded)
		}
		// The stack's own body says the same.
		if got := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/stacks/stk-shop", ""); got.code != http.StatusOK || decode[struct {
			Janitor stackJanitorJSON `json:"janitor"`
		}](t, got).Janitor.Excluded != st.excluded {
			t.Fatalf("GET the stack: %d %s, want 200 with janitor.excluded %v", got.code, got.body, st.excluded)
		}
		if got := count(); got != st.rows {
			t.Fatalf("after PUT %s there are %d audit rows, want %d: a repeat records nothing", body, got, st.rows)
		}
	}
	if rows := janitorKeepAudits(t, f.st.Read, "stack.janitor_included"); len(rows) != 1 || rows[0].Details["name"] != "shop" {
		t.Fatalf("stack.janitor_included = %+v, want one row naming shop", rows)
	}
}
