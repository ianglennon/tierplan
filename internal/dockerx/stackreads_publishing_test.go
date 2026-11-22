// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// bindings is the HostConfig.PortBindings an engine holds for a container created with these -p flags: the
// container port, its protocol and the host port ("" is a binding with no host port).
func bindings(t *testing.T, pairs ...[2]string) *container.InspectResponse {
	t.Helper()
	m := network.PortMap{}
	for _, p := range pairs {
		port, err := network.ParsePort(p[0])
		if err != nil {
			t.Fatalf("ParsePort(%q): %v", p[0], err)
		}
		m[port] = append(m[port], network.PortBinding{HostPort: p[1]})
	}
	return &container.InspectResponse{HostConfig: &container.HostConfig{PortBindings: m}}
}

func TestContainersPublishingFindsStoppedAndCreated(t *testing.T) {
	// An engine lists a created or exited container with no ports, so only its inspect shows the binding.
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{
		{ID: "r1", Names: []string{"/web"}, State: "running", Labels: map[string]string{"com.docker.compose.project": "shop"},
			Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8080, Public: 80, Type: "tcp"}}},
		{ID: "e1", Names: []string{"/old"}, State: "exited",
			Inspect: dockertest.FakeInspect{Document: bindings(t, [2]string{"443/tcp", "443"})}},
		{ID: "c1", Names: []string{"/fresh"}, State: "created", Labels: map[string]string{"k": "v"},
			Inspect: dockertest.FakeInspect{Document: bindings(t, [2]string{"80/tcp", "80"}, [2]string{"9000/tcp", ""})}},
		{ID: "u1", Names: []string{"/quic"}, State: "running",
			Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 443, Public: 443, Type: "udp"}}},
		{ID: "o1", Names: []string{"/other"}, State: "running",
			Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8443, Public: 8443, Type: "tcp"}}},
		{ID: "g1", Names: []string{"/range"}, State: "created",
			Inspect: dockertest.FakeInspect{Document: bindings(t, [2]string{"8000/tcp", "70-90"})}},
		{ID: "x1", Names: []string{"/stopped-quic"}, State: "exited",
			Inspect: dockertest.FakeInspect{Document: bindings(t, [2]string{"443/udp", "443"})}},
	}})

	got, err := newClient(t, fake).ContainersPublishing(context.Background(), 80, 443)
	if err != nil {
		t.Fatalf("ContainersPublishing: %v", err)
	}
	type row struct {
		name, state string
		port        uint32
	}
	var rows []row
	for _, p := range got {
		rows = append(rows, row{p.Name, p.State, p.Port})
	}
	want := []row{{"fresh", "created", 80}, {"old", "exited", 443}, {"range", "created", 80}, {"web", "running", 80}}
	if len(rows) != len(want) {
		t.Fatalf("ContainersPublishing = %+v, want %+v", got, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v (all: %+v)", i, rows[i], want[i], got)
		}
	}
	if got[0].ID != "c1" || got[0].Labels["k"] != "v" {
		t.Errorf("the created row = %+v, want its ID and labels", got[0])
	}
	if got[3].Labels["com.docker.compose.project"] != "shop" {
		t.Errorf("the running row's labels = %v, want the compose project", got[2].Labels)
	}
}

// failingInspect is an engine that lists one exited container and answers its inspect with a 500.
type failingInspect struct{ srv *httptest.Server }

func newFailingInspect(t *testing.T) *failingInspect {
	t.Helper()
	version := regexp.MustCompile(`^/v[0-9.]+`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.52")
		w.Header().Set("Content-Type", "application/json")
		switch p := version.ReplaceAllString(r.URL.Path, ""); p {
		case "/_ping":
			_, _ = w.Write([]byte("OK"))
		case "/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"e1","Names":["/old"],"State":"exited","Image":"nginx","Labels":{},"Ports":[]}]`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"inspect broke"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &failingInspect{srv: srv}
}

func (f *failingInspect) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", f.srv.Listener.Addr().String())
}

func (f *failingInspect) Describe() string { return "tcp://" + f.srv.Listener.Addr().String() }

func TestContainersPublishingFailsWhenAnInspectFails(t *testing.T) {
	got, err := newClient(t, newFailingInspect(t)).ContainersPublishing(context.Background(), 80, 443)
	if err == nil {
		t.Fatalf("ContainersPublishing = %+v, want an error: a container that cannot be read must not be skipped", got)
	}
}
