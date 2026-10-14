// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func serviceFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "web-id", Name: "web", Mode: "replicated", Running: 2, Desired: 2, Image: "registry.example.com/web:1", Version: 7,
				Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "agent-id", Name: "bayplan-agent", Mode: "global", Running: 3, Desired: 3},
			{ID: "job-id", Name: "migrate", Mode: "replicated-job"},
		},
	})
	return newClient(t, fake), fake
}

func TestServiceInspectAnswersTheTypedDetailAndTheRawDocument(t *testing.T) {
	c, _ := serviceFake(t)
	d, raw, err := c.ServiceInspect(context.Background(), "web")
	if err != nil {
		t.Fatalf("ServiceInspect: %v", err)
	}
	if d.ID != "web-id" || d.Name != "web" || d.Mode != "replicated" || d.Stack != "shop" || d.Image != "registry.example.com/web:1" ||
		d.Version != 7 || d.Replicas != 2 || d.Agent || d.PreviousSpec != nil || d.UpdateStatus != nil {
		t.Errorf("detail = %+v", d)
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(d.Spec, &spec); err != nil || spec.Name != "web" {
		t.Errorf("spec = %s (%v)", d.Spec, err)
	}
	if !strings.Contains(string(raw), `"web-id"`) {
		t.Errorf("raw = %s, want the inspect document", raw)
	}
	if a, _, err := c.ServiceInspect(context.Background(), "agent-id"); err != nil || !a.Agent {
		t.Errorf("agent service = %+v, %v; want Agent true", a, err)
	}
}

func TestServiceInspectOfAnUnknownServiceIsNotFound(t *testing.T) {
	c, _ := serviceFake(t)
	_, _, err := c.ServiceInspect(context.Background(), "nope")
	if !errors.Is(err, dockerx.ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
}

func TestServiceScaleSendsOneVersionedUpdateWithTheNewReplicasOnly(t *testing.T) {
	c, fake := serviceFake(t)
	prev, changed, err := c.ServiceScale(context.Background(), "web-id", 5, "Zm9v")
	if err != nil || prev != 2 || !changed {
		t.Fatalf("ServiceScale = %d, %v, %v; want 2, true", prev, changed, err)
	}
	ups := fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.ServiceID != "web-id" || u.Version != "7" || u.Auth != "Zm9v" {
		t.Errorf("update = %+v, want version 7 and the credential", u)
	}
	if got := u.Query.Get("registryAuthFrom") + u.Query.Get("rollback"); got != "" {
		t.Errorf("query = %v, want only the version", u.Query)
	}
	if r := u.Spec.Mode.Replicated; r == nil || r.Replicas == nil || *r.Replicas != 5 {
		t.Errorf("replicas = %+v, want 5", u.Spec.Mode.Replicated)
	}
	if u.Spec.Name != "web" || u.Spec.Labels["com.docker.stack.namespace"] != "shop" || u.Spec.TaskTemplate.ContainerSpec == nil ||
		u.Spec.TaskTemplate.ContainerSpec.Image != "registry.example.com/web:1" {
		t.Errorf("the rest of the spec changed: %+v", u.Spec)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /services/web-id/update?version=7"}) {
		t.Errorf("writes = %v", got)
	}
}

func TestServiceScaleToTheCountItHasWritesNothing(t *testing.T) {
	c, fake := serviceFake(t)
	prev, changed, err := c.ServiceScale(context.Background(), "web", 2, "")
	if err != nil || prev != 2 || changed {
		t.Fatalf("ServiceScale = %d, %v, %v; want 2, false", prev, changed, err)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceScaleRefusesGlobalAndJobServices(t *testing.T) {
	c, fake := serviceFake(t)
	for _, id := range []string{"agent-id", "job-id"} {
		if _, _, err := c.ServiceScale(context.Background(), id, 2, ""); !errors.Is(err, dockerx.ErrNotScalable) {
			t.Errorf("ServiceScale(%s) = %v, want ErrNotScalable", id, err)
		}
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestServiceScaleMapsTheOutOfSequenceAnswerAndSendsNoSecondUpdate(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{{ID: "web-id", Name: "web", Mode: "replicated", Desired: 2, Version: 7}},
		WriteDelay:  50 * time.Millisecond,
	})
	c := newClient(t, fake)
	done := make(chan error, 1)
	go func() {
		_, _, err := c.ServiceScale(context.Background(), "web-id", 4, "")
		done <- err
	}()
	// The update is on its way (the fake holds it for WriteDelay); somebody else changes
	// the service now, so the version the update carries is no longer current.
	deadline := time.Now().Add(5 * time.Second)
	for len(fake.Writes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	fake.BumpService("web-id")
	err := <-done
	if !errors.Is(err, dockerx.ErrServiceChanged) {
		t.Fatalf("err = %v, want ErrServiceChanged", err)
	}
	if got := len(fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: a refused write is never retried", got)
	}
	if got := len(fake.ServiceUpdates()); got != 0 {
		t.Errorf("accepted updates = %d, want none", got)
	}
}
