// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"encoding/json"

	"github.com/moby/moby/api/types/swarm"
)

// taskList is the /tasks answer. A task is listed only when every filter the request
// names matches it, as a real daemon lists only what matches: service (an ID or a name),
// node (an ID or a hostname), desired-state and id. A request with no filter at all
// lists nothing, which is not what a real daemon does (it lists every task) but keeps a
// test that forgets to filter from passing by accident.
//
// The tasks are those of AgentTasks, each a task of the service named bayplan-agent
// unless it says otherwise, then those of Tasks, which are whole swarm tasks.
func (c FakeConfig) taskList(rawFilters string) []swarm.Task {
	out := []swarm.Task{}
	var f map[string]map[string]bool
	if json.Unmarshal([]byte(rawFilters), &f) != nil || len(f) == 0 {
		return out
	}
	alias := map[string]string{} // service ID <-> name
	for _, fs := range c.ServiceRows {
		alias[fs.ID], alias[fs.Name] = fs.Name, fs.ID
	}
	hostOf := map[string]string{} // node ID -> hostname
	for _, n := range c.NodeRows {
		hostOf[n.ID] = n.Hostname
	}
	listed := func(t swarm.Task) bool {
		if want := f["service"]; len(want) > 0 && !want[t.ServiceID] && !want[alias[t.ServiceID]] {
			return false
		}
		if want := f["node"]; len(want) > 0 && !want[t.NodeID] && !want[hostOf[t.NodeID]] {
			return false
		}
		if want := f["desired-state"]; len(want) > 0 && !want[string(t.DesiredState)] {
			return false
		}
		if want := f["id"]; len(want) > 0 && !want[t.ID] {
			return false
		}
		return true
	}
	for _, ft := range c.AgentTasks {
		svc := ft.Service
		if svc == "" {
			svc = "bayplan-agent"
		}
		t := swarm.Task{ID: ft.ID, NodeID: ft.NodeID, ServiceID: svc}
		t.CreatedAt = ft.Created
		t.Status = swarm.TaskStatus{State: swarm.TaskState(ft.State), Err: ft.Error}
		if listed(t) {
			out = append(out, t)
		}
	}
	for _, t := range c.Tasks {
		if listed(t) {
			out = append(out, t)
		}
	}
	return out
}
