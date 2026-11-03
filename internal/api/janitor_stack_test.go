// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
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
