// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The typed detail of one container and the raw inspect document (plan 04-10, D-04).

func detailFake(t *testing.T) *dockerx.Client {
	t.Helper()
	timeout := 20
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{{
		ID: "web-id", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 3 hours",
		Labels: map[string]string{"com.docker.compose.project": "shop"},
		Ports: []dockertest.FakePort{
			{IP: "0.0.0.0", Private: 80, Public: 8080, Type: "tcp"},
			{Private: 443, Type: "tcp"},
			{IP: "::", Private: 53, Public: 5353, Type: "udp"},
		},
		Mounts:      []string{"data", "/srv/site"},
		Networks:    map[string]dockertest.FakeEndpoint{"front": {NetworkID: "n1", IPv4: "10.0.0.2", MAC: "02:42:0a:00:00:02", Aliases: []string{"web", "site"}}, "back": {NetworkID: "n2"}},
		Health:      "healthy",
		StopTimeout: &timeout, StopSignal: "SIGQUIT",
		Inspect: dockertest.FakeInspect{
			Env: []string{"A=1", "TOKEN=a=b"}, Cmd: []string{"nginx", "-g", "daemon off;"}, Entrypoint: []string{"/docker-entrypoint.sh"},
			WorkingDir: "/app", User: "www", ExitCode: 3, StartedAt: "2026-10-01T10:00:00Z", OOMKilled: true, RestartCount: 2,
			LogType: "syslog", LogOptions: map[string]string{"splunk-token": "c0ffee-secret"},
			MountModes:  map[string]string{"/srv/site": "ro"},
			MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000,
		},
	}}, RestartPolicies: map[string]string{"web-id": "on-failure:3"}})
	return newClient(t, fake)
}

func TestContainerDetailMapsTheInspectDocument(t *testing.T) {
	d, raw, err := detailFake(t).ContainerDetail(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "web-id" || d.Name != "web" || d.Image != "nginx:1.27" || d.Status != "running" || !d.Running || d.Health != "healthy" {
		t.Errorf("identity and state = %+v", d)
	}
	if d.ExitCode != 3 || d.StartedAt != "2026-10-01T10:00:00Z" || !d.OOMKilled || d.RestartCount != 2 {
		t.Errorf("state details = %+v", d)
	}
	if !slices.Equal(d.Cmd, []string{"nginx", "-g", "daemon off;"}) || !slices.Equal(d.Entrypoint, []string{"/docker-entrypoint.sh"}) || d.WorkingDir != "/app" || d.User != "www" {
		t.Errorf("command = %+v", d)
	}
	if !slices.Equal(d.Env, []string{"A=1", "TOKEN=a=b"}) || d.Labels["com.docker.compose.project"] != "shop" {
		t.Errorf("env and labels = %v %v", d.Env, d.Labels)
	}
	if d.MemoryLimit != 512<<20 || d.NanoCPUs != 1_500_000_000 {
		t.Errorf("limits = %d bytes, %d nano cpus", d.MemoryLimit, d.NanoCPUs)
	}
	if d.RestartPolicy != "on-failure" || d.RestartMaxRetries != 3 {
		t.Errorf("restart policy = %q %d", d.RestartPolicy, d.RestartMaxRetries)
	}
	if d.StopTimeout == nil || *d.StopTimeout != 20 || d.StopSignal != "SIGQUIT" {
		t.Errorf("stop = %v %q", d.StopTimeout, d.StopSignal)
	}
	if d.LogConfig.Type != "syslog" || d.LogConfig.Options["splunk-token"] != "c0ffee-secret" {
		t.Errorf("log config = %+v", d.LogConfig)
	}
	// Only published ports, one row per binding, sorted by container port.
	wantPorts := []dockerx.PortRow{
		{ContainerPort: 53, Protocol: "udp", HostIP: "::", HostPort: "5353"},
		{ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0", HostPort: "8080"},
	}
	if !slices.Equal(d.Ports, wantPorts) {
		t.Errorf("ports = %+v, want %+v", d.Ports, wantPorts)
	}
	if len(d.Mounts) != 2 {
		t.Fatalf("mounts = %+v", d.Mounts)
	}
	for _, m := range d.Mounts {
		if m.Source == "/srv/site" && (m.Type != "bind" || m.Mode != "ro") {
			t.Errorf("bind mount = %+v", m)
		}
	}
	if len(d.Networks) != 2 || d.Networks[0].Name != "back" || d.Networks[1].Name != "front" {
		t.Fatalf("networks = %+v, want back then front", d.Networks)
	}
	if front := d.Networks[1]; front.IPv4 != "10.0.0.2" || !slices.Equal(front.Aliases, []string{"web", "site"}) {
		t.Errorf("front = %+v", front)
	}
	if d.Networks[0].Aliases == nil {
		t.Error("aliases of a network with none must be an empty list")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc["Id"] != "web-id" {
		t.Errorf("raw inspect = %s (%v)", raw, err)
	}
}

func TestContainerDetailOfAStoppedBareContainerHasNoNilCollections(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
		{ID: "db-id", Names: []string{"/db"}, Image: "postgres", State: "exited", Status: "Exited (0) 1 hour ago"},
	}})
	d, _, err := newClient(t, fake).ContainerDetail(context.Background(), "db-id")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "exited" || d.Running || d.RestartPolicy != "no" || d.Health != "none" {
		t.Errorf("detail = %+v", d)
	}
	if d.Ports == nil || d.Mounts == nil || d.Networks == nil || d.Labels == nil || d.Env == nil || d.Cmd == nil || d.Entrypoint == nil || d.LogConfig.Options == nil {
		t.Errorf("a collection is nil: %+v", d)
	}
}

func TestContainerDetailOfAnUnknownContainerIsNotFound(t *testing.T) {
	_, _, err := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{})).ContainerDetail(context.Background(), "gone")
	if err == nil || !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}
