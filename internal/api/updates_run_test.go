// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Updating a service in one click (plan 12-15, IMG-06, D-05, D-07, D-10, D-13). Docker is a fake daemon behind the real monitor,
// aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the job hub, the operation lock,
// the router and the permission checks are the real ones.

// plainSpec is the spec of a service the tests update: replicas, environment, labels and an update configuration the update
// must keep, running digest a of registry.test/team/<name>:1.
func plainSpec(name string, running byte) swarm.ServiceSpec {
	replicas := uint64(3)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{"team": "core", "tier": "web"}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: fmt.Sprintf("registry.test/team/%s:1@%s", name, updDigest(running)), Env: []string{"MODE=prod", "WORKERS=4"},
		}},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{Parallelism: 2, Order: swarm.UpdateOrderStartFirst, FailureAction: swarm.UpdateFailureActionRollback},
	}
}

// plainService is a service made of plainSpec.
func plainService(name string, running byte) dockertest.FakeService {
	spec := plainSpec(name, running)
	return dockertest.FakeService{
		ID: "svc-" + name, Name: name, Mode: "replicated", Running: 3, Desired: 3, Version: 1, Image: spec.TaskTemplate.ContainerSpec.Image,
		Labels: spec.Annotations.Labels, Spec: &spec,
	}
}

// updateEnvOf is an environment of one swarm manager whose tasks follow the writes made to it.
func updateEnvOf(t *testing.T, heads *scriptedHeads, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	cfg.TasksFollowWrites = true
	cfg.SwarmNodeID = "node-1"
	if tune != nil {
		tune(&cfg)
	}
	return updatesEnvOf(t, heads, cfg)
}

type updateRunAnswer struct {
	JobID string `json:"jobId"`
}

func updateRunBody(name string, expect map[string]string) string {
	body := map[string]any{"kind": "service", "name": name}
	if expect != nil {
		body["expect"] = expect
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// runUpdate asks for the update of a service.
func (f *envFix) runUpdate(t *testing.T, e envBody, name string, expect map[string]string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/updates/run", updateRunBody(name, expect))
}

// awaitUpdate waits for the update's job to end and answers it as the hub holds it.
func (f *envFix) awaitUpdate(t *testing.T, jobID string) jobs.Job {
	t.Helper()
	job, err := f.jobHub.Wait(testsupport.Context(t), jobID)
	if err != nil {
		t.Fatalf("waiting for the update job: %v", err)
	}
	return job
}

// runOf is the run row the update's job recorded.
func (f *envFix) runOf(t *testing.T, job jobs.Job) updates.Run {
	t.Helper()
	run, err := (updates.RunStore{Q: f.st.Q, W: f.st.Write}).Get(context.Background(), job.Meta[updates.MetaRunID])
	if err != nil {
		t.Fatalf("read the run of job %s: %v", job.ID, err)
	}
	return run
}

// stepStates are the states of a job's steps by ID.
func stepStates(job jobs.Job) map[string]string {
	out := map[string]string{}
	for _, it := range job.Items {
		out[it.ID] = it.State
	}
	return out
}

// jsonOf encodes a spec the way the fake decodes it, to compare two of them.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The tracer (plan 12-15, IMG-06): an Operator updates a plain service to the digest the registry has, the job reports it done,
// the run is on record and one audit row says who updated what.
func TestPlainServiceUpdateEndToEnd(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	rec := f.runUpdate(t, e, "web", map[string]string{"web": updDigest('b')})
	if rec.code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.code, rec.body)
	}
	job := f.awaitUpdate(t, decode[updateRunAnswer](t, rec).JobID)
	if job.State != jobs.JobDone || job.Kind != updates.JobKind || job.EnvID != e.ID {
		t.Fatalf("job = %s %s %s, want a done update job of this environment: %+v", job.State, job.Kind, job.EnvID, job.Items)
	}

	sent := f.fake.ServiceUpdates()
	if len(sent) != 1 {
		t.Fatalf("service updates = %d, want 1", len(sent))
	}
	want := plainSpec("web", 'a')
	want.TaskTemplate.ContainerSpec.Image = "registry.test/team/web:1@" + updDigest('b')
	if got := sent[0].Spec; jsonOf(t, got) != jsonOf(t, want) {
		t.Errorf("the spec sent = %s\nwant the old spec with only the image changed: %s", jsonOf(t, got), jsonOf(t, want))
	}
	if sent[0].Version != "1" {
		t.Errorf("the update carried version %q, want the version it read, 1", sent[0].Version)
	}

	run := f.runOf(t, job)
	if run.State != updates.RunDone || run.Kind != "service" || run.Workload != "web" || run.EnvID != e.ID || run.JobID != job.ID ||
		len(run.Items) != 1 || run.Items[0].FromDigest != updDigest('a') || run.Items[0].ToDigest != updDigest('b') || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want a done run of web from a to b, on this environment and job", run)
	}
	if s := stepStates(job); !reflect.DeepEqual(s, map[string]string{"check": "done", "pull": "done", "update": "done", "wait": "done"}) {
		t.Errorf("steps = %v, want every step done", s)
	}
	rows := f.auditRows(t, "image.updated")
	if len(rows) != 1 || rows[0].EnvID != e.ID || rows[0].Result != "success" || rows[0].Details["workload"] != "web" || rows[0].Details["kind"] != "service" ||
		rows[0].Details["from_digest"] != updDigest('a') || rows[0].Details["to_digest"] != updDigest('b') || rows[0].Details["image"] != "registry.test/team/web:1" {
		t.Errorf("audit rows = %+v, want one naming the workload, the image and both digests", rows)
	}
	if rows := f.auditRows(t, "image.update_failed"); len(rows) != 0 {
		t.Errorf("failure rows = %+v, want none", rows)
	}
}
