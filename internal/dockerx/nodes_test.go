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
