// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// networkIDs reads the IDs of the networks called name on a node, as Docker itself lists
// them, so the checks below do not trust the code under test to report its own effect.
func networkIDs(t *testing.T, n *dockertest.Node, name string) []string {
	t.Helper()
	// The name filter is a regular expression on Engine 29 but a plain substring match
	// on Engine 24, so an anchored pattern finds nothing there: filter loosely and keep
	// the exact names here.
	var out []string
	for _, line := range strings.Split(sh(t, n, "docker network ls --no-trunc --filter name="+name+" --format '{{.ID}} {{.Name}}'"), "\n") {
		if id, got, ok := strings.Cut(strings.TrimSpace(line), " "); ok && got == name {
			out = append(out, id)
		}
	}
	return out
}

// sharedNetworks creates and deletes networks through the API on a real swarm of three
// managers and a worker with an agent on every node (plan 04-13, RES-06, D-39, D-41, P2
// D-42): an overlay made through the API exists on the managers; a bridge made with the
// worker's node ID exists only on the worker; a service scaled to zero that mentions the
// overlay blocks its delete, and once the service is gone the delete goes through.
func sharedNetworks(t *testing.T, f *swarmFix) {
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
	overlayID := post(map[string]any{"name": "cm-ovl", "driver": "overlay"})
	for _, m := range f.sw.Managers {
		f.srv.WaitFor("the overlay on "+m.Hostname, 30*time.Second, func() (bool, string) {
			ids := networkIDs(t, m, "cm-ovl")
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
	bridgeID := post(map[string]any{"name": "cm-br", "driver": "bridge", "nodeId": workerAgent})
	if ids := networkIDs(t, worker, "cm-br"); len(ids) != 1 || ids[0] != bridgeID {
		t.Fatalf("the worker's networks called cm-br = %v, want %s", ids, bridgeID)
	}
	for _, m := range f.sw.Managers {
		if ids := networkIDs(t, m, "cm-br"); len(ids) != 0 {
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
	sh(t, mgr, "docker service create --name cm-idle --replicas 0 --network cm-ovl busybox sleep 600")
	code, body := f.srv.Do(http.MethodDelete, base+"/"+overlayID, nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "network_in_use") || !strings.Contains(string(body), "cm-idle") {
		t.Fatalf("deleting the overlay with a service at zero replicas: %d %s, want 409 network_in_use naming cm-idle", code, body)
	}
	if ids := networkIDs(t, mgr, "cm-ovl"); len(ids) != 1 {
		t.Fatalf("the refused delete took the overlay away: %v", ids)
	}

	// With the service gone, the delete goes through.
	sh(t, mgr, "docker service rm cm-idle")
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
		if ids := networkIDs(t, m, "cm-ovl"); len(ids) != 0 {
			t.Errorf("%s still has the overlay: %v", m.Hostname, ids)
		}
	}

	// The worker's bridge is deleted by naming its node, and only there.
	if code, body := f.srv.Do(http.MethodDelete, base+"/"+bridgeID+"?nodeId="+workerAgent, nil); code != http.StatusNoContent {
		t.Fatalf("deleting the worker's bridge: %d %s", code, body)
	}
	if ids := networkIDs(t, worker, "cm-br"); len(ids) != 0 {
		t.Errorf("the worker still has the bridge: %v", ids)
	}
	if code, _ := f.srv.Do(http.MethodDelete, base+"/"+bridgeID+"?nodeId="+workerAgent, nil); code != http.StatusNotFound {
		t.Errorf("a second delete = %d, want 404", code)
	}
}

// sharedNetworkConnect connects a standalone container on a worker to an attachable
// overlay through the API on a real swarm of three managers and a worker with an agent on
// every node (plan 04-15, RES-06, D-43, Pitfall 11, RESEARCH A5). A worker only learns an
// overlay when a task of a service on it runs there, so the first connect may answer
// network_unavailable_on_node with the engine's own words. The test records which way the
// engine goes: if the worker refuses, it starts a task of a service on the overlay on the
// worker and connects again, which must work; if it accepts, it says so. Either way the
// alias is in the container's inspect on the worker and a disconnect removes the endpoint.
func sharedNetworkConnect(t *testing.T, f *swarmFix) {
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	base := "/api/env/" + f.env.ID + "/networks"

	var workerAgent string
	for _, a := range f.srv.Agents(f.env.ID) {
		if a.Node == worker.Hostname {
			workerAgent = a.ID
		}
	}
	if workerAgent == "" {
		t.Fatalf("no agent for the worker %s", worker.Hostname)
	}

	// An attachable overlay made through the API, and a standalone container on the worker.
	code, raw := f.srv.Do(http.MethodPost, base, map[string]any{"name": "cm-attach", "driver": "overlay", "attachable": true})
	if code != http.StatusCreated {
		t.Fatalf("create the overlay: %d %s", code, raw)
	}
	var made struct{ ID string }
	if err := json.Unmarshal(raw, &made); err != nil || made.ID == "" {
		t.Fatalf("create answer %s (%v)", raw, err)
	}
	sh(t, worker, "docker run -d --name cm-wk busybox sleep 600")
	containerID := strings.TrimSpace(sh(t, worker, "docker inspect -f '{{.Id}}' cm-wk"))

	// The list has to know the container, on its node, before the API can find it.
	f.srv.WaitFor("the list to show the worker's container", 60*time.Second, func() (bool, string) {
		code, raw := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/containers", nil)
		return code == http.StatusOK && strings.Contains(string(raw), containerID), string(raw)
	})

	endpoints := func() string {
		return sh(t, worker, "docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}={{$v.Aliases}} {{end}}' cm-wk")
	}
	connect := func() (int, []byte) {
		return f.srv.Do(http.MethodPost, base+"/"+made.ID+"/connect",
			map[string]any{"containerId": containerID, "nodeId": workerAgent, "alias": "wk"})
	}

	code, raw = connect()
	switch {
	case code == http.StatusNoContent:
		t.Logf("RESEARCH A5: the worker accepted a connect to an attachable overlay it had no task on")
	case code == http.StatusConflict && strings.Contains(string(raw), "network_unavailable_on_node"):
		t.Logf("RESEARCH A5: the worker answered network_unavailable_on_node before it had a task on the overlay: %s", raw)
		sh(t, mgr, "docker service create --name cm-anchor --replicas 1 --constraint node.hostname=="+worker.Hostname+" --network cm-attach busybox sleep 600")
		f.srv.WaitFor("a task of the service on the worker", 120*time.Second, func() (bool, string) {
			out := sh(t, mgr, "docker service ps cm-anchor --filter desired-state=running --format '{{.Node}} {{.CurrentState}}'")
			return strings.Contains(out, worker.Hostname) && strings.Contains(out, "Running"), out
		})
		f.srv.WaitFor("the connect to go through once the worker has a task on the overlay", 90*time.Second, func() (bool, string) {
			code, raw := connect()
			switch code {
			case http.StatusNoContent:
				return true, ""
			case http.StatusConflict:
				return false, string(raw)
			}
			t.Fatalf("connect after the task: %d %s", code, raw)
			return false, ""
		})
		t.Logf("RESEARCH A5: the connect worked once a task of a service on the overlay ran on the worker")
	default:
		t.Fatalf("connect: %d %s, want 204 or 409 network_unavailable_on_node", code, raw)
	}
	if got := endpoints(); !strings.Contains(got, "cm-attach=[") || !strings.Contains(got, "wk") {
		t.Fatalf("the container's endpoints on the worker = %q, want cm-attach with the alias wk", got)
	}

	// A second connect is the engine's, mapped refusal, and nothing is added.
	if code, raw := connect(); code != http.StatusConflict || !strings.Contains(string(raw), "already_connected") {
		t.Errorf("a second connect: %d %s, want 409 already_connected", code, raw)
	}

	// Disconnecting removes the endpoint.
	code, raw = f.srv.Do(http.MethodPost, base+"/"+made.ID+"/disconnect", map[string]any{"containerId": containerID, "nodeId": workerAgent})
	if code != http.StatusNoContent {
		t.Fatalf("disconnect: %d %s", code, raw)
	}
	if got := endpoints(); strings.Contains(got, "cm-attach") {
		t.Errorf("the container's endpoints after the disconnect = %q, want cm-attach gone", got)
	}
}
