// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// volumeNames lists the volumes called name on a node, as Docker itself lists them, so
// the checks below do not trust the code under test to report its own effect.
func volumeNames(t *testing.T, n *dockertest.Node, name string) []string {
	t.Helper()
	// The name filter is a regular expression on Engine 29 but a plain substring match
	// on Engine 24, so an anchored pattern finds nothing there: filter loosely and keep
	// the exact names here.
	var out []string
	for _, v := range strings.Fields(sh(t, n, "docker volume ls -q --filter name="+name)) {
		if v == name {
			out = append(out, v)
		}
	}
	return out
}

// sharedVolumes creates and deletes volumes through the API on a real swarm of three
// managers and a worker with an agent on every node (plan 04-17, RES-07, D-45, D-46, P2
// D-42): the same name made on two nodes is two rows with different nodes, deleting one
// node's copy leaves the other, and a service scaled to zero that mounts a volume blocks
// its delete until the service is gone.
func sharedVolumes(t *testing.T, f *swarmFix) {
	dockertest.LoadImage(t, "busybox", f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	base := "/api/env/" + f.env.ID + "/volumes"

	agentOf := func(n *dockertest.Node) string {
		t.Helper()
		for _, a := range f.srv.Agents(f.env.ID) {
			if a.Node == n.Hostname {
				return a.ID
			}
		}
		t.Fatalf("no agent for %s", n.Hostname)
		return ""
	}
	mgrAgent, workerAgent := agentOf(mgr), agentOf(worker)

	create := func(name, node string) {
		t.Helper()
		code, raw := f.srv.Do(http.MethodPost, base, map[string]any{"name": name, "nodeId": node})
		if code != http.StatusCreated {
			t.Fatalf("create %s on %s: %d %s", name, node, code, raw)
		}
	}
	type result struct {
		NodeID, Name, Result, Code, Detail string
	}
	del := func(confirm string, targets ...map[string]string) []result {
		t.Helper()
		code, raw := f.srv.Do(http.MethodPost, base+"/delete", map[string]any{"confirm": confirm, "targets": targets})
		if code != http.StatusOK {
			t.Fatalf("delete %v: %d %s", targets, code, raw)
		}
		var out struct{ Results []result }
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("delete answer %s (%v)", raw, err)
		}
		return out.Results
	}
	rowsOf := func(name string) []string {
		t.Helper()
		code, raw := f.srv.Do(http.MethodGet, base, nil)
		if code != http.StatusOK {
			t.Fatalf("list: %d %s", code, raw)
		}
		var list struct {
			Items []struct{ Name, NodeID string }
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		var nodes []string
		for _, v := range list.Items {
			if v.Name == name {
				nodes = append(nodes, v.NodeID)
			}
		}
		return nodes
	}

	// The same name on two nodes is two rows, one per node.
	create("shared", mgrAgent)
	create("shared", workerAgent)
	f.srv.WaitFor("two rows called shared", 60*time.Second, func() (bool, string) {
		nodes := rowsOf("shared")
		return len(nodes) == 2 && nodes[0] != nodes[1], strings.Join(nodes, ",")
	})

	// Deleting the manager's copy leaves the worker's.
	res := del("shared", map[string]string{"nodeId": mgrAgent, "name": "shared"})
	if len(res) != 1 || res[0].Result != "done" {
		t.Fatalf("delete on the manager: %+v", res)
	}
	if got := volumeNames(t, mgr, "shared"); len(got) != 0 {
		t.Errorf("the manager still has shared: %v", got)
	}
	if got := volumeNames(t, worker, "shared"); len(got) != 1 {
		t.Errorf("the worker lost its copy: %v", got)
	}
	f.srv.WaitFor("one row called shared, the worker's", 60*time.Second, func() (bool, string) {
		nodes := rowsOf("shared")
		return len(nodes) == 1 && nodes[0] == workerAgent, strings.Join(nodes, ",")
	})

	// A second delete of the manager's copy says it is already gone.
	if res := del("shared", map[string]string{"nodeId": mgrAgent, "name": "shared"}); len(res) != 1 || res[0].Code != "not_found" {
		t.Fatalf("second delete: %+v, want not_found", res)
	}

	// A service scaled to zero that mounts a volume blocks its delete.
	create("svcvol", mgrAgent)
	sh(t, mgr, "docker service create --name bp-vol-idle --replicas 0 --mount type=volume,source=svcvol,target=/data busybox sleep 600")
	res = del("svcvol", map[string]string{"nodeId": mgrAgent, "name": "svcvol"})
	if len(res) != 1 || res[0].Result != "failed" || res[0].Code != "volume_in_use" || !strings.Contains(res[0].Detail, "bp-vol-idle") {
		t.Fatalf("delete with a service at zero replicas: %+v, want volume_in_use naming bp-vol-idle", res)
	}
	if got := volumeNames(t, mgr, "svcvol"); len(got) != 1 {
		t.Fatalf("the refused delete took the volume away: %v", got)
	}

	// With the service gone, the delete goes through.
	sh(t, mgr, "docker service rm bp-vol-idle")
	f.srv.WaitFor("the volume to be deletable once the service is gone", 90*time.Second, func() (bool, string) {
		res := del("svcvol", map[string]string{"nodeId": mgrAgent, "name": "svcvol"})
		return len(res) == 1 && res[0].Result == "done", res[0].Code
	})
	if got := volumeNames(t, mgr, "svcvol"); len(got) != 0 {
		t.Errorf("the manager still has svcvol: %v", got)
	}
}
