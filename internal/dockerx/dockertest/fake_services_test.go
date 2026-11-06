// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

func fakeServiceVersion(t *testing.T, f *dockertest.Fake, ref string) uint64 {
	t.Helper()
	req, err := http.NewRequestWithContext(awaitCtx(t), http.MethodGet, "http://engine/v1.52/services/"+ref, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fakeClient(f).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out swarm.Service
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Version.Index
}

func fakeServiceWrite(t *testing.T, f *dockertest.Fake, ref string, version uint64, rollback bool) {
	t.Helper()
	url := "http://engine/v1.52/services/" + ref + "/update?version=" + strconv.FormatUint(version, 10)
	if rollback {
		url += "&rollback=previous"
	}
	body, err := json.Marshal(swarm.ServiceSpec{Annotations: swarm.Annotations{Name: ref}, TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "registry.test/x:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(awaitCtx(t), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fakeClient(f).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write to %s = %d", ref, resp.StatusCode)
	}
}

// The fake moves a service's version on every write to it, whoever makes the write, and on nothing else, as a swarm's manager does
// (measured on Docker 29.8.2: an idle service keeps its version through reads, task restarts and node events; an update, a
// rollback by hand and the swarm's own rollback each move it). Recovery of an interrupted update reads the move as "the run wrote
// this service".
func TestTheFakeMovesAServicesVersionOnEveryWriteAndOnNothingElse(t *testing.T) {
	spec := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web"}, TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "registry.test/x:0"}}}
	f := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{{ID: "svc-web", Name: "web", Mode: "replicated", Version: 5, Spec: &spec}},
	})
	at := fakeServiceVersion(t, f, "web")
	if again := fakeServiceVersion(t, f, "svc-web"); again != at {
		t.Fatalf("version after a read = %d, want it unchanged at %d", again, at)
	}
	steps := []struct {
		name  string
		write func()
	}{
		{"an update", func() { fakeServiceWrite(t, f, "svc-web", at, false) }},
		{"a rollback by hand", func() { fakeServiceWrite(t, f, "svc-web", at, true) }},
		{"the swarm's own rollback", func() {
			if !f.RollBackService("web") {
				t.Fatal("the fake could not roll the service back")
			}
		}},
		{"somebody else's write", func() {
			if !f.ReplaceServiceSpec("web", spec) {
				t.Fatal("the fake could not replace the spec")
			}
		}},
	}
	for _, s := range steps {
		s.write()
		now := fakeServiceVersion(t, f, "web")
		if now == at {
			t.Fatalf("version after %s = %d, want it moved from %d", s.name, now, at)
		}
		at = now
		if again := fakeServiceVersion(t, f, "web"); again != at {
			t.Fatalf("version after %s and a read = %d, want it still %d", s.name, again, at)
		}
	}
}
