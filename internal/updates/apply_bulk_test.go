// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Updating many workloads of one environment in one job (plan 12-30, IMG-06, D-11). Docker is a fake swarm manager behind the real
// monitor, aggregator and cluster reader; the registry is a scripted Heads; the applier, its run store, the job hub, the operation lock
// and the audit log are the real ones.

// bulkRequest is the update of the services as an Operator asks for it, each confirmed at the digest.
func (r *applyRig) bulkRequest(confirmed string, names ...string) updates.BulkRequest {
	req := updates.BulkRequest{
		EnvID: r.envID, EnvName: "Local", BulkID: "bulk-1",
		Actor: r.request("x", "").Actor, Owner: "user:user-1",
	}
	for _, n := range names {
		it := updates.BulkItem{Kind: "service", Name: n}
		if confirmed != "" {
			it.Expect = map[string]string{n: confirmed}
		}
		req.Items = append(req.Items, it)
	}
	return req
}

// runBulk starts a bulk update and waits for its job to end.
func (r *applyRig) runBulk(t *testing.T, req updates.BulkRequest) (jobs.Job, []updates.BulkSkip) {
	t.Helper()
	id, skipped, err := r.applier.StartBulk(testsupport.Context(t), req)
	if err != nil {
		t.Fatalf("StartBulk: %v", err)
	}
	if id == "" {
		t.Fatalf("StartBulk started no job; skipped %+v", skipped)
	}
	job, err := r.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatalf("waiting for the bulk job: %v", err)
	}
	return job, skipped
}

func (r *applyRig) bulkThree() {
	for _, n := range []string{"web", "api", "db"} {
		r.offer(n, dg('b'))
	}
}

// The items are updated in the order they were named, one at a time, each with its run done and its audit entry carrying the bulk ID;
// the job ends with a summary, and the lock is free by then (plan 12-30, D-11).
func TestBulkRunsItemsInOrderUnderOneLock(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'), swarmService("api", 'a'), swarmService("db", 'a'))
	r.bulkThree()

	job, skipped := r.runBulk(t, r.bulkRequest(dg('b'), "db", "web", "api"))
	if len(skipped) != 0 || job.State != jobs.JobDone || job.Meta[updates.MetaBulk] != "bulk-1" || job.Meta[updates.MetaKind] != "bulk" {
		t.Fatalf("job = %s meta %v skipped %+v, want a done bulk job and nothing skipped", job.State, job.Meta, skipped)
	}
	var written []string
	for _, u := range r.fake.ServiceUpdates() {
		written = append(written, u.Spec.Name)
	}
	if len(written) != 3 || written[0] != "db" || written[1] != "web" || written[2] != "api" {
		t.Errorf("services written = %v, want db, web, api: the order they were named", written)
	}
	for _, it := range job.Items {
		if it.Parent == "" && it.State != jobs.ItemDone {
			t.Errorf("item %s = %s, want done", it.ID, it.State)
		}
	}
	if rows := r.audits(t, "image.updated"); len(rows) != 3 || rows[0]["bulk"] != "bulk-1" || rows[2]["bulk"] != "bulk-1" {
		t.Errorf("image.updated rows = %+v, want three with the bulk ID", rows)
	}
	if rows := r.audits(t, "image.bulk_updated"); len(rows) != 1 || rows[0]["done"] != "3" || rows[0]["total"] != "3" || rows[0]["rolled_back"] != "0" || rows[0]["skipped"] != "0" || rows[0]["_result"] != "success" {
		t.Errorf("image.bulk_updated rows = %+v, want one: 3 of 3", rows)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after the job ended")
	}
	if runs, err := r.runs.Running(context.Background(), r.envID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none left queued or running", runs, err)
	}
}

// A held lock skips every item as locked and starts nothing: no job, no run, no write (plan 12-30, D-10).
func TestBulkLockedSkipsEveryItem(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'), swarmService("api", 'a'))
	r.offer("web", dg('b'))
	r.offer("api", dg('b'))
	if _, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{StackID: "stk-1", StackName: "shop", Kind: "deploy", By: "ada", Started: time.Now()}); !ok {
		t.Fatal("could not take the lock")
	}

	id, skipped, err := r.applier.StartBulk(testsupport.Context(t), r.bulkRequest(dg('b'), "web", "api"))
	if err != nil || id != "" {
		t.Fatalf("StartBulk = %q %v, want no job and no error", id, err)
	}
	if len(skipped) != 2 || skipped[0].Reason != updates.BulkLocked || skipped[1].Reason != updates.BulkLocked {
		t.Errorf("skipped = %+v, want both locked", skipped)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("services written = %d, want none", n)
	}
	if runs, err := r.runs.Running(context.Background(), r.envID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none", runs, err)
	}
	if h, held := r.locks.Held(r.envID); !held || h.Kind != "deploy" {
		t.Errorf("holder = %+v %v, want the deploy still holding it", h, held)
	}
}

// What a bulk update does not update is named and the rest goes on: a service that is not there, one named twice (once), a kind it does
// not know and the agent (plan 12-30).
func TestBulkNamesWhatItDoesNotTake(t *testing.T) {
	t.Parallel()
	agent := dockertest.FakeService{
		ID: "svc-agent", Name: "tierplan-agent", Mode: "global", Version: 1, Image: "registry.test/team/agent:1@" + dg('a'),
	}
	r := newApplyRig(t, nil, swarmService("web", 'a'), agent)
	r.offer("web", dg('b'))

	req := r.bulkRequest("", "web", "nobody", "web", "tierplan-agent")
	req.Items = append(req.Items, updates.BulkItem{Kind: "stack", Name: "shop"})
	job, skipped := r.runBulk(t, req)
	got := map[string]updates.BulkSkip{}
	for _, s := range skipped {
		got[s.Kind+"/"+s.Name] = s
	}
	if len(skipped) != 3 || got["service/nobody"].Code != updates.CodeWorkloadGone || got["stack/shop"].Code != updates.CodeUnsupported || got["service/tierplan-agent"].Code != updates.NotUpdatableAgent || got["service/tierplan-agent"].Reason != updates.BulkNotUpdatable {
		t.Errorf("skipped = %+v, want the missing service, the agent and the stack named", skipped)
	}
	if job.State != jobs.JobDone || len(r.fake.ServiceUpdates()) != 1 {
		t.Errorf("job = %s with %d writes, want web alone, updated once", job.State, len(r.fake.ServiceUpdates()))
	}
	if rows := r.audits(t, "image.bulk_updated"); len(rows) != 1 || rows[0]["done"] != "1" || rows[0]["total"] != "4" || rows[0]["skipped"] != "3" {
		t.Errorf("image.bulk_updated rows = %+v, want 1 of 4 with 3 skipped", rows)
	}
}

// An item is judged again at its turn from what the check found, never from the queue it waits in: a registry that cannot be asked
// when the turn comes skips the item as check_failed, and does not update it to a digest an earlier check found (plan 12-30, D-05).
func TestBulkItemWhoseCheckFailsIsSkipped(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'), swarmService("api", 'a'))
	r.offer("web", dg('b'))
	r.heads.set("registry.test/team/api:1", answer{err: errors.New("connection refused")})

	job, _ := r.runBulk(t, r.bulkRequest("", "web", "api"))
	var api jobs.Item
	for _, it := range job.Items {
		if it.ID == "i1" {
			api = it
		}
	}
	if api.State != jobs.ItemSkipped || api.Code != updates.CodeCheckFailed {
		t.Errorf("api's item = %s %q, want skipped as check_failed", api.State, api.Code)
	}
	for _, u := range r.fake.ServiceUpdates() {
		if u.Spec.Name == "api" {
			t.Errorf("api was written although it could not be checked")
		}
	}
	if rows := r.audits(t, "image.bulk_updated"); len(rows) != 1 || rows[0]["done"] != "1" || rows[0]["skipped"] != "1" {
		t.Errorf("image.bulk_updated rows = %+v, want 1 of 2 with 1 skipped", rows)
	}
}

// A rolled back version is never tried again by a bulk update, even when it was not known to have failed when the selection was made
// (plan 12-30, D-14): the item is skipped as previously_failed at its turn.
func TestBulkNeverRetriesAVersionThatFailed(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	r.failingTasks("web", dg('b'), "No such image")
	if job := r.run(t, r.request("web", dg('b'))); r.runRow(t, job).State != updates.RunRolledBack {
		t.Fatal("the setup update did not roll back")
	}
	before := len(r.fake.ServiceUpdates())

	job, _ := r.runBulk(t, r.bulkRequest("", "web"))
	var web jobs.Item
	for _, it := range job.Items {
		if it.ID == "i0" {
			web = it
		}
	}
	if web.State != jobs.ItemSkipped || web.Code != updates.CodePreviouslyFail {
		t.Errorf("web's item = %s %q, want skipped as previously_failed", web.State, web.Code)
	}
	if n := len(r.fake.ServiceUpdates()); n != before {
		t.Errorf("services written = %d after the bulk update, want the %d before it", n, before)
	}
}

// Items waiting in a bulk update are queued runs, and the one at its turn is running, so recovery after a restart finds them (plan 12-30).
func TestBulkQueuesRunsBeforeTheirTurn(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	r := newApplyRig(t, func(c *dockertest.FakeConfig) { c.PullHold = hold }, swarmService("web", 'a'), swarmService("api", 'a'))
	r.offer("web", dg('b'))
	r.offer("api", dg('b'))
	id, _, err := r.applier.StartBulk(testsupport.Context(t), r.bulkRequest("", "web", "api"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	// The pull reaches the engine only after web's run has begun, and it is held there.
	err = r.fake.Await(testsupport.Context(t), func() bool {
		return slices.ContainsFunc(r.fake.Writes(), func(w string) bool { return strings.HasPrefix(w, "POST /images/create") })
	})
	if err != nil {
		t.Fatalf("waiting for web's pull to reach the engine: %v", err)
	}
	runs, err := r.runs.Running(context.Background(), r.envID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]updates.RunState{}
	for _, run := range runs {
		states[run.Workload] = run.State
		if run.BulkID != "bulk-1" || run.JobID != id {
			t.Errorf("run %+v is not tied to the bulk and its job %s", run, id)
		}
	}
	if states["web"] != updates.RunRunning || states["api"] != updates.RunQueued {
		t.Errorf("runs = %v, want web running and api queued", states)
	}
	close(hold)
	if job, err := r.hub.Wait(testsupport.Context(t), id); err != nil || job.State != jobs.JobDone {
		t.Fatalf("job = %v %v, want done", job.State, err)
	}
}

// A service of a stack TierPlan manages is deployed as a revision of the stack, which needs stacks.deploy: without it the item is
// skipped no_permission, and with it but with no way to deploy a stack it is not updatable here, never updated as a plain service
// (plan 12-30, D-06, T-12-89).
func TestBulkStackServiceNeedsTheStackDeployPermission(t *testing.T) {
	t.Parallel()
	managed := swarmService("managed", 'a')
	managed.Spec.Annotations.Labels = map[string]string{stacks.LabelStackID: "stack-1"}
	managed.Labels = managed.Spec.Annotations.Labels
	r := newApplyRig(t, nil, managed, swarmService("web", 'a'))
	r.applier.ManagedStack = func(_ context.Context, _, id string) (bool, error) { return id == "stack-1", nil }
	r.offer("web", dg('b'))
	r.offer("managed", dg('b'))

	for name, tc := range map[string]struct {
		deploy       bool
		reason, code string
	}{
		"without stacks.deploy":       {deploy: false, reason: updates.BulkNoPermission, code: updates.NotUpdatableManaged},
		"with it but no stack engine": {deploy: true, reason: updates.BulkNotUpdatable, code: updates.NotUpdatableManaged},
	} {
		req := r.bulkRequest("", "managed", "web")
		req.StackDeploy = tc.deploy
		job, skipped := r.runBulk(t, req)
		if len(skipped) != 1 || skipped[0].Name != "managed" || skipped[0].Reason != tc.reason || skipped[0].Code != tc.code {
			t.Errorf("%s: skipped = %+v, want managed skipped %s %s", name, skipped, tc.reason, tc.code)
		}
		if job.State != jobs.JobDone {
			t.Errorf("%s: job = %s, want done for web alone", name, job.State)
		}
		r.offer("web", dg('b'))
	}
	for _, u := range r.fake.ServiceUpdates() {
		if u.Spec.Name == "managed" {
			t.Errorf("a service of a managed stack was written as a plain one")
		}
	}
}
