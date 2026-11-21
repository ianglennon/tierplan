// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// A removal lets go of the stack's hostnames only once nothing of the stack is left on the engine, and before the
// definition is deleted (plan 24-25, LBL-06, D-17, PROH-24-10).

// removeIngress is the Ingress a removal sees: it holds hostnames for one stack and remembers every call, in order,
// beside the engine writes that had been made when the release came.
type removeIngress struct {
	stackops.Ingress // every other method is never called by a removal

	rig        *swarmRig
	mu         sync.Mutex
	hostnames  []string
	releaseErr error
	events     []string
	writesAt   int // engine writes made when the release was called
	actor      string
}

func (o *removeIngress) ReleaseStack(_ context.Context, _, stackID, actor string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "release:"+stackID)
	o.writesAt = len(o.rig.fake.Writes())
	o.actor = actor
	return o.releaseErr
}

func (o *removeIngress) HeldRoutes(context.Context, string, string) ([]ingress.Route, error) {
	var out []ingress.Route
	for _, h := range o.hostnames {
		out = append(out, ingress.Route{Hostname: h})
	}
	return out, nil
}

func (o *removeIngress) record(ev string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
}

func (o *removeIngress) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

// orderedStore is a removeStore that tells the ingress when the definition was deleted, so the order shows.
type orderedStore struct {
	removeStore
	ing *removeIngress
}

func (s *orderedStore) DeleteStackWithAutomations(ctx context.Context, id string) error {
	s.ing.record("delete:" + id)
	return s.removeStore.DeleteStackWithAutomations(ctx, id)
}

// ingressRemoveRig is the removal rig with an ingress that holds hostnames for the stack.
func ingressRemoveRig(t *testing.T, tune func(*dockertest.FakeConfig), hostnames ...string) (*swarmRig, *removeIngress, *orderedStore) {
	t.Helper()
	rig := removeRig(t, tune)
	ing := &removeIngress{rig: rig, hostnames: hostnames}
	rig.deps.Ingress = ing
	return rig, ing, &orderedStore{ing: ing}
}

func TestRemoveReleasesClaimsAfterEngine(t *testing.T) {
	t.Parallel()
	rig, ing, store := ingressRemoveRig(t, nil, "app.example.com")

	res, job := rig.remove(t, stackops.RemoveInput{Store: store, ClaimActor: "user:test"})

	if !res.OK() || job.State != jobs.JobDone || !res.DefinitionDeleted {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	events := ing.seen()
	if want := []string{"release:" + rmID, "delete:" + rmID}; !slices.Equal(events, want) {
		t.Fatalf("calls = %v, want the release and then the delete of the definition %v", events, want)
	}
	// Nothing was written to the engine after the release, and everything the stack owned had been removed before it.
	writes := rig.fake.Writes()
	if ing.writesAt != len(writes) {
		t.Errorf("the release came after %d of %d engine writes, want it after all of them", ing.writesAt, len(writes))
	}
	for _, want := range []string{"DELETE /services/svc-web", "DELETE /services/svc-api", "DELETE /networks/net-front", "DELETE /networks/net-back", "DELETE /secrets/sec-tok"} {
		if !slices.Contains(writes[:ing.writesAt], want) {
			t.Errorf("writes before the release = %v, missing %s", writes[:ing.writesAt], want)
		}
	}
	if ing.actor != "user:test" {
		t.Errorf("the release was recorded as %q, want user:test", ing.actor)
	}
}

func TestRemoveKeepsClaimsOnFailure(t *testing.T) {
	t.Parallel()
	rig, ing, store := ingressRemoveRig(t, func(c *dockertest.FakeConfig) {
		c.RemoveErrors = map[string]string{"rm_back": "rpc error: the manager could not remove rm_back"}
	}, "app.example.com")

	res, job := rig.remove(t, stackops.RemoveInput{Store: store})

	if res.Code != stackops.CodeRemoveFailed {
		t.Fatalf("remove = %+v, want remove_failed", res)
	}
	if got := ing.seen(); len(got) != 0 {
		t.Errorf("calls = %v, want no release and no delete while a network stays", got)
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job state = %s, want partial", job.State)
	}
}

func TestRemoveKeepsClaimsWhenTheTasksStay(t *testing.T) {
	t.Parallel()
	rig, ing, store := ingressRemoveRig(t, func(c *dockertest.FakeConfig) {
		c.Tasks = []swarm.Task{
			{ID: "t1", ServiceID: "svc-web", DesiredState: swarm.TaskStateRemove, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: rmOwned()}},
		}
	}, "app.example.com")
	clk := newStepClock()
	rig.deps.Clock, rig.deps.ConvergeTimeout = clk, time.Minute
	rr := rig.startRemove(t, stackops.RemoveInput{Store: store})
	rr.until(t, "the wait to say a task is left", func(items map[string]jobs.Item) bool {
		return items[stackops.ItemWait].Result["left"] == "1"
	})
	clk.Tick(t, 2*time.Minute)
	res, _ := rr.finish(t)

	if res.Code != stackops.CodeTasksRemain {
		t.Fatalf("remove = %+v, want tasks_remain", res)
	}
	if got := ing.seen(); len(got) != 0 {
		t.Errorf("calls = %v, want no release while tasks may still answer", got)
	}
}

func TestRemoveReleaseFailureFailsTheDefinitionAndKeepsIt(t *testing.T) {
	t.Parallel()
	rig, ing, store := ingressRemoveRig(t, nil, "app.example.com")
	ing.releaseErr = errors.New("database is locked")

	res, job := rig.remove(t, stackops.RemoveInput{Store: store})

	if res.Code != stackops.CodeIngressReleaseFailed || res.DefinitionDeleted {
		t.Fatalf("remove = %+v, want ingress_release_failed and the definition kept", res)
	}
	if got := itemStates(job)[stackops.ItemDefinition]; got != "failed:ingress_release_failed" {
		t.Errorf("definition item = %q, want failed:ingress_release_failed", got)
	}
	if store.wasDeleted() {
		t.Errorf("the definition was deleted although the release failed")
	}

	// A second removal, with the store answering, finishes it: the engine has nothing left, so only the release runs.
	ing.mu.Lock()
	ing.releaseErr = nil
	ing.mu.Unlock()
	res, job = rig.remove(t, stackops.RemoveInput{Store: store})
	if !res.OK() || !res.DefinitionDeleted || job.State != jobs.JobDone {
		t.Fatalf("retry = %+v %v, want it done", res, itemStates(job))
	}
}
