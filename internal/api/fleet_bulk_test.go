// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/wire"
)

// Updating and skipping many workloads across the fleet in one action (plan 12-30, ENV-10, IMG-06, D-10, D-11, D-14, D-15). Docker is a
// fake daemon behind the real monitor, aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the
// job hub, the operation lock, the router and the permission checks are the real ones.

// bulkItem is one workload of a request.
func bulkItem(envID, kind, name string, expect map[string]string) map[string]any {
	it := map[string]any{"envId": envID, "kind": kind, "name": name}
	if expect != nil {
		it["expect"] = expect
	}
	return it
}

type bulkSkipped struct {
	EnvID  string `json:"envId"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Code   string `json:"code"`
}

type bulkAnswer struct {
	BulkID string `json:"bulkId"`
	Jobs   []struct {
		EnvID string `json:"envId"`
		JobID string `json:"jobId"`
	} `json:"jobs"`
	Skipped []bulkSkipped `json:"skipped"`
}

// reasons are the reasons of the skipped items, by "kind/name".
func (a bulkAnswer) reasons() map[string]string {
	out := map[string]string{}
	for _, s := range a.Skipped {
		out[s.Kind+"/"+s.Name] = s.Reason
	}
	return out
}

func (a bulkAnswer) jobOf(t *testing.T, envID string) string {
	t.Helper()
	for _, j := range a.Jobs {
		if j.EnvID == envID {
			return j.JobID
		}
	}
	t.Fatalf("no job for environment %s in %+v", envID, a)
	return ""
}

func bulkBody(items ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"items": items})
	return string(raw)
}

// bulkRunAs posts a bulk update as the admin and answers the recorder.
func (f *envFix) bulkRun(t *testing.T, items ...map[string]any) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/fleet/updates/run", bulkBody(items...))
}

// bulkSwarm is one swarm manager running the services, whose registry says what heads says, whose tasks follow the writes made to it
// and whose applier waits on a clock that answers at once.
func bulkSwarm(t *testing.T, heads *scriptedHeads, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	cfg.TasksFollowWrites = true
	cfg.SwarmNodeID = "node-1"
	if tune != nil {
		tune(&cfg)
	}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.updateApplier = func(a *updates.Applier) { a.Clock = &instantClock{now: time.Unix(1_700_000_000, 0)} }
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	return f, e
}

// bulkStates are the states of a bulk job's items by ID.
func bulkItemStates(job jobs.Job) map[string]string {
	out := map[string]string{}
	for _, it := range job.Items {
		out[it.ID] = it.State
	}
	return out
}

// runsOfBulk are the runs a bulk update recorded, oldest first.
func (f *envFix) runsOfBulk(t *testing.T, bulkID string) []updates.Run {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT id FROM image_update_runs WHERE bulk_id = ? ORDER BY started_at, id`, bulkID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	var out []updates.Run
	for _, id := range ids {
		run, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, run)
	}
	return out
}

// The tracer (plan 12-30, IMG-06, D-11): "Update 3 selected" across two environments makes two jobs, one in each, which update their
// items one at a time under one bulk ID. A swarm reached through its agent has two services; the local host has one container. Every
// item is done, every audit row of the selection carries the bulk ID, and each environment's job ends with a summary row.
func TestBulkUpdateTwoEnvironments(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('b')}, "team/app": {digest: updDigest('b')},
	})
	f := newAgentFixHost(t, "unix://"+dockertest.NewFake(t, containerEngine(plainContainer())).SocketPath())
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.updateApplier = func(a *updates.Applier) { a.Clock = &instantClock{now: time.Unix(1_700_000_000, 0)} }
	f.h = f.build(t)
	local := f.add(t)
	f.awaitFirstProbe(t, local.ID)

	swarm := f.srv.NewEnvironment(t, "Swarm")
	cfg := swarmManagerEngine(plainService("web", 'a'), plainService("api", 'a'))
	cfg.TasksFollowWrites, cfg.SwarmNodeID, cfg.SwarmClusterID = true, "node-1", "cluster-1"
	f.connect(t, swarm, cfg, &wire.Facts{
		EngineID: "ENG:mgr-1", Hostname: "mgr-1", EngineReachable: true, EngineVersion: "29.1.0",
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-1", ClusterID: "cluster-1", Control: true},
	})
	f.whenState(t, swarm.ID, "online")
	for _, id := range []string{local.ID, swarm.ID} {
		if rec := f.checkNow(t, envBody{ID: id}); rec.code != http.StatusOK {
			t.Fatalf("check now %s: %d %s", id, rec.code, rec.body)
		}
	}

	rec := f.bulkRun(t,
		bulkItem(swarm.ID, "service", "web", map[string]string{"web": updDigest('b')}),
		bulkItem(local.ID, "container", "app", map[string]string{"app": updDigest('b')}),
		bulkItem(swarm.ID, "service", "api", nil),
	)
	if rec.code != http.StatusOK {
		t.Fatalf("bulk update: %d %s", rec.code, rec.body)
	}
	ans := decode[bulkAnswer](t, rec)
	if len(ans.Jobs) != 2 || len(ans.Skipped) != 0 || ans.BulkID == "" {
		t.Fatalf("answer = %+v, want two jobs, nothing skipped and a bulk ID", ans)
	}

	swarmJob := f.awaitUpdate(t, ans.jobOf(t, swarm.ID))
	localJob := f.awaitUpdate(t, ans.jobOf(t, local.ID))
	for name, job := range map[string]jobs.Job{"swarm": swarmJob, "local": localJob} {
		if job.State != jobs.JobDone || job.Kind != updates.JobKind || job.Meta[updates.MetaBulk] != ans.BulkID {
			t.Errorf("%s job = %s %s bulk %q: %+v, want a done update job of the bulk", name, job.State, job.Kind, job.Meta[updates.MetaBulk], job.Items)
		}
	}
	if s := bulkItemStates(swarmJob); s["i0"] != "done" || s["i1"] != "done" || s["summary"] != "done" {
		t.Errorf("swarm job items = %v, want both services and the summary done", s)
	}
	if s := bulkItemStates(localJob); s["i0"] != "done" || s["summary"] != "done" {
		t.Errorf("local job items = %v, want the container and the summary done", s)
	}

	// The runs of the selection are done, one after the other, each tied to its own environment's job.
	runs := f.runsOfBulk(t, ans.BulkID)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v, want three", runs)
	}
	for _, run := range runs {
		want := swarmJob.ID
		if run.EnvID == local.ID {
			want = localJob.ID
		}
		if run.State != updates.RunDone || run.JobID != want || len(run.Items) != 1 || run.Items[0].ToDigest != updDigest('b') || run.Items[0].FromDigest != updDigest('a') {
			t.Errorf("run = %+v, want a done run of one item a to b in job %s", run, want)
		}
	}

	// Every audit row of the selection carries the bulk ID, and each environment has one summary row.
	rows := f.auditRows(t, "image.updated")
	if len(rows) != 3 {
		t.Fatalf("image.updated rows = %+v, want three", rows)
	}
	for _, r := range rows {
		if r.Details["bulk"] != ans.BulkID {
			t.Errorf("row %+v does not carry the bulk ID", r)
		}
	}
	sums := f.auditRows(t, "image.bulk_updated")
	if len(sums) != 2 {
		t.Fatalf("image.bulk_updated rows = %+v, want one for each environment", sums)
	}
	got := map[string]string{}
	for _, r := range sums {
		if r.Details["bulk"] != ans.BulkID || r.Details["rolled_back"] != "0" || r.Details["skipped"] != "0" || r.Result != "success" {
			t.Errorf("summary row = %+v", r)
		}
		got[r.EnvID] = r.Details["done"] + " of " + r.Details["total"]
	}
	if got[swarm.ID] != "2 of 2" || got[local.ID] != "1 of 1" {
		t.Errorf("summaries = %v, want 2 of 2 in the swarm and 1 of 1 locally", got)
	}
	for _, id := range []string{swarm.ID, local.ID} {
		if h, held := f.stackLocks.Held(id); held {
			t.Errorf("the lock of %s is still held by %+v", id, h)
		}
	}
}
