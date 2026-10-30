// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// Remove of a swarm stack runs against the fake swarm manager, over a swarm that already holds the stack's
// objects beside objects that are not its own.

const rmID = "stack-rm"

// rmOwned are the labels an object of the stack "rm" carries.
func rmOwned() map[string]string {
	return map[string]string{"com.docker.stack.namespace": "rm", "coaming.stack.id": rmID}
}

// rmOther are the labels of an object of another stack of the same swarm.
func rmOther() map[string]string {
	return map[string]string{"com.docker.stack.namespace": "other", "coaming.stack.id": "stack-other"}
}

// removeStore plays the stored side of a removal and remembers what it was asked.
type removeStore struct {
	mu      sync.Mutex
	deleted []string
	stopped map[string]bool
}

func (s *removeStore) DeleteStackWithAutomations(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *removeStore) SetStopped(_ context.Context, id string, stopped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped == nil {
		s.stopped = map[string]bool{}
	}
	s.stopped[id] = stopped
	return nil
}

func (s *removeStore) wasDeleted() bool { return s.wasDeletedAs(rmID) }

func (s *removeStore) wasDeletedAs(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.deleted, id)
}

// removeRig is a swarm that runs the stack "rm" (two services, two networks, a secret, a config, two volumes)
// beside things that are not its own: another stack's service, network and secret, an external network, secret
// and volume, and a volume of another stack. tune changes the fake's configuration after that.
func removeRig(t *testing.T, tune func(*dockertest.FakeConfig)) *swarmRig {
	t.Helper()
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{
			{ID: "svc-web", Name: "rm_web", Mode: "replicated", Image: "nginx:1.27", Version: 3, Labels: rmOwned(), Desired: 2, Running: 2},
			{ID: "svc-api", Name: "rm_api", Mode: "replicated", Image: "nginx:1.28", Version: 2, Labels: rmOwned(), Desired: 1, Running: 1},
			{ID: "svc-other", Name: "other_web", Mode: "replicated", Image: "nginx:1.28", Version: 2, Labels: rmOther(), Desired: 1, Running: 1},
		}
		c.Networks = []dockertest.FakeNetwork{
			{ID: "net-front", Name: "rm_front", Driver: "overlay", Scope: "swarm", Labels: rmOwned()},
			{ID: "net-back", Name: "rm_back", Driver: "overlay", Scope: "swarm", Labels: rmOwned()},
			{ID: "net-shared", Name: "shared", Driver: "overlay", Scope: "swarm"},
			{ID: "net-other", Name: "other_net", Driver: "overlay", Scope: "swarm", Labels: rmOther()},
		}
		c.Secrets = []dockertest.FakeSecret{
			{ID: "sec-tok", Name: "rm_tok_abc", Labels: rmOwned()},
			{ID: "sec-ext", Name: "extsecret"},
			{ID: "sec-other", Name: "other_tok_abc", Labels: rmOther()},
		}
		c.Configs = []dockertest.FakeConfigObject{
			{ID: "cfg-site", Name: "rm_site_abc", Labels: rmOwned()},
			{ID: "cfg-ext", Name: "extconfig"},
		}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "rm_data", Driver: "local", Labels: rmOwned()},
			{Name: "rm_cache", Driver: "local", Labels: rmOwned()},
			{Name: "shared_vol", Driver: "local"},
			{Name: "other_data", Driver: "local", Labels: rmOther()},
		}
		if tune != nil {
			tune(c)
		}
	})
}

// rmStack is the stack "rm" as the stack store reads it.
func rmStack() stackops.Stack {
	return stackops.Stack{ID: rmID, Name: "rm", Target: stacks.TargetSwarm}
}

// runningRemoval is a removal in flight: the caller moves the fake and the clock on and ends it with finish.
type runningRemoval struct {
	rig   *swarmRig
	job   jobs.Job
	done  chan struct{}
	res   *stackops.RemoveResult
	items map[string]jobs.Item
	ch    <-chan jobs.Event
}

// startRemove starts Remove as a job and follows it.
func (r *swarmRig) startRemove(t *testing.T, in stackops.RemoveInput) *runningRemoval {
	t.Helper()
	if in.Caller.Principal == nil {
		in.Caller = stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"}
	}
	in.EnvID = r.envID
	if in.Stack.ID == "" {
		in.Stack = rmStack()
	}
	rr := &runningRemoval{rig: r, done: make(chan struct{}), res: new(stackops.RemoveResult), items: map[string]jobs.Item{}}
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_remove", EnvID: r.envID, Owner: "user:test",
		Items: stackops.RemoveItemSpecs(in.Stack.Target, in.Volumes, in.KeepDefinition),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		defer close(rr.done)
		*rr.res = r.deps.Remove(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ch, cancel, err := r.hub.Subscribe(testsupport.Context(t), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	rr.job, rr.ch = job, ch
	for _, it := range snap.Items {
		rr.items[it.ID] = it
	}
	return rr
}

// until reads the job's events until the items satisfy cond.
func (rr *runningRemoval) until(t *testing.T, what string, cond func(items map[string]jobs.Item) bool) {
	t.Helper()
	ctx := testsupport.Context(t)
	for !cond(rr.items) {
		select {
		case ev, ok := <-rr.ch:
			if !ok {
				t.Fatalf("the job ended before %s; items: %v", what, rr.states())
			}
			if ev.Item != nil {
				rr.items[ev.Item.ID] = *ev.Item
			}
			if ev.Job != nil {
				for _, it := range ev.Job.Items {
					rr.items[it.ID] = it
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s; items: %v", what, rr.states())
		}
	}
}

func (rr *runningRemoval) states() map[string]string {
	out := map[string]string{}
	for id, it := range rr.items {
		out[id] = it.State
		if it.Code != "" {
			out[id] += ":" + it.Code
		}
	}
	return out
}

// finish waits for the removal to end and returns its result and the job as it ended.
func (rr *runningRemoval) finish(t *testing.T) (stackops.RemoveResult, jobs.Job) {
	t.Helper()
	ended, err := rr.rig.hub.Wait(testsupport.Context(t), rr.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the removal function to return", rr.done)
	return *rr.res, ended
}

// remove runs Remove as a job to its end.
func (r *swarmRig) remove(t *testing.T, in stackops.RemoveInput) (stackops.RemoveResult, jobs.Job) {
	t.Helper()
	return r.startRemove(t, in).finish(t)
}

// writesTo lists the writes the fake took whose text starts with prefix.
func writesTo(rig *swarmRig, prefix string) []string {
	var out []string
	for _, w := range rig.fake.Writes() {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}

func TestPreviewListsWhatTheStackOwnsAndNamesTheExternalsTheFileUses(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	decl := stackops.Declared{
		Networks: []stackops.DeclaredObject{{Key: "front"}, {Key: "outer", Name: "shared", External: true}},
		Secrets:  []stackops.DeclaredObject{{Key: "ext", Name: "extsecret", External: true}},
		Volumes:  []stackops.DeclaredObject{{Key: "keep", Name: "shared_vol", External: true}},
	}

	got, err := rig.deps.Preview(testsupport.Context(t), rig.envID, rmStack(), decl)

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rm_api", "rm_web"}; !slices.Equal(got.Services, want) {
		t.Errorf("services = %v, want %v", got.Services, want)
	}
	if want := []string{"rm_back", "rm_front"}; !slices.Equal(got.Networks, want) {
		t.Errorf("networks = %v, want only the stack's own %v", got.Networks, want)
	}
	if want := []string{"rm_tok_abc"}; !slices.Equal(got.Secrets, want) {
		t.Errorf("secrets = %v, want %v", got.Secrets, want)
	}
	if want := []string{"rm_site_abc"}; !slices.Equal(got.Configs, want) {
		t.Errorf("configs = %v, want %v", got.Configs, want)
	}
	var vols []string
	for _, v := range got.Volumes {
		vols = append(vols, v.Name)
		if v.NodeID == "" || v.NodeName == "" {
			t.Errorf("volume %+v names no node", v)
		}
	}
	if want := []string{"rm_cache", "rm_data"}; !slices.Equal(vols, want) {
		t.Errorf("volumes = %v, want only the stack's own %v", vols, want)
	}
	if want := []string{"extsecret", "shared", "shared_vol"}; !slices.Equal(got.Externals, want) {
		t.Errorf("externals = %v, want %v", got.Externals, want)
	}
}

func TestRemoveDeletesTheServicesNetworksSecretsAndConfigsTheStackOwnsAndNothingElse(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	store := &removeStore{}

	res, job := rig.remove(t, stackops.RemoveInput{Store: store})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if res.Services != 2 || res.Networks != 2 || res.Secrets != 1 || res.Configs != 1 || !res.DefinitionDeleted {
		t.Errorf("result = %+v, want 2 services, 2 networks, 1 secret, 1 config and the definition", res)
	}
	writes := rig.fake.Writes()
	for _, want := range []string{
		"DELETE /services/svc-web", "DELETE /services/svc-api", "DELETE /networks/net-front", "DELETE /networks/net-back",
		"DELETE /secrets/sec-tok", "DELETE /configs/cfg-site",
	} {
		if !slices.Contains(writes, want) {
			t.Errorf("writes = %v, want %q", writes, want)
		}
	}
	for _, w := range writes {
		for _, keep := range []string{"svc-other", "net-shared", "net-other", "sec-ext", "sec-other", "cfg-ext", "/volumes/"} {
			if strings.Contains(w, keep) {
				t.Errorf("write %q touched %s, which is not the stack's to remove", w, keep)
			}
		}
	}
	if !store.wasDeleted() {
		t.Errorf("the definition was not deleted")
	}
	for id, want := range map[string]string{
		stackops.ItemCheck: "done", stackops.ItemServices: "done", stackops.ItemWait: "done", stackops.ItemRemoveNetworks: "done",
		stackops.ItemRemoveObjects: "done", stackops.ItemDefinition: "done",
	} {
		if got := itemStates(job)[id]; got != want {
			t.Errorf("item %s = %q, want %q (%v)", id, got, want, itemStates(job))
		}
	}
}

func TestRemoveDeletesNetworksOnlyAfterTheServicesAndTheirTasksAreGone(t *testing.T) {
	t.Parallel()
	var base dockertest.FakeConfig
	rig := removeRig(t, func(c *dockertest.FakeConfig) {
		// The swarm keeps two tasks of the removed services running for a while, each with the labels its service had.
		c.Tasks = []swarm.Task{
			{ID: "t1", ServiceID: "svc-web", DesiredState: swarm.TaskStateRemove, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: rmOwned()}},
			{ID: "t2", ServiceID: "svc-api", DesiredState: swarm.TaskStateRemove, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: rmOwned()}},
			{ID: "t3", ServiceID: "svc-other", DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: rmOther()}},
		}
		base = *c
	})
	clk := newStepClock()
	rig.deps.Clock = clk
	store := &removeStore{}
	rr := rig.startRemove(t, stackops.RemoveInput{Store: store})

	rr.until(t, "the wait to say two tasks are left", func(items map[string]jobs.Item) bool {
		return items[stackops.ItemWait].Result["left"] == "2"
	})
	if n := len(writesTo(rig, "DELETE /networks/")) + len(writesTo(rig, "DELETE /secrets/")); n != 0 {
		t.Fatalf("writes = %v: a network or a secret was removed while the tasks were still stopping", rig.fake.Writes())
	}
	if len(writesTo(rig, "DELETE /services/")) != 2 {
		t.Errorf("writes = %v, want both services removed first", rig.fake.Writes())
	}
	// One task stops; the other is still there at the next poll.
	base.Tasks = base.Tasks[1:]
	rig.fake.Reconfigure(base)
	clk.Tick(t, stackops.PollInterval)
	rr.until(t, "the wait to say one task is left", func(items map[string]jobs.Item) bool {
		return items[stackops.ItemWait].Result["left"] == "1"
	})
	if n := len(writesTo(rig, "DELETE /networks/")); n != 0 {
		t.Fatalf("writes = %v: a network was removed while a task was still stopping", rig.fake.Writes())
	}
	// The last task of the stack goes (the other stack's stays); the removal goes on.
	base.Tasks = base.Tasks[1:]
	rig.fake.Reconfigure(base)
	clk.Tick(t, stackops.PollInterval)
	res, job := rr.finish(t)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if n := len(writesTo(rig, "DELETE /networks/")); n != 2 {
		t.Errorf("writes = %v, want both of the stack's networks removed once its tasks were gone", rig.fake.Writes())
	}
	if it := itemByID(t, job, stackops.ItemWait); it.Result["left"] != "0" {
		t.Errorf("wait item = %+v, want none left", it)
	}
}

func TestRemoveStopsAtTheWaitWhenTasksAreStillThereAtTheTimeoutAndRemovesNoNetwork(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, func(c *dockertest.FakeConfig) {
		c.Tasks = []swarm.Task{
			{ID: "t1", ServiceID: "svc-web", DesiredState: swarm.TaskStateRemove, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: rmOwned()}},
		}
	})
	clk := newStepClock()
	rig.deps.Clock, rig.deps.ConvergeTimeout = clk, time.Minute
	store := &removeStore{}
	rr := rig.startRemove(t, stackops.RemoveInput{Store: store})

	rr.until(t, "the wait to say a task is left", func(items map[string]jobs.Item) bool {
		return items[stackops.ItemWait].Result["left"] == "1"
	})
	clk.Tick(t, 2*time.Minute)
	res, job := rr.finish(t)

	if res.Code != stackops.CodeTasksRemain || res.FailedStep != stackops.ItemWait {
		t.Fatalf("remove = %+v, want tasks_remain at the wait", res)
	}
	got := itemStates(job)
	if got[stackops.ItemServices] != "done" || got[stackops.ItemWait] != "failed:tasks_remain" || got[stackops.ItemRemoveNetworks] != "skipped:not_run" || got[stackops.ItemDefinition] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
	if n := len(writesTo(rig, "DELETE /networks/")) + len(writesTo(rig, "DELETE /secrets/")) + len(writesTo(rig, "DELETE /configs/")); n != 0 {
		t.Errorf("writes = %v, want no network, secret or config removed", rig.fake.Writes())
	}
	if store.wasDeleted() {
		t.Errorf("the definition was deleted although the removal did not finish")
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job state = %s, want partial", job.State)
	}
}

func TestRemoveOfAStackWhoseObjectsAreAlreadyGoneCompletesWithEveryStepDoneAndDeletesTheDefinition(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil) // a swarm that holds nothing of the stack
	store := &removeStore{}

	res, job := rig.remove(t, stackops.RemoveInput{Store: store})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if res.Services != 0 || res.Networks != 0 || !res.DefinitionDeleted {
		t.Errorf("result = %+v, want nothing removed and the definition deleted", res)
	}
	for id, state := range itemStates(job) {
		if state != "done" {
			t.Errorf("item %s = %q, want done", id, state)
		}
	}
	if len(writesTo(rig, "DELETE")) != 0 {
		t.Errorf("writes = %v, want none", rig.fake.Writes())
	}
	if !store.wasDeleted() {
		t.Errorf("the definition was not deleted")
	}
}

func TestRemoveKeepingTheDefinitionDeletesNothingStoredAndClearsTheStoppedRecord(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	store := &removeStore{}

	res, job := rig.remove(t, stackops.RemoveInput{Store: store, KeepDefinition: true})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if store.wasDeleted() || res.DefinitionDeleted || !res.DefinitionKept {
		t.Errorf("deleted = %v, result = %+v, want the definition kept", store.wasDeleted(), res)
	}
	if stopped, set := store.stopped[rmID]; !set || stopped {
		t.Errorf("stopped record = %v (set %v), want it cleared: the stack is not stopped, it is not deployed", stopped, set)
	}
	if _, has := itemStates(job)[stackops.ItemDefinition]; has {
		t.Errorf("items = %v, want no step for a definition that is kept", itemStates(job))
	}
}

func TestRemoveThatCannotRemoveANetworkStillRemovesTheSecretsAndConfigsAndKeepsTheDefinition(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, func(c *dockertest.FakeConfig) {
		c.RemoveErrors = map[string]string{"rm_back": "rpc error: the manager could not remove rm_back"}
	})
	store := &removeStore{}

	res, job := rig.remove(t, stackops.RemoveInput{Store: store})

	if res.Code != stackops.CodeRemoveFailed || res.FailedName != "rm_back" {
		t.Fatalf("remove = %+v, want remove_failed at rm_back", res)
	}
	got := itemStates(job)
	if got[stackops.ItemRemoveNetworks] != "failed:remove_failed" || got[stackops.ItemRemoveObjects] != "done" || got[stackops.ItemDefinition] != "skipped:not_run" {
		t.Errorf("items = %v, want the secrets and configs removed in spite of the network and the definition kept", got)
	}
	if !slices.Contains(rig.fake.Writes(), "DELETE /networks/net-front") || !slices.Contains(rig.fake.Writes(), "DELETE /secrets/sec-tok") {
		t.Errorf("writes = %v, want the rest removed", rig.fake.Writes())
	}
	if store.wasDeleted() {
		t.Errorf("the definition was deleted although a network stayed")
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job state = %s, want partial", job.State)
	}
}

// previewedVolumes are the rm_ volumes of the stack as the preview names them, each on its node, by name.
func previewedVolumes(t *testing.T, rig *swarmRig) map[string]stackops.VolumeChoice {
	t.Helper()
	preview, err := rig.deps.Preview(testsupport.Context(t), rig.envID, rmStack(), stackops.Declared{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]stackops.VolumeChoice{}
	for _, v := range preview.Volumes {
		out[v.Name] = v
	}
	return out
}

func TestPreviewCountsTheContainersOutsideTheStackThatMountAVolume(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{
			{ID: "c-own", Names: []string{"/rm_web.1.abc"}, Image: "nginx", State: "running", Labels: rmOwned(), Mounts: []string{"rm_data"}},
			{ID: "c-out1", Names: []string{"/backup"}, Image: "busybox", State: "running", Mounts: []string{"rm_data"}},
			{ID: "c-out2", Names: []string{"/old-backup"}, Image: "busybox", State: "exited", Mounts: []string{"rm_data", "rm_cache"}},
			{ID: "c-bind", Names: []string{"/binder"}, Image: "busybox", State: "running", Mounts: []string{"/srv/rm_data"}},
		}
	})

	got := previewedVolumes(t, rig)

	if got["rm_data"].UsedOutside != 2 || got["rm_cache"].UsedOutside != 1 {
		t.Errorf("volumes = %+v, want two containers outside the stack on rm_data (the stack's own and a bind mount do not count) and one on rm_cache", got)
	}
}

func TestRemoveRemovesOnlyTheTickedVolumesEachOnItsNodeAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	store := &removeStore{}
	vols := previewedVolumes(t, rig)

	res, job := rig.remove(t, stackops.RemoveInput{Store: store, Volumes: []stackops.VolumeChoice{vols["rm_data"]}})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if got := writesTo(rig, "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/rm_data"}) {
		t.Errorf("volume writes = %v, want only the ticked rm_data (not rm_cache, the external volume or another stack's)", got)
	}
	if res.Volumes != 1 || res.KeptVolumes != 1 || !res.DefinitionDeleted {
		t.Errorf("result = %+v, want 1 volume removed, 1 kept and the definition deleted", res)
	}
	id := "remove-volume:" + vols["rm_data"].NodeID + "/rm_data"
	if it := itemByID(t, job, id); it.State != jobs.ItemDone || it.Label != "Remove volume rm_data on "+vols["rm_data"].NodeName {
		t.Errorf("volume item = %+v, want a done step labelled with the volume and its node", it)
	}
	for _, w := range writesTo(rig, "DELETE") {
		if strings.Contains(w, "shared_vol") || strings.Contains(w, "other_data") || strings.Contains(w, "rm_cache") {
			t.Errorf("write %q touched a volume that was not ticked", w)
		}
	}
}

func TestRemoveWithNoVolumeTickedRemovesNoVolumeAndCountsThemAllKept(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)

	res, job := rig.remove(t, stackops.RemoveInput{Store: &removeStore{}})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v", res, itemStates(job))
	}
	if got := writesTo(rig, "DELETE /volumes/"); len(got) != 0 {
		t.Errorf("volume writes = %v, want none: volumes are removed only when ticked", got)
	}
	if res.Volumes != 0 || res.KeptVolumes != 2 {
		t.Errorf("result = %+v, want no volume removed and the stack's two kept", res)
	}
}

func TestRemoveRefusesATickedVolumeThatIsAnotherStacksAndRemovesNothing(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	store := &removeStore{}
	vols := previewedVolumes(t, rig)
	foreign := stackops.VolumeChoice{Name: "other_data", NodeID: vols["rm_data"].NodeID, NodeName: vols["rm_data"].NodeName}

	res, job := rig.remove(t, stackops.RemoveInput{Store: store, Volumes: []stackops.VolumeChoice{vols["rm_data"], foreign}})

	if res.Code != stackops.CodeVolumeNotOwned || res.FailedName != "other_data" {
		t.Fatalf("remove = %+v %v, want volume_not_owned at other_data", res, itemStates(job))
	}
	if got := rig.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none: a removal that asked for something that is not the stack's removes nothing", got)
	}
	if store.wasDeleted() {
		t.Errorf("the definition was deleted")
	}
}

func TestRemoveRefusesAnExternalVolumeEvenWhenTheRequestNamesIt(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	vols := previewedVolumes(t, rig)
	external := stackops.VolumeChoice{Name: "shared_vol", NodeID: vols["rm_data"].NodeID, NodeName: vols["rm_data"].NodeName}

	res, _ := rig.remove(t, stackops.RemoveInput{Store: &removeStore{}, Volumes: []stackops.VolumeChoice{external}})

	if res.Code != stackops.CodeVolumeNotOwned {
		t.Fatalf("remove = %+v, want volume_not_owned", res)
	}
	if got := writesTo(rig, "DELETE"); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestRemoveOfATickedVolumeThatIsAlreadyGoneCompletesTheStep(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, nil)
	vols := previewedVolumes(t, rig)
	gone := stackops.VolumeChoice{Name: "rm_vanished", NodeID: vols["rm_data"].NodeID, NodeName: vols["rm_data"].NodeName}

	res, job := rig.remove(t, stackops.RemoveInput{Store: &removeStore{}, Volumes: []stackops.VolumeChoice{gone}})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if got := writesTo(rig, "DELETE /volumes/"); len(got) != 0 {
		t.Errorf("volume writes = %v, want none for a volume that is not there", got)
	}
}

func TestRemoveLeavesATickedVolumeAContainerOutsideTheStackMountsAndRemovesTheRestKeepingTheDefinition(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{
			{ID: "c-out", Names: []string{"/backup"}, Image: "busybox", State: "running", Mounts: []string{"rm_data"}},
		}
	})
	store := &removeStore{}
	vols := previewedVolumes(t, rig)

	res, job := rig.remove(t, stackops.RemoveInput{Store: store, Volumes: []stackops.VolumeChoice{vols["rm_data"], vols["rm_cache"]}})

	if res.Code != stackops.CodeInUse || res.FailedName != "rm_data" {
		t.Fatalf("remove = %+v %v, want in_use at rm_data", res, itemStates(job))
	}
	if got := writesTo(rig, "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/rm_cache"}) {
		t.Errorf("volume writes = %v, want the busy volume left (no call made) and rm_cache removed", got)
	}
	if res.Volumes != 1 || res.KeptVolumes != 1 {
		t.Errorf("result = %+v, want 1 removed and 1 kept", res)
	}
	got := itemStates(job)
	if got["remove-volume:"+vols["rm_data"].NodeID+"/rm_data"] != "failed:in_use" || got["remove-volume:"+vols["rm_cache"].NodeID+"/rm_cache"] != "done" || got[stackops.ItemDefinition] != "skipped:not_run" {
		t.Errorf("items = %v", got)
	}
	if got := writesTo(rig, "DELETE /services/"); len(got) != 2 {
		t.Errorf("writes = %v, want the services, networks and objects removed all the same", rig.fake.Writes())
	}
	if store.wasDeleted() {
		t.Errorf("the definition was deleted although a volume stayed")
	}
	if job.State != jobs.JobPartial {
		t.Errorf("job state = %s, want partial", job.State)
	}
}

func TestRemoveLeavesATickedVolumeTheEngineRefusesAsInUseAndSaysSo(t *testing.T) {
	t.Parallel()
	rig := removeRig(t, func(c *dockertest.FakeConfig) { c.BusyVolumes = []string{"rm_data"} })
	vols := previewedVolumes(t, rig)

	res, job := rig.remove(t, stackops.RemoveInput{Store: &removeStore{}, Volumes: []stackops.VolumeChoice{vols["rm_data"]}})

	if res.Code != stackops.CodeInUse || res.FailedName != "rm_data" {
		t.Fatalf("remove = %+v %v, want in_use at rm_data", res, itemStates(job))
	}
}

// A standalone host's stack is removed by the Compose SDK's down, which takes the containers and the project's
// networks and never a volume; the volumes the person ticked are removed on the node afterwards.

func hostOwned() map[string]string {
	return map[string]string{"com.docker.compose.project": "plain", "coaming.stack.id": "stack-plain"}
}

func TestStandaloneRemoveRunsDownWithoutVolumesThenRemovesTheTickedVolumesOnTheNode(t *testing.T) {
	t.Parallel()
	var base dockertest.FakeConfig
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/plain-web-1"}, Image: "nginx", State: "running", Labels: withLabel(hostOwned(), "com.docker.compose.service", "web"), Mounts: []string{"plain_data"}},
		}
		c.Networks = []dockertest.FakeNetwork{{ID: "net-plain", Name: "plain_default", Driver: "bridge", Scope: "local", Labels: hostOwned()}}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "plain_data", Driver: "local", Labels: hostOwned()},
			{Name: "plain_keep", Driver: "local", Labels: hostOwned()},
			{Name: "external_vol", Driver: "local"},
		}
		base = *c
	})
	var downs []string
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.DownProject = func(_ context.Context, _ *dockerx.Client, project string) error {
		downs = append(downs, project)
		// The SDK removes the containers and the project's network, as a down does, and no volume.
		base.Containers, base.Networks = nil, nil
		rig.fake.Reconfigure(base)
		return nil
	}
	st := stackops.Stack{ID: "stack-plain", Name: "plain", Target: stacks.TargetStandalone}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := rig.deps.Preview(testsupport.Context(t), rig.envID, st, stackops.Declared{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Containers, []string{"plain-web-1"}) || len(preview.Volumes) != 2 || preview.Volumes[0].UsedOutside != 0 {
		t.Fatalf("preview = %+v, want the container and the stack's two volumes, none used outside", preview)
	}
	var data stackops.VolumeChoice
	for _, v := range preview.Volumes {
		if v.Name == "plain_data" {
			data = v
		}
	}
	store := &removeStore{}

	rr := rig.startRemove(t, stackops.RemoveInput{Stack: st, NodeID: node, Store: store, Volumes: []stackops.VolumeChoice{data}})
	res, job := rr.finish(t)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if !slices.Equal(downs, []string{"plain"}) {
		t.Errorf("down calls = %v, want one for the project plain", downs)
	}
	if got := writesTo(rig, "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/plain_data"}) {
		t.Errorf("volume writes = %v, want only the ticked plain_data", got)
	}
	if res.Services != 1 || res.Volumes != 1 || res.KeptVolumes != 1 || !res.DefinitionDeleted {
		t.Errorf("result = %+v, want 1 container, 1 volume removed, 1 kept and the definition deleted", res)
	}
	if _, has := itemStates(job)[stackops.ItemRemoveObjects]; has {
		t.Errorf("items = %v, a standalone host has no secrets or configs to remove", itemStates(job))
	}
	if it := itemByID(t, job, stackops.ItemServices); it.Label != "Remove containers" {
		t.Errorf("services item label = %q, want Remove containers", it.Label)
	}
	if !store.wasDeletedAs("stack-plain") {
		t.Errorf("the definition was not deleted")
	}
}

func TestStandaloneRemoveOfAProjectAnotherOwnerMadeFailsTheCheckAndRunsNothing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/plain-web-1"}, Image: "nginx", State: "running", Labels: map[string]string{"com.docker.compose.project": "plain"}},
		}
	})
	var downs int
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.DownProject = func(context.Context, *dockerx.Client, string) error { downs++; return nil }
	st := stackops.Stack{ID: "stack-plain", Name: "plain", Target: stacks.TargetStandalone}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}

	res, job := rig.remove(t, stackops.RemoveInput{Stack: st, NodeID: node, Store: &removeStore{}})

	if res.Code != stackops.CodeNameClash || downs != 0 {
		t.Fatalf("remove = %+v (down calls %d) %v, want stack_name_clash and no down", res, downs, itemStates(job))
	}
}

// hostCLIAdded is what `docker compose -f <the person's file> up -d --scale web=2 --no-recreate` adds to a stack's
// project: a container of the project and the service with none of Coaming's labels (G-08-6, the debug session
// standalone-cli-scale-not-drift).
func hostCLIAdded(id, name string, mounts ...string) dockertest.FakeContainer {
	return dockertest.FakeContainer{
		ID: id, Names: []string{"/" + name}, Image: "nginx", State: "running", Mounts: mounts,
		Labels: map[string]string{"com.docker.compose.project": "plain", "com.docker.compose.service": "web"},
	}
}

func TestStandaloneRemoveTakesAContainerTheHostCLIAddedAndWaitsForIt(t *testing.T) {
	t.Parallel()
	var base dockertest.FakeConfig
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/plain-web-1"}, Image: "nginx", State: "running", Labels: withLabel(hostOwned(), "com.docker.compose.service", "web"), Mounts: []string{"plain_data"}},
			hostCLIAdded("c2", "plain-web-2", "plain_data"),
		}
		c.Networks = []dockertest.FakeNetwork{{ID: "net-plain", Name: "plain_default", Driver: "bridge", Scope: "local", Labels: hostOwned()}}
		c.Volumes = []dockertest.FakeVolume{{Name: "plain_data", Driver: "local", Labels: hostOwned()}}
		base = *c
	})
	clk := newStepClock()
	rig.deps.Clock = clk
	var downs []string
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.DownProject = func(_ context.Context, _ *dockerx.Client, project string) error {
		downs = append(downs, project)
		// The stack's own container goes first and the network with it; the one the host CLI added is still there until
		// the test says it is gone, so the wait is what keeps the removal from going on.
		base.Containers, base.Networks = base.Containers[1:], nil
		rig.fake.Reconfigure(base)
		return nil
	}
	st := stackops.Stack{ID: "stack-plain", Name: "plain", Target: stacks.TargetStandalone}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := rig.deps.Preview(testsupport.Context(t), rig.envID, st, stackops.Declared{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Containers, []string{"plain-web-1", "plain-web-2"}) {
		t.Fatalf("preview containers = %v, want both the stack's container and the one the host CLI added", preview.Containers)
	}
	if len(preview.Volumes) != 1 || preview.Volumes[0].UsedOutside != 0 {
		t.Fatalf("preview volumes = %+v, want plain_data with no user outside the stack: the added container is the stack's", preview.Volumes)
	}
	rr := rig.startRemove(t, stackops.RemoveInput{Stack: st, NodeID: node, Store: &removeStore{}, Volumes: []stackops.VolumeChoice{preview.Volumes[0]}})

	rr.until(t, "the wait to say the added container is left", func(items map[string]jobs.Item) bool {
		return items[stackops.ItemWait].Result["left"] == "1"
	})
	if got := writesTo(rig, "DELETE /volumes/"); len(got) != 0 {
		t.Fatalf("volume writes = %v: a volume was removed while the added container was still there", got)
	}
	base.Containers = nil
	rig.fake.Reconfigure(base)
	clk.Tick(t, stackops.PollInterval)
	res, job := rr.finish(t)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if !slices.Equal(downs, []string{"plain"}) {
		t.Errorf("down calls = %v, want one for the project plain", downs)
	}
	if res.Services != 2 || res.Volumes != 1 {
		t.Errorf("result = %+v, want 2 containers and the volume removed", res)
	}
}

func TestStandaloneRemoveStillFailsTheCheckForAnotherStacksContainerBesideItsOwn(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		stranger := hostCLIAdded("c3", "plain-web-3")
		stranger.Labels["coaming.stack.id"] = "stack-stranger"
		c.Containers = []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/plain-web-1"}, Image: "nginx", State: "running", Labels: withLabel(hostOwned(), "com.docker.compose.service", "web")},
			hostCLIAdded("c2", "plain-web-2"),
			stranger,
		}
	})
	var downs int
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.DownProject = func(context.Context, *dockerx.Client, string) error { downs++; return nil }
	st := stackops.Stack{ID: "stack-plain", Name: "plain", Target: stacks.TargetStandalone}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}

	res, job := rig.remove(t, stackops.RemoveInput{Stack: st, NodeID: node, Store: &removeStore{}})

	if res.Code != stackops.CodeNameClash || downs != 0 {
		t.Fatalf("remove = %+v (down calls %d) %v, want stack_name_clash and no down", res, downs, itemStates(job))
	}
}

func TestStandaloneRemoveWhenTheHostIsReadOnlyChangesNothing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	var downs int
	rig.deps.Writable = func(context.Context, string, string) error { return envs.ErrHostReadOnly }
	rig.deps.DownProject = func(context.Context, *dockerx.Client, string) error { downs++; return nil }
	st := stackops.Stack{ID: "stack-plain", Name: "plain", Target: stacks.TargetStandalone}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	store := &removeStore{}

	res, _ := rig.remove(t, stackops.RemoveInput{Stack: st, NodeID: node, Store: store})

	if res.Code != stackops.CodeHostReadOnly || downs != 0 || store.wasDeletedAs("stack-plain") {
		t.Errorf("remove = %+v (down calls %d), want host_read_only, nothing run and nothing deleted", res, downs)
	}
}

func withLabel(m map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for key, val := range m {
		out[key] = val
	}
	out[k] = v
	return out
}

func TestRemoveWaitsForTheNodesToRemoveTheContainersOfTasksThatHaveEndedBeforeItRemovesAVolume(t *testing.T) {
	t.Parallel()
	var base dockertest.FakeConfig
	rig := removeRig(t, func(c *dockertest.FakeConfig) {
		// Every task has ended, but the node has not yet removed the container of one of them, and a container
		// of another stack (which is not waited for) mounts the same name of a volume of its own.
		c.Containers = []dockertest.FakeContainer{
			{ID: "c-old", Names: []string{"/rm_web.1.abc"}, Image: "nginx", State: "exited", Labels: rmOwned(), Mounts: []string{"rm_data"}},
			{ID: "c-other", Names: []string{"/other_web.1.abc"}, Image: "nginx", State: "running", Labels: rmOther()},
		}
		base = *c
	})
	clk := newStepClock()
	rig.deps.Clock = clk
	vols := previewedVolumes(t, rig)
	rr := rig.startRemove(t, stackops.RemoveInput{Store: &removeStore{}, Volumes: []stackops.VolumeChoice{vols["rm_data"]}})

	rr.until(t, "the wait to say a container is left", func(items map[string]jobs.Item) bool {
		return items[stackops.ItemWait].Result["left"] == "1"
	})
	if n := len(writesTo(rig, "DELETE /networks/")) + len(writesTo(rig, "DELETE /volumes/")); n != 0 {
		t.Fatalf("writes = %v: a network or a volume was removed while a container of the stack was still there", rig.fake.Writes())
	}
	base.Containers = base.Containers[1:]
	rig.fake.Reconfigure(base)
	clk.Tick(t, stackops.PollInterval)
	res, job := rr.finish(t)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if got := writesTo(rig, "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/rm_data"}) {
		t.Errorf("volume writes = %v, want rm_data removed once its container was gone", got)
	}
}

// An adopted stack (plan 08-18) is removed like any other, and only what it owns: its services by the namespace
// while its first deploy is pending and by its own ID after, the networks and volumes it recorded when it was
// adopted, and the secrets and configs it made. What it only references, and what appeared later, stays.

// adoptedRm is the stack "rm" as an adopted one: it recorded two networks and two volumes, and references the secret
// extsecret and the config extconfig as external.
func adoptedRm(pending bool) stackops.Stack {
	st := rmStack()
	st.Source = stacks.Source{Kind: stacks.SourceAdopted, Ref: "rm"}
	st.Adoption = stacks.Adoption{
		Pending: pending, PriorID: "stale-id",
		Networks: []string{"rm_front", "rm_back"}, Volumes: []string{"rm_data", "rm_cache"},
		Secrets: []string{"extsecret"}, Configs: []string{"extconfig"},
	}
	return st
}

// rmNamespaceOnly are the labels docker stack deploy puts on an object: the namespace and nothing of Coaming's.
func rmNamespaceOnly() map[string]string {
	return map[string]string{"com.docker.stack.namespace": "rm"}
}

// adoptedRemoveRig is a swarm that runs the adopted stack "rm", with the services labelled as pending says: with the
// namespace alone before its first deploy, with the stack's ID after. Its networks and volumes carry the namespace and
// no Coaming ID, or a stale one; the secret and the config it made carry its ID; the ones it references carry only the
// namespace. Beside them are an external network and volume and another stack's service.
func adoptedRemoveRig(t *testing.T, pending bool, tune func(*dockertest.FakeConfig)) *swarmRig {
	t.Helper()
	svcLabels := rmOwned()
	if pending {
		svcLabels = rmNamespaceOnly()
	}
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{
			{ID: "svc-web", Name: "rm_web", Mode: "replicated", Image: "nginx:1.27", Version: 3, Labels: svcLabels, Desired: 2, Running: 2},
			{ID: "svc-api", Name: "rm_api", Mode: "replicated", Image: "nginx:1.28", Version: 2, Labels: svcLabels, Desired: 1, Running: 1},
			{ID: "svc-other", Name: "other_web", Mode: "replicated", Image: "nginx:1.28", Version: 2, Labels: rmOther(), Desired: 1, Running: 1},
		}
		c.Networks = []dockertest.FakeNetwork{
			{ID: "net-front", Name: "rm_front", Driver: "overlay", Scope: "swarm", Labels: rmNamespaceOnly()},
			{ID: "net-back", Name: "rm_back", Driver: "overlay", Scope: "swarm", Labels: withLabel(rmNamespaceOnly(), "coaming.stack.id", "stale-id")},
			{ID: "net-shared", Name: "shared", Driver: "overlay", Scope: "swarm"},
			{ID: "net-other", Name: "other_net", Driver: "overlay", Scope: "swarm", Labels: rmOther()},
		}
		c.Secrets = []dockertest.FakeSecret{
			{ID: "sec-tok", Name: "rm_tok_abc", Labels: rmOwned()},
			{ID: "sec-ext", Name: "extsecret", Labels: rmNamespaceOnly()},
		}
		c.Configs = []dockertest.FakeConfigObject{
			{ID: "cfg-site", Name: "rm_site_abc", Labels: rmOwned()},
			{ID: "cfg-ext", Name: "extconfig", Labels: rmNamespaceOnly()},
		}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "rm_data", Driver: "local", Labels: rmNamespaceOnly()},
			{Name: "rm_cache", Driver: "local", Labels: withLabel(rmNamespaceOnly(), "coaming.stack.id", "stale-id")},
			{Name: "shared_vol", Driver: "local"},
			{Name: "other_data", Driver: "local", Labels: rmOther()},
		}
		if tune != nil {
			tune(c)
		}
	})
}

func TestRemoveAdoptedSwarmStack(t *testing.T) {
	t.Parallel()
	rig := adoptedRemoveRig(t, false, nil)
	vols := previewedVolumesOf(t, rig, adoptedRm(false))
	store := &removeStore{}

	res, job := rig.remove(t, stackops.RemoveInput{Stack: adoptedRm(false), Store: store, Volumes: []stackops.VolumeChoice{vols["rm_data"]}})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if res.Services != 2 || res.Networks != 2 || res.Secrets != 1 || res.Configs != 1 || res.Volumes != 1 || res.KeptVolumes != 1 || !res.DefinitionDeleted {
		t.Errorf("result = %+v, want 2 services, the 2 recorded networks, the secret and the config it made, 1 volume removed and 1 kept", res)
	}
	writes := rig.fake.Writes()
	for _, want := range []string{
		"DELETE /services/svc-web", "DELETE /services/svc-api", "DELETE /networks/net-front", "DELETE /networks/net-back",
		"DELETE /secrets/sec-tok", "DELETE /configs/cfg-site", "DELETE /volumes/rm_data",
	} {
		if !slices.Contains(writes, want) {
			t.Errorf("writes = %v, want %q", writes, want)
		}
	}
	for _, w := range writes {
		for _, keep := range []string{"svc-other", "net-shared", "net-other", "sec-ext", "cfg-ext", "rm_cache", "shared_vol", "other_data"} {
			if strings.Contains(w, keep) {
				t.Errorf("write %q touched %s, which the adopted stack does not own or was not ticked", w, keep)
			}
		}
	}
	if !store.wasDeleted() {
		t.Errorf("the definition was not deleted")
	}
}

func TestAnAdoptedSwarmStackIsRemovedBeforeItsFirstDeploy(t *testing.T) {
	t.Parallel()
	// The services carry the namespace alone, so the pending adoption is what makes them the stack's.
	rig := adoptedRemoveRig(t, true, nil)

	res, job := rig.remove(t, stackops.RemoveInput{Stack: adoptedRm(true), Store: &removeStore{}})

	if !res.OK() || job.State != jobs.JobDone || res.Services != 2 || res.Networks != 2 {
		t.Fatalf("remove = %+v %v, want the 2 services and the 2 recorded networks removed", res, itemStates(job))
	}
	if got := writesTo(rig, "DELETE /services/"); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"DELETE /services/svc-api", "DELETE /services/svc-web"}) {
		t.Errorf("service writes = %v, want only the stack's two", got)
	}
}

func TestRemovalPreviewOfAdoptedStack(t *testing.T) {
	t.Parallel()
	rig := adoptedRemoveRig(t, true, nil)
	decl := stackops.Declared{
		Networks: []stackops.DeclaredObject{{Key: "outer", Name: "shared", External: true}},
		Secrets:  []stackops.DeclaredObject{{Key: "ext", Name: "extsecret", External: true}},
		Configs:  []stackops.DeclaredObject{{Key: "ext", Name: "extconfig", External: true}},
	}

	got, err := rig.deps.Preview(testsupport.Context(t), rig.envID, adoptedRm(true), decl)

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rm_api", "rm_web"}; !slices.Equal(got.Services, want) {
		t.Errorf("services = %v, want %v", got.Services, want)
	}
	if want := []string{"rm_back", "rm_front"}; !slices.Equal(got.Networks, want) {
		t.Errorf("networks = %v, want the two recorded %v", got.Networks, want)
	}
	if !slices.Equal(got.Secrets, []string{"rm_tok_abc"}) || !slices.Equal(got.Configs, []string{"rm_site_abc"}) {
		t.Errorf("secrets = %v configs = %v, want only the ones it made: the external ones it references are not its own", got.Secrets, got.Configs)
	}
	var vols []string
	for _, v := range got.Volumes {
		vols = append(vols, v.Name)
	}
	if want := []string{"rm_cache", "rm_data"}; !slices.Equal(vols, want) {
		t.Errorf("volumes = %v, want the two recorded %v", vols, want)
	}
	if want := []string{"extconfig", "extsecret", "shared"}; !slices.Equal(got.Externals, want) {
		t.Errorf("externals = %v, want %v", got.Externals, want)
	}
}

func TestRemoveLeavesAForeignLateService(t *testing.T) {
	t.Parallel()
	// After the first deploy the stack's services carry its ID. Two services of the namespace appeared since, one with
	// no Coaming ID and one with another stack's: neither is the stack's, and their running tasks are not waited for.
	rig := adoptedRemoveRig(t, false, func(c *dockertest.FakeConfig) {
		c.ServiceRows = append(c.ServiceRows,
			dockertest.FakeService{ID: "svc-late", Name: "rm_late", Mode: "replicated", Image: "nginx:1.28", Version: 1, Labels: rmNamespaceOnly(), Desired: 1, Running: 1},
			dockertest.FakeService{ID: "svc-alien", Name: "rm_alien", Mode: "replicated", Image: "nginx:1.28", Version: 1, Labels: withLabel(rmNamespaceOnly(), "coaming.stack.id", "stack-alien"), Desired: 1, Running: 1},
		)
		c.Tasks = []swarm.Task{
			{ID: "t-late", ServiceID: "svc-late", DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: rmNamespaceOnly()}},
			{ID: "t-alien", ServiceID: "svc-alien", DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}, Annotations: swarm.Annotations{Labels: withLabel(rmNamespaceOnly(), "coaming.stack.id", "stack-alien")}},
		}
	})
	rig.deps.ConvergeTimeout = 2 * time.Second

	res, job := rig.remove(t, stackops.RemoveInput{Stack: adoptedRm(false), Store: &removeStore{}})

	if !res.OK() || job.State != jobs.JobDone || res.Services != 2 {
		t.Fatalf("remove = %+v %v, want the stack's two services removed and the wait over", res, itemStates(job))
	}
	for _, w := range rig.fake.Writes() {
		if strings.Contains(w, "svc-late") || strings.Contains(w, "svc-alien") {
			t.Errorf("write %q touched a service the stack does not own", w)
		}
	}
}

func TestRemoveAdoptedStandaloneStack(t *testing.T) {
	t.Parallel()
	project := map[string]string{"com.docker.compose.project": "plain"}
	var base dockertest.FakeConfig
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/plain-web-1"}, Image: "nginx", State: "running", Labels: withLabel(project, "com.docker.compose.service", "web"), Mounts: []string{"plain_data"}},
		}
		c.Networks = []dockertest.FakeNetwork{{ID: "net-plain", Name: "plain_default", Driver: "bridge", Scope: "local", Labels: project}}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "plain_data", Driver: "local", Labels: project},
			{Name: "plain_keep", Driver: "local", Labels: project},
			{Name: "external_vol", Driver: "local"},
		}
		base = *c
	})
	var downs []string
	rig.deps.Writable = func(context.Context, string, string) error { return nil }
	rig.deps.DownProject = func(_ context.Context, _ *dockerx.Client, p string) error {
		downs = append(downs, p)
		base.Containers, base.Networks = nil, nil
		rig.fake.Reconfigure(base)
		return nil
	}
	st := stackops.Stack{
		ID: "stack-plain", Name: "plain", Target: stacks.TargetStandalone,
		Source:   stacks.Source{Kind: stacks.SourceAdopted, Ref: "plain"},
		Adoption: stacks.Adoption{Pending: true, Networks: []string{"plain_default"}, Volumes: []string{"plain_data", "plain_keep"}},
	}
	node, err := rig.deps.StandaloneNode(testsupport.Context(t), rig.envID)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := rig.deps.Preview(testsupport.Context(t), rig.envID, st, stackops.Declared{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Containers, []string{"plain-web-1"}) || !slices.Equal(preview.Networks, []string{"plain_default"}) || len(preview.Volumes) != 2 {
		t.Fatalf("preview = %+v, want the container, the recorded network and the two recorded volumes", preview)
	}
	var data stackops.VolumeChoice
	for _, v := range preview.Volumes {
		if v.Name == "plain_data" {
			data = v
		}
	}

	res, job := rig.remove(t, stackops.RemoveInput{Stack: st, NodeID: node, Store: &removeStore{}, Volumes: []stackops.VolumeChoice{data}})

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("remove = %+v %v, want it done", res, itemStates(job))
	}
	if !slices.Equal(downs, []string{"plain"}) {
		t.Errorf("down calls = %v, want one for the project plain", downs)
	}
	if got := writesTo(rig, "DELETE /volumes/"); !slices.Equal(got, []string{"DELETE /volumes/plain_data"}) {
		t.Errorf("volume writes = %v, want only the ticked plain_data (not plain_keep, not external_vol)", got)
	}
	if res.Services != 1 || res.Volumes != 1 || res.KeptVolumes != 1 {
		t.Errorf("result = %+v, want 1 container, 1 volume removed and 1 kept", res)
	}
}

// previewedVolumesOf is previewedVolumes for a stack other than rm.
func previewedVolumesOf(t *testing.T, rig *swarmRig, st stackops.Stack) map[string]stackops.VolumeChoice {
	t.Helper()
	preview, err := rig.deps.Preview(testsupport.Context(t), rig.envID, st, stackops.Declared{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]stackops.VolumeChoice{}
	for _, v := range preview.Volumes {
		out[v.Name] = v
	}
	return out
}
