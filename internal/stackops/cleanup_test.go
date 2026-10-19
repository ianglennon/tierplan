// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// A deploy that stops (plan 07-18, D-14, STK-07): it stops where it is, removes what it created that
// nothing uses, and never rolls back or retries.

// failCompose is three services: api stays on the network the stack already has, mid and web join a
// new one, and mid reads a new secret. mid is the one the swarm rejects.
const failCompose = `services:
  api:
    image: busybox:1.36
    networks: [legacy]
  mid:
    image: busybox:1.36
    networks: [fresh]
    secrets: [tok]
  web:
    image: nginx:1.27-alpine
    networks: [fresh]
networks:
  legacy: {}
  fresh: {}
secrets:
  tok:
    file: tok.txt
`

var tOwned = map[string]string{"com.docker.stack.namespace": "t", "bayplan.stack.id": "stack-t"}

// failRig is a swarm that already runs the stack's three services and its legacy network, and
// rejects the update of mid.
func failRig(t *testing.T, tune func(*dockertest.FakeConfig)) *swarmRig {
	t.Helper()
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n-legacy", Name: "t_legacy", Driver: "overlay", Scope: "swarm", Labels: tOwned}}
		if tune != nil {
			tune(c)
		}
	},
		dockertest.FakeService{ID: "svc-api", Name: "t_api", Mode: "replicated", Image: "busybox:1.35", Version: 3, Labels: tOwned, Desired: 1, Running: 1},
		dockertest.FakeService{ID: "svc-mid", Name: "t_mid", Mode: "replicated", Image: "busybox:1.35", Version: 4, Labels: tOwned, Desired: 1, Running: 1, UpdateError: "rpc error: invalid environment"},
		dockertest.FakeService{ID: "svc-web", Name: "t_web", Mode: "replicated", Image: "nginx:1.26", Version: 5, Labels: tOwned, Desired: 1, Running: 1},
	)
}

func TestFailedDeployCleansUpOnlyWhatItCreated(t *testing.T) {
	t.Parallel()
	rig := failRig(t, nil)
	_, in := stackInput(t, "t", failCompose, tokFile("s3cret"))
	secret := hashedName("t", "tok", "s3cret")
	res, job := rig.deploy(t, in)

	if res.Code != stackops.CodeServiceRejected || res.FailedService != "t_mid" {
		t.Fatalf("result = %+v, want the swarm's rejection of t_mid", res)
	}
	if res.Updated != 1 || res.Total != 3 {
		t.Errorf("updated %d of %d, want 1 of 3: only t_api was written", res.Updated, res.Total)
	}
	if !slices.Equal(res.CleanedUp, []string{"t_fresh", secret}) {
		t.Errorf("CleanedUp = %v, want the network and the secret this deploy created", res.CleanedUp)
	}
	if len(res.LeftInPlace) != 0 {
		t.Errorf("LeftInPlace = %v, want nothing: no service uses what was created", res.LeftInPlace)
	}
	writes := rig.fake.Writes()
	if countWrites(writes, "DELETE /networks/") != 1 || countWrites(writes, "DELETE /secrets/") != 1 || countWrites(writes, "DELETE /services/") != 0 {
		t.Errorf("writes = %v, want the created network and the created secret removed and no service", writes)
	}
	if names := rig.secretNames(t); len(names) != 0 {
		t.Errorf("secrets = %v, want none left", names)
	}

	for id, want := range map[string]string{
		"service:t_api": "done", "service:t_mid": "failed:service_rejected", "service:t_web": "skipped:not_run",
		stackops.ItemServices: "failed:service_rejected", stackops.ItemCleanup: "skipped:not_run",
	} {
		if got := itemStates(job)[id]; got != want {
			t.Errorf("item %s = %q, want %q (%v)", id, got, want, itemStates(job))
		}
	}
	// Each removal is a step under the cleanup step, done, and says it was cleaned up.
	for _, id := range []string{"clean-network:t_fresh", "clean-secret:" + secret} {
		it := itemByID(t, job, id)
		if it.Parent != stackops.ItemCleanup || it.Kind != stackops.KindStep || it.State != jobs.ItemDone || it.Result["cleaned"] != "true" {
			t.Errorf("item %s = %+v, want a done step under the cleanup step that says it was cleaned up", id, it)
		}
	}
	if it := itemByID(t, job, "clean-network:t_fresh"); it.Label != "Remove network t_fresh" {
		t.Errorf("label = %q, want Remove network t_fresh", it.Label)
	}
	if !slices.Contains(outputOf(t, rig, job), "Removed network t_fresh") {
		t.Errorf("output = %q, want a line for the network removed", outputOf(t, rig, job))
	}
}

func TestFailedDeployKeepsACreatedNetworkAnUpdatedServiceUses(t *testing.T) {
	t.Parallel()
	rig := failRig(t, nil)
	// api joins the new network too, so by the time mid is rejected a service already uses it.
	compose := strings.Replace(failCompose, "networks: [legacy]", "networks: [legacy, fresh]", 1)
	_, in := stackInput(t, "t", compose, tokFile("s3cret"))
	secret := hashedName("t", "tok", "s3cret")
	res, job := rig.deploy(t, in)

	if res.FailedService != "t_mid" {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Equal(res.LeftInPlace, []string{"t_fresh"}) || !slices.Equal(res.CleanedUp, []string{secret}) {
		t.Errorf("LeftInPlace = %v, CleanedUp = %v, want the network kept for t_api and the secret removed", res.LeftInPlace, res.CleanedUp)
	}
	if countWrites(rig.fake.Writes(), "DELETE /networks/") != 0 {
		t.Errorf("writes = %v, a network a service uses must not be removed", rig.fake.Writes())
	}
	it := itemByID(t, job, "clean-network:t_fresh")
	if it.State != jobs.ItemDone || it.Result["cleaned"] != "false" || it.Result["left"] != "in_use" {
		t.Errorf("item = %+v, want it done, not cleaned and left because a service uses it", it)
	}
}

func TestFailedDeployNamesWhatItCouldNotRemove(t *testing.T) {
	t.Parallel()
	rig := failRig(t, func(c *dockertest.FakeConfig) { c.RemoveErrors = map[string]string{"t_fresh": "network is busy"} })
	_, in := stackInput(t, "t", failCompose, tokFile("s3cret"))
	secret := hashedName("t", "tok", "s3cret")
	res, job := rig.deploy(t, in)

	if !slices.Equal(res.LeftInPlace, []string{"t_fresh"}) || !slices.Equal(res.CleanedUp, []string{secret}) {
		t.Fatalf("LeftInPlace = %v, CleanedUp = %v, want the network named and the secret removed", res.LeftInPlace, res.CleanedUp)
	}
	it := itemByID(t, job, "clean-network:t_fresh")
	if it.State != jobs.ItemFailed || it.Result["left"] != "failed" || it.Result["name"] != "t_fresh" || it.Result["kind"] != "network" {
		t.Errorf("item = %+v, want a failed removal that says what stays", it)
	}
}

func TestFailedDeployLeavesEverythingWhenTheServicesCannotBeReadAgain(t *testing.T) {
	t.Parallel()
	// The swarm loses its quorum when the rejected write arrives, so the read that decides what
	// nothing uses fails, and nothing may be judged unused.
	var fake atomic.Pointer[dockertest.Fake]
	rig := failRig(t, func(c *dockertest.FakeConfig) {
		c.OnWrite = func(w string) {
			if strings.HasPrefix(w, "POST /services/svc-mid/update") {
				fake.Load().LoseQuorum()
			}
		}
	})
	fake.Store(rig.fake)
	_, in := stackInput(t, "t", failCompose, tokFile("s3cret"))
	res, _ := rig.deploy(t, in)

	if len(res.CleanedUp) != 0 || len(res.LeftInPlace) != 2 {
		t.Errorf("CleanedUp = %v, LeftInPlace = %v, want nothing removed and both named", res.CleanedUp, res.LeftInPlace)
	}
	if n := countWrites(rig.fake.Writes(), "DELETE"); n != 0 {
		t.Errorf("writes = %v, nothing may be removed on a guess", rig.fake.Writes())
	}
}

// cancelOn cancels a deploy's job at the first write that starts with prefix, from inside the fake
// daemon while it holds that write, so the cancel lands at an exact point of the deploy.
type cancelOn struct {
	prefix string
	hub    atomic.Pointer[jobs.Hub]
	id     atomic.Value
	ready  chan struct{}
	fired  atomic.Bool
}

func newCancelOn(prefix string) *cancelOn {
	return &cancelOn{prefix: prefix, ready: make(chan struct{})}
}

func (c *cancelOn) hook(write string) {
	if !strings.HasPrefix(write, c.prefix) || !c.fired.CompareAndSwap(false, true) {
		return
	}
	<-c.ready
	id, _ := c.id.Load().(string)
	_ = c.hub.Load().Cancel(id)
}

// arm tells the hook which job to cancel; the deploy waits for it at the write.
func (c *cancelOn) arm(hub *jobs.Hub, id string) {
	c.hub.Store(hub)
	c.id.Store(id)
	close(c.ready)
}

// cancelCompose is two services; web reads a secret api does not.
const cancelCompose = `services:
  api:
    image: busybox:1.36
  web:
    image: nginx:1.27-alpine
    secrets: [tok]
secrets:
  tok:
    file: tok.txt
`

func TestCancelStopsAfterTheCurrentStep(t *testing.T) {
	t.Parallel()
	co := newCancelOn("POST /services/create")
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) { c.OnWrite = co.hook })
	_, in := stackInput(t, "t", cancelCompose, tokFile("s3cret"))
	secret := hashedName("t", "tok", "s3cret")
	d := rig.startDeploy(t, in)
	co.arm(rig.hub, d.job.ID)
	res, job := d.finish(t)

	if res.Code != stackops.CodeCancelled {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	// The service being written when the cancel came is finished, and no other is started.
	if creates := rig.fake.ServiceCreates(); len(creates) != 1 || creates[0].Spec.Name != "t_api" {
		t.Errorf("service creates = %+v, want only t_api: the one in flight finishes, none starts after the cancel", creates)
	}
	if n := countWrites(rig.fake.Writes(), "POST /services/"); n != 1 {
		t.Errorf("service writes = %d, want 1: no service is written twice", n)
	}
	api := itemByID(t, job, "service:t_api")
	if api.State != jobs.ItemDone || api.Result["action"] != "created" || api.Result["settled"] != "false" {
		t.Errorf("api row = %+v, want done as created and not waited for", api)
	}
	if web := itemByID(t, job, "service:t_web"); web.State != jobs.ItemCancelled {
		t.Errorf("web row = %+v, want cancelled", web)
	}
	// The secret only web would have read is removed; the network api joined stays.
	if !slices.Equal(res.CleanedUp, []string{secret}) || !slices.Equal(res.LeftInPlace, []string{"t_default"}) {
		t.Errorf("CleanedUp = %v, LeftInPlace = %v, want the unused secret removed and the network api uses kept", res.CleanedUp, res.LeftInPlace)
	}
	if it := itemByID(t, job, "clean-secret:"+secret); it.State != jobs.ItemDone || it.Result["cleaned"] != "true" {
		t.Errorf("cleanup item = %+v", it)
	}
}

func TestCancelDuringTheWaitEndsTheServicesAsCreatedAndRemovesNothingInUse(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	clk := newStepClock()
	rig.deps.Clock = clk
	rig.fake.SetTaskState("t_api", "preparing", "")
	_, in := stackInput(t, "t", cancelCompose, tokFile("s3cret"))
	d := rig.startDeploy(t, in)
	d.until(t, "the stuck row to count its tasks", func(items map[string]jobs.Item) bool {
		return items["service:t_api"].Unit == "tasks"
	})
	if err := rig.hub.Cancel(d.job.ID); err != nil {
		t.Fatal(err)
	}
	res, job := d.finish(t)

	if res.Code != stackops.CodeCancelled {
		t.Fatalf("result = %+v, want cancelled", res)
	}
	// api was still coming up when the cancel came, so it ends as created; web had settled.
	if it := itemByID(t, job, "service:t_api"); it.State != jobs.ItemDone || it.Result["settled"] != "false" || it.Result["action"] != "created" {
		t.Errorf("api row = %+v, want done as created and not settled", it)
	}
	if it := itemByID(t, job, "service:t_web"); it.State != jobs.ItemDone || it.Result["settled"] != "true" {
		t.Errorf("web row = %+v, want done and settled", it)
	}
	if len(res.CleanedUp) != 0 || countWrites(rig.fake.Writes(), "DELETE") != 0 {
		t.Errorf("CleanedUp = %v, writes = %v, want nothing removed: both services use what was created", res.CleanedUp, rig.fake.Writes())
	}
}

func TestNoAutomaticRollBack(t *testing.T) {
	t.Parallel()
	rig := failRig(t, nil)
	_, in := stackInput(t, "t", failCompose, tokFile("s3cret"))
	res, _ := rig.deploy(t, in)
	if res.OK() {
		t.Fatalf("result = %+v, want the deploy to fail", res)
	}
	updates := map[string]int{}
	for _, w := range rig.fake.Writes() {
		if strings.Contains(w, "rollback=") {
			t.Errorf("write %q is a roll back: a failed deploy is never rolled back by itself", w)
		}
		if rest, ok := strings.CutPrefix(w, "POST /services/"); ok {
			id, _, _ := strings.Cut(rest, "/")
			updates[id]++
		}
	}
	// api was updated once, mid was tried once and refused, web was never tried: nothing was retried
	// and nothing was put back.
	if updates["svc-api"] != 1 || updates["svc-mid"] != 1 || updates["svc-web"] != 0 {
		t.Errorf("service writes by service = %v, want api 1, mid 1 (refused), web 0", updates)
	}
	if ups := rig.fake.ServiceUpdates(); len(ups) != 1 || ups[0].ServiceID != "svc-api" {
		t.Errorf("accepted updates = %+v, want only api's", ups)
	}
}
