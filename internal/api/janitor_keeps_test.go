// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Keeping anything out of cleanup from the Janitor page (plan 11-27, JAN-07, JAN-06, D-12, D-21). The
// engines are fake daemons behind the real monitor and aggregator; the routes, the planner, the runner
// and the audit log are the real ones.

// keepOne is the body of a keep for one object.
func keepOne(kind, nodeID, ref string) string {
	return fmt.Sprintf(`{"targets":[{"kind":%q,"nodeId":%q,"ref":%q}]}`, kind, nodeID, ref)
}

// keepsURL is the keep route of an environment.
func keepsURL(envID string) string { return "/api/env/" + envID + "/janitor/keeps" }

// notRemovedRef finds an item the preview left, by its ref.
func notRemovedRef(p janitorPreview, ref string) (janitorItem, bool) {
	for _, it := range p.NotRemoved {
		if it.Ref == ref {
			return it, true
		}
	}
	return janitorItem{}, false
}

// The tracer: a stale task container kept from the Janitor page is listed protected, and neither a
// scheduled run (All) nor Clean now removes it.
func TestJanitorKeepContainerProtects(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, before := s.previewJanitor(t)
	if len(before.Items) != 3 || before.Items[0].Ref != "web-x1" {
		t.Fatalf("preview items = %+v, want web-x1 to web-x3 removable", before.Items)
	}
	victim := before.Items[0]

	rec := s.req(t, http.MethodPut, keepsURL(s.env.ID), keepOne("container", victim.NodeID, victim.Ref))
	if rec.code != http.StatusOK || decode[struct {
		Kept int `json:"kept"`
	}](t, rec).Kept != 1 {
		t.Fatalf("keep: %d %s, want 200 with one new keep", rec.code, rec.body)
	}

	_, after := s.previewJanitor(t)
	var refs []string
	for _, it := range after.Items {
		refs = append(refs, it.Ref)
	}
	if !slices.Equal(refs, []string{"web-x2", "web-x3"}) {
		t.Fatalf("preview items after the keep = %v, want web-x2 and web-x3", refs)
	}
	it, ok := notRemovedRef(after, "web-x1")
	if !ok || it.Class != "protected" || it.Reason.Code != "kept_row" || it.Type != "container" {
		t.Fatalf("web-x1 = %+v (listed %v), want a protected container with reason kept_row", it, ok)
	}

	// A scheduled run removes everything removable on its own fresh plan, and leaves the kept container.
	runner := &janitor.Runner{
		Engine: api.NewJanitorEngine(s.agg, s.clusters, nil, janitor.KeepStore{St: s.st}, nil), Locks: s.stackLocks, Jobs: s.jobHub,
		Clock: nowClock(s.clk.now), Store: &janitor.SQLStore{St: s.st}, Audit: s.aud, Policy: janitor.PolicyStore{St: s.st}.Effective,
		Nodes: janitor.PolicyStore{St: s.st}.Nodes,
	}
	started, err := runner.Start(testsupport.Context(t), janitor.RunRequest{
		EnvID: s.env.ID, EnvName: "swarm", Trigger: janitor.TriggerSchedule, Actor: "system:janitor", Owner: "system:janitor:" + s.env.ID, All: true,
	})
	if err != nil {
		t.Fatalf("Runner.Start: %v", err)
	}
	s.awaitRun(t, started.JobID)
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/web-x2", "DELETE /containers/web-x3"}) {
		t.Fatalf("the daemons saw %v, want web-x2 and web-x3 deleted and web-x1 left", got)
	}
}
