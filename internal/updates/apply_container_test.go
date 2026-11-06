// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The update of a plain container (plan 12-20, IMG-06, D-07, D-12, D-13, T-12-56, T-12-57, T-12-58). Docker is a fake engine behind the
// real monitor and aggregator; the registry is a scripted Heads; the applier, its run store, the job hub, the operation lock and the
// audit log are the real ones. Time is the rig's clock, which answers a wait at once.

const (
	appImage   = "registry.test/team/app:1"
	appID      = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	appShort   = "cccccccccccc"
	appNewName = "app-tierplan-new-" + appShort
	appOldName = "app-tierplan-old-" + appShort
)

// image is the repository, tag and digest an update of app to digest sets.
func pinnedApp(digest string) string { return appImage + "@" + digest }

// appContainer is a container made with docker run: an environment, a command, a label, a restart policy, a network with an alias,
// a named volume and an anonymous one.
func appContainer(state string) dockertest.FakeContainer {
	doc := container.InspectResponse{
		Config: &container.Config{
			Image: appImage, Hostname: appShort, Env: []string{"MODE=prod"}, Cmd: []string{"serve"}, Labels: map[string]string{"team": "core"},
			Volumes: map[string]struct{}{"/cache": {}},
		},
		HostConfig: &container.HostConfig{NetworkMode: "appnet", RestartPolicy: container.RestartPolicy{Name: "always"}, Binds: []string{"appdata:/data"}},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "appdata", Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "anon0123", Destination: "/cache", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"appnet": {NetworkID: "net-app", Aliases: []string{"app", appShort}},
		}},
	}
	return dockertest.FakeContainer{
		ID: appID, Names: []string{"/app"}, Image: appImage, ImageID: imgID('1'), State: state, Status: "Up 1 hour",
		Labels: map[string]string{"team": "core"}, Mounts: []string{"appdata", "anon0123"},
		Networks: map[string]dockertest.FakeEndpoint{"appnet": {NetworkID: "net-app", Aliases: []string{"app"}}},
		Inspect:  dockertest.FakeInspect{Document: &doc, ImageID: imgID('1')},
	}
}

// imgID is an image ID made of one repeated character.
func imgID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// containerRig is a rig whose engine holds these containers, the image of appImage at digest a and the network appnet. tune changes
// the engine's configuration further.
func containerRig(t *testing.T, tune func(*dockertest.FakeConfig), containers ...dockertest.FakeContainer) *applyRig {
	t.Helper()
	return newApplyRig(t, func(c *dockertest.FakeConfig) {
		c.Containers = containers
		c.Images = []dockertest.FakeImage{{
			ID: imgID('1'), RepoTags: []string{appImage}, RepoDigests: []string{"registry.test/team/app@" + dg('a')},
			Env: []string{"PATH=/bin", "VERSION=1", "MODE=dev"}, Cmd: []string{"nginx"}, Labels: map[string]string{"org.version": "1", "team": "image"},
		}}
		c.Networks = []dockertest.FakeNetwork{{ID: "net-app", Name: "appnet", Driver: "bridge", Scope: "local"}}
		if tune != nil {
			tune(c)
		}
	})
}

// containerRequest is the update of a container as an Operator asks for it, confirmed at the digest.
func (r *applyRig) containerRequest(name, confirmed string) updates.Request {
	req := r.request(name, confirmed)
	req.Kind = "container"
	return req
}

// writeKinds names the writes the engine served in a word each: the ones an update of a container is made of.
func writeKinds(writes []string) []string {
	var out []string
	for _, w := range writes {
		method, path, _ := strings.Cut(w, " ")
		path, _, _ = strings.Cut(path, "?")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case path == "/images/create":
			out = append(out, "pull")
		case path == "/containers/create":
			out = append(out, "create")
		case method == http.MethodDelete && len(parts) == 2 && parts[0] == "containers":
			out = append(out, "remove")
		case len(parts) == 3 && parts[0] == "containers":
			out = append(out, parts[2])
		}
	}
	return out
}

// stepStates are the states of a job's steps by ID.
func stepStates(job jobs.Job) map[string]string {
	out := map[string]string{}
	for _, it := range job.Items {
		out[it.ID] = it.State
	}
	return out
}

// containersNow are the names of the containers the engine lists now, by name, with their state.
func (r *applyRig) containersNow(t *testing.T) map[string]string {
	t.Helper()
	snap, _, err := r.applier.Aggregator.FreshSnapshot(testsupport.Context(t), r.envID)
	if err != nil {
		t.Fatalf("reading the containers: %v", err)
	}
	out := map[string]string{}
	for _, c := range snap.Containers {
		out[strings.TrimPrefix(c.Name, "/")] = c.State + " " + c.ID
	}
	return out
}

// An agent that refuses the replacement by its host-access policy stops the update before the old container is touched: nothing was
// stopped, renamed or removed (T-12-56).
func TestCreateRefusedChangesNothing(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]struct {
		fake   dockertest.FakeRefusal
		reason string
	}{
		"host policy": {dockertest.FakeRefusal{Status: http.StatusForbidden, Message: "agent_policy_denied: host_policy: POST /containers/create"}, updates.ReasonAgentHostPolicy},
		"bad config":  {dockertest.FakeRefusal{Status: http.StatusBadRequest, Message: "invalid config: engine words that never reach a person"}, updates.ReasonCreateRefused},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := containerRig(t, func(c *dockertest.FakeConfig) {
				c.RefuseCreate = map[string]dockertest.FakeRefusal{appNewName: refusal.fake}
			}, appContainer("running"))
			r.offer("app", dg('b'))

			job := r.run(t, r.containerRequest("app", dg('b')))
			if job.State == jobs.JobDone {
				t.Fatalf("job = %s %+v, want it to fail", job.State, job.Items)
			}
			if got, want := writeKinds(r.fake.Writes()), []string{"pull", "create"}; !reflect.DeepEqual(got, want) {
				t.Errorf("the engine was asked %v, want %v: nothing may be stopped or renamed after a refused create", got, want)
			}
			run := r.runRow(t, job)
			if run.State != updates.RunFailed || run.Reason != refusal.reason {
				t.Errorf("run = %s %q, want failed %q", run.State, run.Reason, refusal.reason)
			}
			if now := r.containersNow(t); !reflect.DeepEqual(now, map[string]string{"app": "running " + appID}) {
				t.Errorf("containers = %v, want only the old one, still running", now)
			}
			if s := stepStates(job); s["create"] != "failed" || s["swap"] != "skipped" || s["start"] != "skipped" {
				t.Errorf("steps = %v, want create failed and the rest skipped", s)
			}
			for _, it := range job.Items {
				if strings.Contains(it.Message, "engine words") || strings.Contains(it.Detail, "engine words") {
					t.Errorf("item %s carries the engine's own words: %+v", it.ID, it)
				}
			}
			if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != refusal.reason {
				t.Errorf("failure audit rows = %+v, want one with reason %s", rows, refusal.reason)
			}
		})
	}
}

// A replacement that exits at once is removed, the old container gets its name back and is started again, and the run says why
// (D-12, T-12-58).
func TestContainerFailureRestoresOld(t *testing.T) {
	t.Parallel()
	r := containerRig(t, func(c *dockertest.FakeConfig) {
		c.ExitOnStart = map[string]bool{pinnedApp(dg('b')): true}
	}, appContainer("running"))
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if job.State == jobs.JobDone {
		t.Fatalf("job = %s %+v, want it to fail", job.State, job.Items)
	}
	if got, want := writeKinds(r.fake.Writes()), []string{"pull", "create", "stop", "rename", "rename", "start", "remove", "rename", "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the engine was asked %v, want %v: the replacement removed, the old one renamed back and started", got, want)
	}
	run := r.runRow(t, job)
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart || run.OldContainer != appID {
		t.Errorf("run = %s %q old %q, want rolled_back didnt_start that recorded the old container", run.State, run.Reason, run.OldContainer)
	}
	if now := r.containersNow(t); !reflect.DeepEqual(now, map[string]string{"app": "running " + appID}) {
		t.Errorf("containers = %v, want only the old container, under its own name and running", now)
	}
	renames := r.fake.ContainerRenames()
	if len(renames) != 3 || renames[2].ID != appID || renames[2].To != "app" {
		t.Errorf("renames = %+v, want the old container renamed back last", renames)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after the failure")
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["kind"] != "container" || rows[0]["reason"] != updates.ReasonDidntStart {
		t.Errorf("failure audit rows = %+v, want one for the container with reason didnt_start", rows)
	}
}

// withHealthcheck is the container with a health check of its own.
func withHealthcheck(c dockertest.FakeContainer, hc *container.HealthConfig) dockertest.FakeContainer {
	c.Inspect.Document.Config.Healthcheck = hc
	return c
}

// A replacement with a health check that is not healthy by the end of the window is removed and the old container put back; one the
// check fails reads unhealthy, one still starting when time is up reads timeout.
func TestContainerUnhealthyRestoresOld(t *testing.T) {
	t.Parallel()
	// wall-clock: a field of the image's health check, which the fake engine stores and never runs.
	hc := &container.HealthConfig{Test: []string{"CMD", "true"}, Interval: time.Second, Timeout: time.Second, Retries: 1}
	for health, reason := range map[string]string{"unhealthy": updates.ReasonUnhealthy, "starting": updates.ReasonTimeout} {
		t.Run(health, func(t *testing.T) {
			t.Parallel()
			r := containerRig(t, func(c *dockertest.FakeConfig) {
				c.HealthOnStart = map[string]string{pinnedApp(dg('b')): health}
			}, withHealthcheck(appContainer("running"), hc))
			r.offer("app", dg('b'))

			job := r.run(t, r.containerRequest("app", dg('b')))
			if job.State == jobs.JobDone {
				t.Fatalf("job = %s %+v, want it to fail", job.State, job.Items)
			}
			run := r.runRow(t, job)
			if run.State != updates.RunRolledBack || run.Reason != reason {
				t.Errorf("run = %s %q, want rolled_back %s", run.State, run.Reason, reason)
			}
			if s := stepStates(job); s["health"] != "failed" || s["remove_old"] != "skipped" {
				t.Errorf("steps = %v, want the health step failed and the old container never removed", s)
			}
			if now := r.containersNow(t); !reflect.DeepEqual(now, map[string]string{"app": "running " + appID}) {
				t.Errorf("containers = %v, want only the old container, running", now)
			}
			if reason == updates.ReasonTimeout {
				// The window is the converge timeout here (30 seconds): the clock moved on by about that, no more.
				if waited := r.clock.Now().Sub(t0); waited < 30*time.Second || waited > 40*time.Second {
					t.Errorf("waited %v of the clock's time, want the window of 30 seconds", waited)
				}
			}
		})
	}
}

// A health check that needs longer than five minutes to pass or fail is waited for as long as it needs, up to the converge timeout.
func TestContainerHealthWindowFollowsTheHealthcheck(t *testing.T) {
	t.Parallel()
	hc := &container.HealthConfig{Test: []string{"CMD", "true"}, StartPeriod: 7 * time.Minute, Interval: time.Minute, Timeout: time.Minute, Retries: 2}
	r := containerRig(t, func(c *dockertest.FakeConfig) {
		c.HealthOnStart = map[string]string{pinnedApp(dg('b')): "starting"}
	}, withHealthcheck(appContainer("running"), hc))
	r.applier.ConvergeTimeout = time.Hour
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if run := r.runRow(t, job); run.Reason != updates.ReasonTimeout {
		t.Fatalf("run = %s %q, want a timeout", run.State, run.Reason)
	}
	// 7 minutes of start period and (1 + 1 minute) for each of 3 tries.
	if waited := r.clock.Now().Sub(t0); waited < 13*time.Minute || waited > 14*time.Minute {
		t.Errorf("waited %v of the clock's time, want the 13 minutes the health check needs", waited)
	}
}

// A container with no health check is up when it runs: nothing waits for a health that does not exist.
func TestContainerNoHealthcheckRunningIsEnough(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"))
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s %+v, want done", job.State, job.Items)
	}
	if s := stepStates(job); !reflect.DeepEqual(s, map[string]string{"check": "done", "pull": "done", "create": "done", "swap": "done", "start": "done", "remove_old": "done"}) {
		t.Errorf("steps = %v, want no health step", s)
	}
	run := r.runRow(t, job)
	if run.State != updates.RunDone || run.OldContainer != appID {
		t.Errorf("run = %s old %q, want done", run.State, run.OldContainer)
	}
	now := r.containersNow(t)
	if len(now) != 1 || !strings.HasPrefix(now["app"], "running ") || strings.HasSuffix(now["app"], appID) {
		t.Errorf("containers = %v, want the replacement alone, under the old name and running", now)
	}
}

// With a health check the update waits for it, and the replacement passes when it reports healthy.
func TestContainerHealthyIsDone(t *testing.T) {
	t.Parallel()
	hc := &container.HealthConfig{Test: []string{"CMD", "true"}}
	r := containerRig(t, func(c *dockertest.FakeConfig) {
		c.HealthOnStart = map[string]string{pinnedApp(dg('b')): "healthy"}
	}, withHealthcheck(appContainer("running"), hc))
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s %+v, want done", job.State, job.Items)
	}
	if s := stepStates(job); s["health"] != "done" || s["remove_old"] != "done" {
		t.Errorf("steps = %v, want health and remove_old done", s)
	}
}

// What the old image gave the container follows the new image; what the person gave it stays (RecreateSpec keeps the inspect, and the
// defaults of the old image are taken out of it).
func TestUpdateFollowsTheNewImageForWhatTheImageGave(t *testing.T) {
	t.Parallel()
	old := appContainer("running")
	cfg := old.Inspect.Document.Config
	cfg.Env = []string{"PATH=/bin", "VERSION=1", "MODE=prod"}
	cfg.Cmd = []string{"nginx"}
	cfg.Labels = map[string]string{"org.version": "1", "team": "core"}
	old.Labels = cfg.Labels
	r := containerRig(t, nil, old)
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s %+v, want done", job.State, job.Items)
	}
	creates := r.fake.ContainerCreates()
	if len(creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(creates))
	}
	c := creates[0].Config
	if !reflect.DeepEqual(c.Env, []string{"MODE=prod"}) {
		t.Errorf("env = %v, want only what the person set: the image's PATH and VERSION come from the new image", c.Env)
	}
	if len(c.Cmd) != 0 {
		t.Errorf("cmd = %v, want the new image's command (the old one was the old image's)", c.Cmd)
	}
	if !reflect.DeepEqual(c.Labels, map[string]string{"team": "core"}) {
		t.Errorf("labels = %v, want the person's label only", c.Labels)
	}
}

// ForgetImageDefaults takes out exactly what the old image gave and nothing the person chose.
func TestForgetImageDefaults(t *testing.T) {
	t.Parallel()
	hc := &container.HealthConfig{Test: []string{"CMD", "true"}}
	image := &container.Config{
		Env: []string{"PATH=/bin", "V=1"}, Cmd: []string{"run"}, Entrypoint: []string{"/e"}, Labels: map[string]string{"a": "1", "b": "2"},
		Healthcheck: hc, WorkingDir: "/w", User: "u", StopSignal: "SIGINT",
	}
	for _, tc := range []struct {
		name string
		cfg  container.Config
		want container.Config
	}{
		{
			name: "everything the image gave goes",
			cfg: container.Config{Env: []string{"PATH=/bin", "V=1"}, Cmd: []string{"run"}, Entrypoint: []string{"/e"}, Labels: map[string]string{"a": "1"},
				Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}}, WorkingDir: "/w", User: "u", StopSignal: "SIGINT"},
			want: container.Config{Env: []string{}, Labels: map[string]string{}},
		},
		{
			name: "what the person set stays, a changed value of the image's key included",
			cfg: container.Config{Env: []string{"PATH=/bin", "V=2", "MINE=1"}, Entrypoint: []string{"/e"}, Cmd: []string{"other"}, Labels: map[string]string{"a": "9", "mine": "x"},
				WorkingDir: "/mine", User: "me"},
			want: container.Config{Env: []string{"V=2", "MINE=1"}, Cmd: []string{"other"}, Labels: map[string]string{"a": "9", "mine": "x"}, WorkingDir: "/mine", User: "me"},
		},
		{
			name: "an entrypoint of the person's keeps the command, which the engine would not have taken from the image",
			cfg:  container.Config{Entrypoint: []string{"/mine"}, Cmd: []string{"run"}},
			want: container.Config{Entrypoint: []string{"/mine"}, Cmd: []string{"run"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			envBefore := append([]string(nil), cfg.Env...)
			updates.ForgetImageDefaults(&cfg, image)
			if !reflect.DeepEqual(cfg, tc.want) {
				t.Errorf("config = %+v, want %+v", cfg, tc.want)
			}
			if !reflect.DeepEqual(tc.cfg.Env, envBefore) {
				t.Errorf("the input's environment was changed: %v", tc.cfg.Env)
			}
		})
	}
	// An image that could not be read leaves the config as it is.
	cfg := container.Config{Env: []string{"A=1"}, Cmd: []string{"x"}}
	updates.ForgetImageDefaults(&cfg, nil)
	if !reflect.DeepEqual(cfg, container.Config{Env: []string{"A=1"}, Cmd: []string{"x"}}) {
		t.Errorf("config = %+v after an unknown image, want it kept", cfg)
	}
}

// A stopped container is recreated on the new image and stays stopped.
func TestStoppedContainerStaysStopped(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("exited"))
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if job.State == jobs.JobFailed {
		t.Fatalf("job = %s %+v, want it to work", job.State, job.Items)
	}
	if got, want := writeKinds(r.fake.Writes()), []string{"pull", "create", "rename", "rename", "remove"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the engine was asked %v, want %v: no stop and no start for a container that was not running", got, want)
	}
	if run := r.runRow(t, job); run.State != updates.RunDone {
		t.Errorf("run = %s %q, want done", run.State, run.Reason)
	}
	now := r.containersNow(t)
	if len(now) != 1 || !strings.HasPrefix(now["app"], "created ") {
		t.Errorf("containers = %v, want the replacement alone, not started", now)
	}
}

// If the old container cannot be removed after the replacement runs, the update still worked: the run is done and the step says
// what is left behind.
func TestOldContainerThatCannotBeRemovedIsSaid(t *testing.T) {
	t.Parallel()
	r := containerRig(t, func(c *dockertest.FakeConfig) {
		c.RefuseRemove = map[string]dockertest.FakeRefusal{appID: {Status: http.StatusInternalServerError, Message: "driver failed: engine words"}}
	}, appContainer("running"))
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	if run := r.runRow(t, job); run.State != updates.RunDone {
		t.Fatalf("run = %s %q, want done: the new container runs", run.State, run.Reason)
	}
	var item jobs.Item
	for _, it := range job.Items {
		if it.ID == "remove_old" {
			item = it
		}
	}
	if item.State != jobs.ItemFailed || item.Code != "remove_old_failed" || strings.Contains(item.Message+item.Detail, "engine words") {
		t.Errorf("remove_old = %+v, want failed with TierPlan's own words", item)
	}
	if now := r.containersNow(t); len(now) != 2 {
		t.Errorf("containers = %v, want the replacement and the old container under its temporary name", now)
	}
}

// When the replacement fails and the old container cannot be put back either, the run says so.
func TestRestoreFailureIsSaidSo(t *testing.T) {
	t.Parallel()
	firstNew := fmt.Sprintf("%064x", uint64(0xfa4ec0de00000001))
	r := containerRig(t, func(c *dockertest.FakeConfig) {
		c.ExitOnStart = map[string]bool{pinnedApp(dg('b')): true}
		c.RefuseRemove = map[string]dockertest.FakeRefusal{firstNew: {Status: http.StatusInternalServerError, Message: "stuck"}}
	}, appContainer("running"))
	r.offer("app", dg('b'))

	job := r.run(t, r.containerRequest("app", dg('b')))
	run := r.runRow(t, job)
	if run.State != updates.RunRollbackFailed || run.Reason != updates.ReasonRestoreFailed {
		t.Errorf("run = %s %q, want rollback_failed restore_failed", run.State, run.Reason)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after the failure")
	}
}

// What is not updatable here is refused before anything is written: TierPlan's own server, a task container, a container of a
// Compose project TierPlan does not manage, one of a stack TierPlan manages (updated through the stack), and one that is not there.
func TestContainerRefusals(t *testing.T) {
	t.Parallel()
	label := func(c dockertest.FakeContainer, name string, l map[string]string) dockertest.FakeContainer {
		c.ID = name + strings.Repeat("0", 64-len(name))
		c.Names = []string{"/" + name}
		c.Labels = l
		return c
	}
	self := label(appContainer("running"), "self", nil)
	task := label(appContainer("running"), "task", map[string]string{"com.docker.swarm.service.id": "svc1"})
	compose := label(appContainer("running"), "compose", map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "web"})
	managed := label(appContainer("running"), "managed", map[string]string{"com.docker.compose.project": "solo", "com.docker.compose.service": "web", "tierplan.stack.id": "stack-1"})
	r := containerRig(t, nil, self, task, compose, managed)
	r.applier.SelfContainerID = self.ID[:12]
	r.applier.ManagedStack = func(_ context.Context, _, id string) (bool, error) { return id == "stack-1", nil }
	r.offer("app", dg('b'))

	for name, code := range map[string]string{
		"self": updates.NotUpdatableAgent, "task": updates.NotUpdatableTaskContainer, "compose": updates.NotUpdatableComposeUnmanaged,
		"managed": updates.NotUpdatableManaged,
	} {
		_, err := r.applier.Start(testsupport.Context(t), r.containerRequest(name, ""))
		var nu *updates.NotUpdatableError
		if !errors.As(err, &nu) || nu.Code != code {
			t.Errorf("Start(%s) = %v, want not updatable: %s", name, err, code)
		}
	}
	if _, err := r.applier.Start(testsupport.Context(t), r.containerRequest("nobody", "")); !errors.Is(err, updates.ErrWorkloadGone) {
		t.Errorf("Start(nobody) = %v, want ErrWorkloadGone", err)
	}
	if got := writeKinds(r.fake.Writes()); len(got) != 0 {
		t.Errorf("the engine was asked %v after refusals, want nothing", got)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after refusals")
	}
	if runs, err := r.runs.Running(context.Background(), r.envID); err != nil || len(runs) != 0 {
		t.Errorf("runs = %+v %v, want none", runs, err)
	}
}

// A digest that moved since the confirm is refused before anything is created, with the refreshed one.
func TestContainerRefusesAMovedDigest(t *testing.T) {
	t.Parallel()
	r := containerRig(t, nil, appContainer("running"))
	r.offer("app", dg('c'))

	_, err := r.applier.Start(testsupport.Context(t), r.containerRequest("app", dg('b')))
	var moved *updates.MovedError
	if !errors.As(err, &moved) || len(moved.Items) != 1 || moved.Items[0].Service != "app" || moved.Items[0].ToDigest != dg('c') {
		t.Fatalf("Start = %v, want a MovedError naming app at c", err)
	}
	if got := writeKinds(r.fake.Writes()); len(got) != 0 {
		t.Errorf("the engine was asked %v, want nothing", got)
	}
}

// The warnings of updating a container come in the order of the confirm and read the container's own health check before the image's.
func TestContainerWarnings(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	vol := []container.MountPoint{{Type: mount.TypeVolume, Name: "v", Destination: "/v"}}
	bind := []container.MountPoint{{Type: mount.TypeBind, Source: "/srv", Destination: "/s"}}
	withHC := func(test ...string) *container.Config {
		return &container.Config{Healthcheck: &container.HealthConfig{Test: test}}
	}
	for _, tc := range []struct {
		name  string
		insp  container.InspectResponse
		image *bool
		want  []string
	}{
		{"nothing known: no check, no volume", container.InspectResponse{}, nil, []string{"container_downtime", "no_healthcheck"}},
		{"a check of its own", container.InspectResponse{Config: withHC("CMD", "true")}, nil, []string{"container_downtime"}},
		{"a check of its image's", container.InspectResponse{}, &yes, []string{"container_downtime"}},
		{"an image without one", container.InspectResponse{}, &no, []string{"container_downtime", "no_healthcheck"}},
		{"its check switched off wins over the image's", container.InspectResponse{Config: withHC("NONE")}, &yes, []string{"container_downtime", "no_healthcheck"}},
		{"a volume", container.InspectResponse{Config: withHC("CMD", "true"), Mounts: vol}, nil, []string{"container_downtime", "volumes"}},
		{"a bind is not a volume", container.InspectResponse{Config: withHC("CMD", "true"), Mounts: bind}, nil, []string{"container_downtime"}},
		{"all three", container.InspectResponse{Mounts: vol}, nil, []string{"container_downtime", "no_healthcheck", "volumes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, w := range updates.ContainerWarnings("app", tc.insp, tc.image) {
				if w.Workload != "app" {
					t.Errorf("warning %+v is about %q, want app", w, w.Workload)
				}
				got = append(got, w.Code)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("warnings = %v, want %v", got, tc.want)
			}
		})
	}
}

// containerFailureOf is the failure remembered for a container, and whether there is one.
func (r *applyRig) containerFailureOf(t *testing.T, name string) (updates.Failure, bool) {
	t.Helper()
	all, err := r.failures.ForEnv(context.Background(), r.envID)
	if err != nil {
		t.Fatalf("read the failures: %v", err)
	}
	f, ok := all[updates.FailureKey("container", name)]
	return f, ok
}

// A container whose replacement failed is recorded the way a service is: the restore its recreate made ends the run rolled back, the
// digest that failed is remembered and not offered again, and both the failure and the rollback are audited; a restore that failed
// is rollback_failed all the way through (plan 12-22, IMG-07, D-12, D-14).
func TestContainerFailureRecorded(t *testing.T) {
	t.Parallel()
	t.Run("restored", func(t *testing.T) {
		t.Parallel()
		r := containerRig(t, func(c *dockertest.FakeConfig) {
			c.ExitOnStart = map[string]bool{pinnedApp(dg('b')): true}
		}, appContainer("running"))
		r.offer("app", dg('b'))

		job := r.run(t, r.containerRequest("app", dg('b')))
		if run := r.runRow(t, job); run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart {
			t.Fatalf("run = %s %q, want rolled_back with didnt_start", run.State, run.Reason)
		}
		f, ok := r.containerFailureOf(t, "app")
		if !ok || f.Digest != dg('b') || f.Reason != updates.ReasonDidntStart || f.RollbackFailed {
			t.Errorf("failure = %+v %v, want the new digest with didnt_start, not rollback_failed", f, ok)
		}
		if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["kind"] != "container" || rows[0]["reason"] != updates.ReasonDidntStart {
			t.Errorf("failure audit rows = %+v, want one for the container", rows)
		}
		if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "success" || rows[0]["kind"] != "container" || rows[0]["workload"] != "app" {
			t.Errorf("rollback audit rows = %+v, want one success for the container", rows)
		}
		sv, found, err := r.applier.Service.Find(testsupport.Context(t), r.envID, "container", "app")
		if err != nil || !found || sv.Verdict.State != updates.StateRolledBack || sv.Verdict.Failure == nil || sv.Verdict.Failure.Digest != dg('b') {
			t.Errorf("verdict = %+v %v %v, want rolled_back for the digest that failed", sv.Verdict, found, err)
		}
	})

	t.Run("restore failed", func(t *testing.T) {
		t.Parallel()
		firstNew := fmt.Sprintf("%064x", uint64(0xfa4ec0de00000001))
		r := containerRig(t, func(c *dockertest.FakeConfig) {
			c.ExitOnStart = map[string]bool{pinnedApp(dg('b')): true}
			c.RefuseRemove = map[string]dockertest.FakeRefusal{firstNew: {Status: http.StatusInternalServerError, Message: "stuck"}}
		}, appContainer("running"))
		r.offer("app", dg('b'))

		job := r.run(t, r.containerRequest("app", dg('b')))
		if run := r.runRow(t, job); run.State != updates.RunRollbackFailed || run.Reason != updates.ReasonRestoreFailed {
			t.Fatalf("run = %s %q, want rollback_failed with restore_failed", run.State, run.Reason)
		}
		f, ok := r.containerFailureOf(t, "app")
		if !ok || !f.RollbackFailed || f.Digest != dg('b') || f.Reason != updates.ReasonDidntStart {
			t.Errorf("failure = %+v %v, want the new digest with rollback_failed and the reason the update failed", f, ok)
		}
		if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonDidntStart {
			t.Errorf("failure audit rows = %+v, want one with the reason the update failed", rows)
		}
		if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 1 || rows[0]["_result"] != "failure" {
			t.Errorf("rollback audit rows = %+v, want one failure", rows)
		}
	})

	t.Run("a refusal at create is no rollback", func(t *testing.T) {
		t.Parallel()
		r := containerRig(t, func(c *dockertest.FakeConfig) {
			c.RefuseCreate = map[string]dockertest.FakeRefusal{appNewName: {Status: http.StatusBadRequest, Message: "bad config"}}
		}, appContainer("running"))
		r.offer("app", dg('b'))
		job := r.run(t, r.containerRequest("app", dg('b')))
		if run := r.runRow(t, job); run.State != updates.RunFailed {
			t.Fatalf("run = %s %q, want failed: nothing was changed, so nothing was rolled back", run.State, run.Reason)
		}
		if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 0 {
			t.Errorf("rollback audit rows = %+v, want none", rows)
		}
		if f, ok := r.containerFailureOf(t, "app"); ok {
			t.Errorf("failure = %+v, want none: only a pull that failed or a rollback is remembered", f)
		}
	})
}
