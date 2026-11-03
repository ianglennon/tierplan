// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/tunnel"
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
func (f *envFix) cleanNow(t *testing.T, e envBody, items ...janitorItem) (jobs.Job, janitorStarted) {
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
	return job, started
}

// runRecord reads a run back as the fixture's admin.
func (f *envFix) runRecord(t *testing.T, e envBody, runID string) janitorRunRead {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/janitor/runs/"+runID, "")
	if rec.code != http.StatusOK {
		t.Fatalf("read run: %d %s", rec.code, rec.body)
	}
	return decode[janitorRunRead](t, rec)
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

	job, _ := f.cleanNow(t, e, it)
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

// netItem is the preview's item for the network called name, wherever the preview put it.
func netItem(t *testing.T, p janitorPreview, name string) janitorItem {
	t.Helper()
	for _, it := range append(slices.Clone(p.Items), p.NotRemoved...) {
		if it.Type == "network" && it.Name == name {
			return it
		}
	}
	t.Fatalf("the preview has no network %s: %+v %+v", name, p.Items, p.NotRemoved)
	return janitorItem{}
}

// Every class the janitor reads of a network, in one preview.
func TestJanitorPreviewNetworkClasses(t *testing.T) {
	t.Parallel()
	kept := stackNet("kept1", "kept_net", "oldshop", 72*time.Hour)
	kept.Labels["tierplan.keep"] = "true"
	owned := idleNet("owned1", "live_default")
	owned.Created = janitorNow.Add(-72 * time.Hour)
	owned.Labels = map[string]string{"com.docker.compose.project": "live"}
	busy := idleNet("busy1", "busy")
	busy.Created = janitorNow.Add(-72 * time.Hour)
	loose := idleNet("loose1", "loose")
	loose.Created = janitorNow.Add(-72 * time.Hour)
	cfg := orphanEngine(
		stackNet("old1", "oldshop_default", "oldshop", 72*time.Hour), stackNet("new1", "newshop_default", "newshop", 2*time.Hour),
		loose, kept, owned, busy,
	)
	cfg.Containers = []dockertest.FakeContainer{
		{ID: "c-live", Names: []string{"/live-web-1"}, State: "running", Labels: map[string]string{"com.docker.compose.project": "live"}},
		{ID: "c-busy", Names: []string{"/busy-1"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"busy": {NetworkID: "busy1"}}},
	}
	f, e := dashEnv(t, cfg)
	p := f.previewJanitor(t, e)

	removable := netItem(t, p, "oldshop_default")
	if removable.Class != "removable" || removable.Reason.Code != "network_orphaned" || p.Summary.Networks != 1 || len(p.Items) != 1 {
		t.Errorf("oldshop_default = %+v summary = %+v items = %d, want it the only removable network", removable, p.Summary, len(p.Items))
	}
	grace := netItem(t, p, "newshop_default")
	if grace.Class != "grace" || grace.Reason.Code != "younger_than_grace" || grace.Reason.RemainingSeconds != int64((22*time.Hour).Seconds()) {
		t.Errorf("newshop_default = %+v, want grace with 22h left", grace)
	}
	unused := netItem(t, p, "loose")
	if unused.Class != "reported" || unused.Reason.Code != "opt_in_off_networks" {
		t.Errorf("loose = %+v, want reported opt_in_off_networks while the opt-in is off", unused)
	}
	protected := netItem(t, p, "kept_net")
	if protected.Class != "protected" || protected.Reason.Code != "kept_label" {
		t.Errorf("kept_net = %+v, want protected kept_label", protected)
	}
	for _, name := range []string{"bridge", "live_default", "busy"} {
		for _, it := range append(slices.Clone(p.Items), p.NotRemoved...) {
			if it.Name == name {
				t.Errorf("%s is listed: %+v, want an engine, owned or in-use network left out", name, it)
			}
		}
	}
	if p.NetworksWithheld {
		t.Error("networks withheld, want every node reporting")
	}
}

// An unused network is never removed without the environment's opt-in, even when it is ticked.
func TestJanitorUnusedNetworkNeedsOptIn(t *testing.T) {
	t.Parallel()
	loose := idleNet("loose1", "loose")
	loose.Created = janitorNow.Add(-72 * time.Hour)
	f, e := dashEnv(t, orphanEngine(loose))
	it := netItem(t, f.previewJanitor(t, e), "loose")
	job, started := f.cleanNow(t, e, it)
	if job.State != jobs.JobDone || len(job.Items) != 0 {
		t.Fatalf("job = %s items %+v, want a done job that attempted nothing", job.State, job.Items)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none: the network is unused and the opt-in is off", w)
	}
	run := f.runRecord(t, e, started.RunID)
	if got, want := runItems(run), []string{"loose1 skipped_protected no_longer_stale"}; !slices.Equal(got, want) || run.Counts.RemovedNetworks != 0 {
		t.Fatalf("run items = %v counts = %+v, want %v", got, run.Counts, want)
	}
	f.noInspectSaved(t)
}

// A container attached after the preview makes the network in use: the run's own check, made after
// its containers, sees it and sends nothing.
func TestJanitorNetworkInUseAgain(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine(stackNet("old1", "oldshop_default", "oldshop", 72*time.Hour))
	f, e := dashEnv(t, cfg)
	it := netItem(t, f.previewJanitor(t, e), "oldshop_default")
	cfg.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/late"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"oldshop_default": {NetworkID: "old1"}}}}
	f.fake.Reconfigure(cfg)

	job, started := f.cleanNow(t, e, it)
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s, want done", job.State)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none: the network is in use", w)
	}
	f.noInspectSaved(t)
	if n := len(f.keepAudits(t, audit.ActionNetworkRemoved)); n != 0 {
		t.Errorf("%d network.removed rows, want none: nothing was attempted", n)
	}
	run := f.runRecord(t, e, started.RunID)
	if got, want := runItems(run), []string{"old1 skipped_protected no_longer_stale"}; !slices.Equal(got, want) || run.Counts.Kept != 1 || run.State != "completed" {
		t.Fatalf("run = %v %+v %s, want the network kept and the run completed", got, run.Counts, run.State)
	}
}

// The engine itself refuses the remove as in use: kept, recorded once, sent once, never retried.
func TestJanitorNetworkEngineRefusal(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine(stackNet("old1", "oldshop_default", "oldshop", 72*time.Hour))
	cfg.RemoveErrors = map[string]string{"oldshop_default": "error while removing network: network oldshop_default id old1 has active endpoints"}
	f, e := dashEnv(t, cfg)
	it := netItem(t, f.previewJanitor(t, e), "oldshop_default")
	_, started := f.cleanNow(t, e, it)
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /networks/old1"}) {
		t.Fatalf("writes = %v, want exactly one remove request", w)
	}
	run := f.runRecord(t, e, started.RunID)
	if got, want := runItems(run), []string{"old1 refused_in_use in_use"}; !slices.Equal(got, want) || run.Counts.Kept != 1 || run.State != "completed" {
		t.Fatalf("run = %v %+v %s, want refused_in_use and a completed run", got, run.Counts, run.State)
	}
	saved := f.keepAudits(t, audit.ActionNetworkInspectSaved)
	removed := f.keepAudits(t, audit.ActionNetworkRemoved)
	if len(saved) != 1 || len(removed) != 1 || removed[0].Result != string(audit.ResultFailure) || removed[0].Details["code"] != "network_in_use" || removed[0].Details["source"] != "janitor" {
		t.Fatalf("saved = %+v removed = %+v, want the inspect saved before the remove and one failure with source janitor", saved, removed)
	}
}

// A document the audit log cannot store whole refuses the removal, as it refuses the manual one.
func TestJanitorNetworkInspectTooLarge(t *testing.T) {
	t.Parallel()
	n := stackNet("old1", "oldshop_default", "oldshop", 72*time.Hour)
	for i := range 300 {
		n.Labels[fmt.Sprintf("label-%03d", i)] = strings.Repeat("x", 250)
	}
	f, e := dashEnv(t, orphanEngine(n))
	it := netItem(t, f.previewJanitor(t, e), "oldshop_default")
	_, started := f.cleanNow(t, e, it)
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none: the remove must not be sent", w)
	}
	f.noInspectSaved(t)
	run := f.runRecord(t, e, started.RunID)
	if got, want := runItems(run), []string{"old1 error inspect_too_large"}; !slices.Equal(got, want) || run.Counts.Failed != 1 || run.State != "partial" {
		t.Fatalf("run = %v %+v %s, want error inspect_too_large and a partial run", got, run.Counts, run.State)
	}
	removed := f.keepAudits(t, audit.ActionNetworkRemoved)
	if len(removed) != 1 || removed[0].Details["code"] != "inspect_too_large" || removed[0].Details["source"] != "janitor" {
		t.Fatalf("network.removed = %+v, want one failure inspect_too_large with source janitor", removed)
	}
}

// A node that does not report withholds every network: the preview says so and names it, and a
// run ticked before the node went away removes nothing.
func TestJanitorNetworkNodeMissing(t *testing.T) {
	t.Parallel()
	s := swarmTuned(t, func(cfg *dockertest.FakeConfig) {
		cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{
			ID: "old-id", Name: "oldshop_default", Driver: "overlay", Scope: "swarm", Created: longAgo,
			Labels: map[string]string{"com.docker.stack.namespace": "oldshop"},
		})
	})
	_, before := s.previewJanitor(t)
	it := netItem(t, before, "oldshop_default")
	if it.Class != "removable" || it.Reason.Code != "network_orphaned" || it.NodeID != "" || it.Node != "Cluster" {
		t.Fatalf("item = %+v, want the swarm-wide network removable, on no node", it)
	}

	s.srv.Hub.Kick(s.w.id, tunnel.DisconnectClosed)
	if _, err := s.srv.Hub.AwaitSession(testsupport.Context(t), s.w.id, func(sess *tunnel.Session) bool { return sess == nil }); err != nil {
		t.Fatalf("the worker's session never went: %v", err)
	}
	_, during := s.previewJanitor(t)
	held := netItem(t, during, "oldshop_default")
	if held.Class != "cant_tell" || held.Reason.Code != "networks_withheld" || !during.NetworksWithheld || !slices.Equal(during.WithheldNodes, []string{"wrk-1"}) {
		t.Fatalf("item = %+v withheld = %v %v, want cant_tell networks_withheld, networks withheld and wrk-1 named", held, during.NetworksWithheld, during.WithheldNodes)
	}
	if during.Summary.Networks != 0 {
		t.Errorf("summary = %+v, want no network removable while a node is out", during.Summary)
	}

	rec, started := s.runJanitor(t, tick(it))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	s.noDeletes(t)
	_, run := s.getRun(t, started.RunID)
	if got, want := runItems(run), []string{"old-id skipped_node_offline node_offline"}; !slices.Equal(got, want) || run.Counts.SkippedOffline != 1 || run.State != "partial" {
		t.Fatalf("run = %v %+v %s, want the network skipped for the missing node and a partial run", got, run.Counts, run.State)
	}
}
