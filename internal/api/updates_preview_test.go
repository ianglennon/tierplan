// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// What the compact confirm shows (plan 12-16, D-05, IMG-06). Docker is a fake daemon behind the real monitor, aggregator and cluster
// reader; the registry is a scripted Heads; the applier, the stored checks, the operation lock and the permission checks are the
// real ones.

type previewAnswer struct {
	Items []struct {
		Kind, Name, Stack, Image string
		FromDigest, ToDigest     string
		Platforms                []string
		ChangedPlatforms         []string
	} `json:"items"`
	Warnings []struct {
		Workload string `json:"workload"`
		Code     string `json:"code"`
	} `json:"warnings"`
	Lock *struct {
		Kind, By, Started, Workload, Stack string
	} `json:"lock"`
	Offered bool   `json:"offered"`
	Refusal string `json:"refusal"`
}

func (p previewAnswer) codes() []string {
	out := []string{}
	for _, w := range p.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func (f *envFix) preview(t *testing.T, e envBody, name string) *envRec {
	t.Helper()
	return f.req(t, http.MethodGet, "/api/env/"+e.ID+"/updates/preview?kind=service&name="+url.QueryEscape(name), "")
}

// riskyService is a service of one replica that stops before its replacement starts, has no health check, publishes a port in
// host mode and mounts a volume.
func riskyService(name string, running byte) dockertest.FakeService {
	svc := plainService(name, running)
	one := uint64(1)
	svc.Spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}}
	svc.Spec.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1, Order: swarm.UpdateOrderStopFirst}
	svc.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{PublishedPort: 8080, TargetPort: 80, PublishMode: swarm.PortConfigPublishModeHost}}}
	svc.Running, svc.Desired = 1, 1
	return svc
}

// The tracer (plan 12-16, D-05): opening Update on a service of one replica lists its digest change and warns it will be down
// briefly, has no health check, holds a host port and mounts a volume.
func TestPreviewOneReplicaWarnsDowntime(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	svc := riskyService("web", 'a')
	svc.Spec.TaskTemplate.ContainerSpec.Mounts = append(svc.Spec.TaskTemplate.ContainerSpec.Mounts, mount.Mount{Type: mount.TypeVolume, Source: "data", Target: "/data"})
	f, e := updateEnvOf(t, heads, nil, svc)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	rec := f.preview(t, e, "web")
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	got := decode[previewAnswer](t, rec)
	if !got.Offered || got.Refusal != "" || len(got.Items) != 1 {
		t.Fatalf("preview = %+v, want an offered update of one item", got)
	}
	it := got.Items[0]
	if it.Kind != "service" || it.Name != "web" || it.Image != "registry.test/team/web:1" || it.FromDigest != updDigest('a') || it.ToDigest != updDigest('b') {
		t.Errorf("item = %+v, want web from a to b, the image without a digest", it)
	}
	if it.Platforms == nil || it.ChangedPlatforms == nil {
		t.Errorf("item = %+v, want lists and not null", it)
	}
	if want := []string{"downtime", "no_healthcheck", "host_ports", "volumes"}; !slices.Equal(got.codes(), want) {
		t.Errorf("warnings = %v, want %v", got.codes(), want)
	}
	if got.Lock != nil {
		t.Errorf("lock = %+v, want none while nothing holds the environment", got.Lock)
	}
	// Opening the confirm asks no registry and changes nothing.
	if n := len(f.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d after a preview, want none", n)
	}
}

// liveInfo is the update object with what is happening now.
type liveInfo struct {
	updateInfo
	JobID  string `json:"jobId"`
	Holder *struct {
		Kind, By, Started, Workload, Stack string
	} `json:"holder"`
}

type liveList struct {
	Items []struct {
		Name   string    `json:"name"`
		Update *liveInfo `json:"update"`
	} `json:"items"`
}

func (f *envFix) liveOf(t *testing.T, e envBody, name string) *liveInfo {
	t.Helper()
	for _, it := range decode[liveList](t, f.get(t, e, "services")).Items {
		if it.Name == name {
			if it.Update == nil {
				t.Fatalf("service %q has no update object", name)
			}
			return it.Update
		}
	}
	t.Fatalf("no service %q", name)
	return nil
}

// While another operation holds the environment, a service an update is offered for reads Busy with who holds it; everything the
// check found that is not an offer stays as it is, and the count of updates does not drop (plan 12-16, UI-SPEC "Busy").
func TestBusyBadgeFollowsTheLock(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('a')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'), plainService("api", 'a'))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	if u := f.liveOf(t, e, "web"); u.State != "update_available" || u.Holder != nil {
		t.Fatalf("before the lock = %+v, want update_available", u)
	}

	release, _, ok := f.stackLocks.TryAcquire(e.ID, stackops.Holder{Kind: stackops.HolderKindJanitor, RunID: "run-1", Started: time.Now().UTC()})
	if !ok {
		t.Fatal("could not take the lock")
	}
	web := f.liveOf(t, e, "web")
	if web.State != "busy" || web.Holder == nil || web.Holder.Kind != "janitor" || web.Holder.Started == "" || web.RemoteDigest != updDigest('b') {
		t.Errorf("web under the janitor = %+v (holder %+v), want busy, held by the janitor, still offered b", web, web.Holder)
	}
	if api := f.liveOf(t, e, "api"); api.State != "up_to_date" || api.Holder != nil {
		t.Errorf("api under the janitor = %+v, want up_to_date: only an offered update reads busy", api)
	}
	if got := decode[updateCheckAnswer](t, f.checkNow(t, e)); got.Updates != 1 {
		t.Errorf("check now counts %d updates under a lock, want 1: busy hides no update", got.Updates)
	}
	// Skipping what is offered is refused the way an update is.
	if rec := f.skip(t, e, http.MethodPost, "web"); rec.code != http.StatusConflict || rec.ptyp != "operation_locked" {
		t.Errorf("skip under the lock: %d %q, want 409 operation_locked", rec.code, rec.ptyp)
	}
	if p := decode[previewAnswer](t, f.preview(t, e, "web")); p.Lock == nil || p.Lock.Kind != "janitor" || !p.Offered {
		t.Errorf("preview under the lock = %+v, want the janitor named and the update still offered", p)
	}

	release()
	if u := f.liveOf(t, e, "web"); u.State != "update_available" || u.Holder != nil {
		t.Errorf("after the lock cleared = %+v, want update_available again", u)
	}
	if p := decode[previewAnswer](t, f.preview(t, e, "web")); p.Lock != nil {
		t.Errorf("preview after the lock cleared names %+v, want no holder", p.Lock)
	}
}

// A service whose update is running reads Updating with the job to follow, whatever else holds (plan 12-16, IMG-06).
func TestUpdatingBadgeFollowsTheRun(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}, "team/api": {digest: updDigest('b')}})
	f, e := updateEnvOf(t, heads, nil, plainService("web", 'a'), plainService("api", 'a'))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	runs := updates.RunStore{Q: f.st.Q, W: f.st.Write}
	if err := runs.Insert(context.Background(), updates.Run{
		ID: "run-1", EnvID: e.ID, Kind: "service", Workload: "web", State: updates.RunRunning, JobID: "job-1",
	}); err != nil {
		t.Fatal(err)
	}
	release, _, ok := f.stackLocks.TryAcquire(e.ID, stackops.Holder{Kind: stackops.HolderKindImageUpdate, Workload: "web", By: "ann", Started: time.Now().UTC()})
	if !ok {
		t.Fatal("could not take the lock")
	}
	defer release()

	web := f.liveOf(t, e, "web")
	if web.State != "updating" || web.JobID != "job-1" || web.Holder != nil {
		t.Errorf("web = %+v, want updating with its job", web)
	}
	api := f.liveOf(t, e, "api")
	if api.State != "busy" || api.Holder == nil || api.Holder.Kind != "update" || api.Holder.Workload != "web" || api.Holder.By != "ann" {
		t.Errorf("api = %+v (holder %+v), want busy, held by the update of web", api, api.Holder)
	}
}
