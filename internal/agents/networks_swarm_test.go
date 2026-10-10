// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// networkIDs reads the IDs of the networks called name on a node, as Docker itself lists
// them, so the checks below do not trust the code under test to report its own effect.
func networkIDs(t *testing.T, n *dockertest.Node, name string) []string {
	t.Helper()
	return strings.Fields(sh(t, n, "docker network ls --no-trunc --filter name=^"+name+"$ --format '{{.ID}}'"))
}

// TestSwarmNetworks creates and deletes networks through the API on a real swarm of three
// managers and a worker with an agent on every node (plan 04-13, RES-06, D-39, D-41, P2
// D-42): an overlay made through the API exists on the managers; a bridge made with the
// worker's node ID exists only on the worker; a service scaled to zero that mentions the
// overlay blocks its delete, and once the service is gone the delete goes through.
func TestSwarmNetworks(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	base := "/api/env/" + f.env.ID + "/networks"

	post := func(body map[string]any) string {
		t.Helper()
		code, raw := f.srv.Do(http.MethodPost, base, body)
		if code != http.StatusCreated {
			t.Fatalf("create %v: %d %s", body, code, raw)
		}
		var out struct{ ID string }
		if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
			t.Fatalf("create answer %s (%v)", raw, err)
		}
		return out.ID
	}

	// An overlay made through the API exists on the managers, once each.
	overlayID := post(map[string]any{"name": "bp-ovl", "driver": "overlay"})
	for _, m := range f.sw.Managers {
		f.srv.WaitFor("the overlay on "+m.Hostname, 30*time.Second, func() (bool, string) {
			ids := networkIDs(t, m, "bp-ovl")
			return len(ids) == 1 && ids[0] == overlayID, strings.Join(ids, ",")
		})
	}

	// A bridge made with the worker's node ID exists only on the worker.
	var workerAgent string
	for _, a := range f.srv.Agents(f.env.ID) {
		if a.Node == worker.Hostname {
			workerAgent = a.ID
		}
	}
	if workerAgent == "" {
		t.Fatalf("no agent for the worker %s", worker.Hostname)
	}
	bridgeID := post(map[string]any{"name": "bp-br", "driver": "bridge", "nodeId": workerAgent})
	if ids := networkIDs(t, worker, "bp-br"); len(ids) != 1 || ids[0] != bridgeID {
		t.Fatalf("the worker's networks called bp-br = %v, want %s", ids, bridgeID)
	}
	for _, m := range f.sw.Managers {
		if ids := networkIDs(t, m, "bp-br"); len(ids) != 0 {
			t.Errorf("%s has the worker's bridge: %v", m.Hostname, ids)
		}
	}

	// A built-in network is refused and stays.
	f.srv.WaitFor("the networks list to show the bridge of the worker", 30*time.Second, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodGet, base, nil)
		return code == http.StatusOK && strings.Contains(string(raw), bridgeID), string(raw)
	})
	var list struct {
		Items []struct {
			ID, Name, NodeID string
			BuiltIn          bool
		}
	}
	_, raw := f.srv.Do(http.MethodGet, base, nil)
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, n := range list.Items {
		if n.Name == "ingress" && n.BuiltIn {
			code, body := f.srv.Do(http.MethodDelete, base+"/"+n.ID, nil)
			if code != http.StatusConflict || !strings.Contains(string(body), "network_builtin") {
				t.Errorf("deleting ingress: %d %s, want 409 network_builtin", code, body)
			}
		}
	}
	if ids := networkIDs(t, mgr, "ingress"); len(ids) != 1 {
		t.Errorf("ingress after the refusal: %v", ids)
	}

	// A service scaled to zero that mentions the overlay blocks its delete.
	sh(t, mgr, "docker service create --name bp-idle --replicas 0 --network bp-ovl busybox sleep 600")
	code, body := f.srv.Do(http.MethodDelete, base+"/"+overlayID, nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "network_in_use") || !strings.Contains(string(body), "bp-idle") {
		t.Fatalf("deleting the overlay with a service at zero replicas: %d %s, want 409 network_in_use naming bp-idle", code, body)
	}
	if ids := networkIDs(t, mgr, "bp-ovl"); len(ids) != 1 {
		t.Fatalf("the refused delete took the overlay away: %v", ids)
	}

	// With the service gone, the delete goes through.
	sh(t, mgr, "docker service rm bp-idle")
	f.srv.WaitFor("the overlay to be deletable once the service is gone", 90*time.Second, func() (bool, string) {
		code, body := f.srv.Do(http.MethodDelete, base+"/"+overlayID, nil)
		switch code {
		case http.StatusNoContent:
			return true, ""
		case http.StatusConflict:
			return false, string(body)
		}
		t.Fatalf("deleting the overlay: %d %s", code, body)
		return false, ""
	})
	for _, m := range f.sw.Managers {
		if ids := networkIDs(t, m, "bp-ovl"); len(ids) != 0 {
			t.Errorf("%s still has the overlay: %v", m.Hostname, ids)
		}
	}

	// The worker's bridge is deleted by naming its node, and only there.
	if code, body := f.srv.Do(http.MethodDelete, base+"/"+bridgeID+"?nodeId="+workerAgent, nil); code != http.StatusNoContent {
		t.Fatalf("deleting the worker's bridge: %d %s", code, body)
	}
	if ids := networkIDs(t, worker, "bp-br"); len(ids) != 0 {
		t.Errorf("the worker still has the bridge: %v", ids)
	}
	if code, _ := f.srv.Do(http.MethodDelete, base+"/"+bridgeID+"?nodeId="+workerAgent, nil); code != http.StatusNotFound {
		t.Errorf("a second delete = %d, want 404", code)
	}
}
