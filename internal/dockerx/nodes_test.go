// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func nodeFake(t *testing.T, cfg dockertest.FakeConfig) *dockerx.Client {
	t.Helper()
	cfg.SwarmState, cfg.SwarmControl = "active", true
	cfg.NodeRows = []dockertest.FakeNode{
		{
			ID: "mgr1", Hostname: "alpha", Role: "manager", Leader: true, Address: "10.0.0.1", ManagerAddr: "10.0.0.1:2377",
			EngineVersion: "29.1.0", OS: "linux", Arch: "aarch64", NanoCPUs: 4e9, MemoryBytes: 8 << 30,
			Labels: map[string]string{"zone": "a"}, EngineLabels: map[string]string{"disk": "ssd"},
			Version:   12,
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), UpdatedAt: time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC),
		},
		{ID: "wrk1", Hostname: "bravo", Address: "10.0.0.2", State: "down", Availability: "drain"},
	}
	return newClient(t, dockertest.NewFake(t, cfg))
}

func TestNodeInspectAnswersTheTypedDetailAndTheRawDocument(t *testing.T) {
	c := nodeFake(t, dockertest.FakeConfig{})
	d, raw, err := c.NodeInspect(context.Background(), "mgr1")
	if err != nil {
		t.Fatalf("NodeInspect: %v", err)
	}
	if d.ID != "mgr1" || d.Version != 12 || d.Hostname != "alpha" || d.Role != "manager" || d.Availability != "active" ||
		d.State != "ready" || d.Address != "10.0.0.1" || d.Reachability != "reachable" || d.ManagerAddr != "10.0.0.1:2377" ||
		d.EngineVersion != "29.1.0" || d.OS != "linux" || d.Arch != "aarch64" || !d.Leader ||
		d.NanoCPUs != 4e9 || d.MemoryBytes != 8<<30 {
		t.Errorf("detail = %+v", d)
	}
	if d.Labels["zone"] != "a" || d.EngineLabels["disk"] != "ssd" {
		t.Errorf("labels = %v, engine labels = %v", d.Labels, d.EngineLabels)
	}
	if !d.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || !d.UpdatedAt.Equal(time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)) ||
		d.CreatedAt.Location() != time.UTC || d.UpdatedAt.Location() != time.UTC {
		t.Errorf("times = %v, %v; want the node's, in UTC", d.CreatedAt, d.UpdatedAt)
	}
	if !strings.Contains(string(raw), `"mgr1"`) || !strings.Contains(string(raw), `"alpha"`) {
		t.Errorf("raw = %s, want the inspect document", raw)
	}
}

func TestNodeInspectOfAWorkerHasNoManagerFactsAndNeverNilLabels(t *testing.T) {
	c := nodeFake(t, dockertest.FakeConfig{})
	d, _, err := c.NodeInspect(context.Background(), "wrk1")
	if err != nil {
		t.Fatalf("NodeInspect: %v", err)
	}
	if d.Role != "worker" || d.Availability != "drain" || d.State != "down" || d.Leader || d.Reachability != "" || d.ManagerAddr != "" {
		t.Errorf("detail = %+v", d)
	}
	if d.Labels == nil || d.EngineLabels == nil {
		t.Errorf("labels = %#v, engine labels = %#v; want non-nil maps", d.Labels, d.EngineLabels)
	}
}

func TestNodeInspectOfAnUnknownNodeIsNotFound(t *testing.T) {
	c := nodeFake(t, dockertest.FakeConfig{})
	_, _, err := c.NodeInspect(context.Background(), "nope")
	if !errors.Is(err, dockerx.ErrNodeNotFound) {
		t.Fatalf("err = %v, want ErrNodeNotFound", err)
	}
}

func TestNodeInspectOnAWorkerEngineKeepsTheDaemonsWords(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{SwarmState: "active"})
	_, _, err := newClient(t, fake).NodeInspect(context.Background(), "mgr1")
	if err == nil || !strings.Contains(err.Error(), "not a swarm manager") {
		t.Fatalf("err = %v, want the worker refusal", err)
	}
}

func updateFake(t *testing.T, cfg dockertest.FakeConfig) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	cfg.SwarmState, cfg.SwarmControl = "active", true
	cfg.NodeRows = []dockertest.FakeNode{
		{ID: "mgr1", Hostname: "alpha", Role: "manager", Leader: true, Version: 12, Labels: map[string]string{"zone": "a"}},
		{ID: "wrk2", Hostname: "charlie", Version: 3, Labels: map[string]string{"disk": "ssd", "rack": "4"}},
	}
	fake := dockertest.NewFake(t, cfg)
	return newClient(t, fake), fake
}

func TestNodeUpdateSendsTheFullSpecWithOnlyTheAvailabilityChanged(t *testing.T) {
	c, fake := updateFake(t, dockertest.FakeConfig{})
	if err := c.NodeUpdate(context.Background(), "wrk2", dockerx.NodeChange{Version: 3, Availability: "drain"}); err != nil {
		t.Fatalf("NodeUpdate: %v", err)
	}
	ups := fake.NodeUpdates()
	if len(ups) != 1 {
		t.Fatalf("the daemon saw %d updates, want 1", len(ups))
	}
	u := ups[0]
	if u.NodeID != "wrk2" || u.Version != "3" || u.Spec.Availability != "drain" || u.Spec.Role != "worker" ||
		u.Spec.Labels["disk"] != "ssd" || u.Spec.Labels["rack"] != "4" || len(u.Spec.Labels) != 2 {
		t.Errorf("update = %+v, want the full spec with only availability drain", u)
	}
	d, _, err := c.NodeInspect(context.Background(), "wrk2")
	if err != nil || d.Availability != "drain" || d.Version != 4 {
		t.Errorf("the node after the update = %+v, %v; want drain at version 4", d, err)
	}
}

func TestNodeUpdateChangesOnlyTheRoleAndKeepsAvailabilityAndLabels(t *testing.T) {
	c, fake := updateFake(t, dockertest.FakeConfig{})
	if err := c.NodeUpdate(context.Background(), "wrk2", dockerx.NodeChange{Version: 3, Role: "manager"}); err != nil {
		t.Fatalf("NodeUpdate: %v", err)
	}
	u := fake.NodeUpdates()[0]
	if u.Spec.Role != "manager" || u.Spec.Availability != "active" || len(u.Spec.Labels) != 2 {
		t.Errorf("update = %+v, want role manager with availability and labels as they were", u.Spec)
	}
}

func TestNodeUpdateLabelsNilLeavesThemEmptyClearsThemAMapReplacesThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		ch   dockerx.NodeChange
		want map[string]string
	}{
		{"nil leaves labels alone", dockerx.NodeChange{Version: 3, Availability: "pause"}, map[string]string{"disk": "ssd", "rack": "4"}},
		{"an empty map clears them", dockerx.NodeChange{Version: 3, Labels: map[string]string{}}, map[string]string{}},
		{"SetLabels with nil clears them", dockerx.NodeChange{Version: 3, SetLabels: true}, map[string]string{}},
		{"a map replaces them", dockerx.NodeChange{Version: 3, Labels: map[string]string{"zone": "b"}}, map[string]string{"zone": "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, fake := updateFake(t, dockertest.FakeConfig{})
			if err := c.NodeUpdate(context.Background(), "wrk2", tc.ch); err != nil {
				t.Fatalf("NodeUpdate: %v", err)
			}
			got := fake.NodeUpdates()[0].Spec.Labels
			if len(got) != len(tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("labels = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestNodeUpdateWithAStaleVersionIsNodeChangedAndWritesNothing(t *testing.T) {
	c, fake := updateFake(t, dockertest.FakeConfig{})
	err := c.NodeUpdate(context.Background(), "wrk2", dockerx.NodeChange{Version: 2, Availability: "drain"})
	if !errors.Is(err, dockerx.ErrNodeChanged) {
		t.Fatalf("err = %v, want ErrNodeChanged", err)
	}
	if n := len(fake.NodeUpdates()); n != 0 {
		t.Errorf("the daemon saw %d updates, want none", n)
	}
}

func TestNodeUpdateMapsTheDaemonsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		want          error
	}{
		{"out of sequence", "rpc error: code = Unknown desc = update out of sequence", dockerx.ErrNodeChanged},
		{"the last manager", "rpc error: code = FailedPrecondition desc = attempting to demote the last manager of the swarm", dockerx.ErrLastManager},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := updateFake(t, dockertest.FakeConfig{NodeUpdateError: tc.message})
			err := c.NodeUpdate(context.Background(), "mgr1", dockerx.NodeChange{Version: 12, Role: "worker"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNodeUpdateKeepsAnyOtherRefusalsWords(t *testing.T) {
	c, _ := updateFake(t, dockertest.FakeConfig{NodeUpdateError: "rpc error: code = Unavailable desc = no leader"})
	err := c.NodeUpdate(context.Background(), "mgr1", dockerx.NodeChange{Version: 12, Availability: "drain"})
	if err == nil || !strings.Contains(err.Error(), "no leader") || errors.Is(err, dockerx.ErrNodeChanged) || errors.Is(err, dockerx.ErrLastManager) {
		t.Fatalf("err = %v, want the daemon's own words and neither typed refusal", err)
	}
}

func TestNodeUpdateOfAnUnknownNodeIsNotFound(t *testing.T) {
	c, _ := updateFake(t, dockertest.FakeConfig{})
	err := c.NodeUpdate(context.Background(), "nope", dockerx.NodeChange{Version: 1, Availability: "drain"})
	if !errors.Is(err, dockerx.ErrNodeNotFound) {
		t.Fatalf("err = %v, want ErrNodeNotFound", err)
	}
}
