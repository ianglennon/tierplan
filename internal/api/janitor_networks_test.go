// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The janitor's networks (plan 11-08, JAN-04, JAN-06, D-01, D-11). A fake daemon behind the real
// monitor, aggregator and orphan check; the janitor judges networks by the very check the Address
// space page lists with and removes them by the code its delete route calls.

// stackNet is an idle network of a swarm stack, idle since ago before the fixture's clock.
func stackNet(id, name, stack string, ago time.Duration) dockertest.FakeNetwork {
	n := idleNet(id, name)
	n.Created = janitorNow.Add(-ago)
	n.Labels = map[string]string{"com.docker.stack.namespace": stack}
	return n
}

// previewJanitor previews as the fixture's admin once the monitor has seen the node.
func (f *envFix) previewJanitor(t *testing.T, e envBody) janitorPreview {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/janitor/preview", "")
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	return decode[janitorPreview](t, rec)
}

// cleanNow ticks the items and waits for the run's job to end.
func (f *envFix) cleanNow(t *testing.T, e envBody, items ...janitorItem) jobs.Job {
	t.Helper()
	rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/janitor/runs", tick(items...))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	started := decode[janitorStarted](t, rec)
	job, err := f.jobHub.Wait(testsupport.Context(t), started.JobID)
	if err != nil {
		t.Fatalf("the job never ended: %v", err)
	}
	return job
}

// actorsOf lists type:id of the actor of each audit row of the action, in order.
func (f *envFix) actorsOf(t *testing.T, action string) []string {
	t.Helper()
	q, err := f.st.Read.QueryContext(testsupport.Context(t), `SELECT actor_type || ':' || actor_id FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	var out []string
	for q.Next() {
		var a string
		if err := q.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// The tracer: a network left behind by a removed stack is in the preview, Clean now removes it by
// the Address space page's path, and the audit log holds the same evidence as a manual delete.
func TestJanitorRemovesOrphanedNetwork(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(stackNet("old1", "oldshop_default", "oldshop", 25*time.Hour)))
	preview := f.previewJanitor(t, e)
	if len(preview.Items) != 1 || preview.Summary.Networks != 1 {
		t.Fatalf("preview items = %+v summary = %+v, want the one orphaned network", preview.Items, preview.Summary)
	}
	it := preview.Items[0]
	if it.Type != "network" || it.Ref != "old1" || it.Name != "oldshop_default" || it.Class != "removable" || it.Reason.Code != "network_orphaned" {
		t.Fatalf("item = %+v, want oldshop_default removable network_orphaned", it)
	}
	if preview.NetworksWithheld || len(preview.WithheldNodes) != 0 {
		t.Errorf("withheld = %v %v, want nothing withheld", preview.NetworksWithheld, preview.WithheldNodes)
	}

	job := f.cleanNow(t, e, it)
	if job.State != jobs.JobDone || len(job.Items) != 1 || job.Items[0].State != jobs.ItemDone || job.Items[0].Kind != "network" {
		t.Fatalf("job = %s items %+v, want a done job with one done network", job.State, job.Items)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /networks/old1"}) {
		t.Fatalf("writes = %v, want exactly one remove of the network", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 2 || rows[0].Action != audit.ActionNetworkInspectSaved || rows[1].Action != audit.ActionNetworkRemoved {
		t.Fatalf("audit = %+v, want network.inspect_saved and then network.removed", rows)
	}
	for _, r := range rows {
		if r.Details["source"] != "janitor" || r.TargetID != "old1" || r.EnvID != e.ID || r.Result != string(audit.ResultSuccess) {
			t.Errorf("%s = %+v, want a success on old1 with source janitor", r.Action, r)
		}
	}
	if rows[1].Details["reason"] != "orphan" || rows[1].Details["code"] != "removed" {
		t.Errorf("network.removed = %+v, want reason orphan and code removed", rows[1])
	}
	// The run's actor is the person who pressed Clean now, on the network rows as on the run's own.
	cleaned := f.actorsOf(t, audit.ActionJanitorCleaned)
	if len(cleaned) != 1 || !slices.Equal(f.actorsOf(t, audit.ActionNetworkRemoved), cleaned) || !slices.Equal(f.actorsOf(t, audit.ActionNetworkInspectSaved), cleaned) {
		t.Errorf("actors: cleaned %v removed %v saved %v, want one and the same", cleaned, f.actorsOf(t, audit.ActionNetworkRemoved), f.actorsOf(t, audit.ActionNetworkInspectSaved))
	}
	if after := f.previewJanitor(t, e); len(after.Items) != 0 {
		t.Errorf("preview after = %+v, want the network gone", after.Items)
	}
}
