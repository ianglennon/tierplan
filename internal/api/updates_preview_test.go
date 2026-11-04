// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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
