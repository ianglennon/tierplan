// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
)

// fakeTaskState is the state SetTaskState gave a service's tasks and the error text they carry, or, in
// the case of SetServiceTasks, the tasks themselves.
type fakeTaskState struct {
	state, errText string
	tasks          []FakeServiceTask
	// whileImage, when set, limits the state to the writes whose image contains it: a service written with another image has tasks
	// that run, as a swarm's do once a bad version is replaced.
	whileImage string
}

// appliesTo reports whether the state is the one the spec's tasks are in.
func (o fakeTaskState) appliesTo(spec *swarm.ServiceSpec) bool {
	if o.whileImage == "" {
		return true
	}
	cs := spec.TaskTemplate.ContainerSpec
	return cs != nil && strings.Contains(cs.Image, o.whileImage)
}

// FakeServiceTask is one task of a service whose tasks SetServiceTasks scripts.
type FakeServiceTask struct {
	// ID is the task's ID. "" means "<service ID>-scripted-<n>", where n is the 1-based position in the
	// script.
	ID string
	// Slot is the replica slot of a replicated service's task, 0 for a global service's.
	Slot int
	// Node is the ID of the node the task is on. "" means "node-1".
	Node string
	// State is the state the swarm last saw the task in.
	State string
	// Desired is the state the swarm wants the task in. "" means "running".
	Desired string
	// Err and Message are the swarm's free text about the task.
	Err, Message string
	// Networks are the networks the task is attached to, by ID or by name as the daemon knows
	// them. They are the task's NetworksAttachments, which a task keeps after an update removed
	// the network from its service's spec.
	Networks []string
	// Old puts the task on a spec other than the service's current task template, so its spec hash
	// differs from the service's.
	Old bool
}

// withTaskStates returns c with the states SetTaskState gave the tasks that follow writes.
func (c FakeConfig) withTaskStates(states map[string]fakeTaskState) FakeConfig {
	if len(states) > 0 {
		c.taskStates = maps.Clone(states)
	}
	return c
}

// SetTaskState sets the state, and the error text the swarm records with it, of the tasks that follow
// the writes to the service of that name, as a swarm moves them on: preparing while it pulls the
// image, rejected when it cannot, running once it can. A test moves a service on with it between
// two polls of a deploy. The state "" is running again, and takes a SetServiceTasks script away.
func (f *Fake) SetTaskState(service, state, errText string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taskStates == nil {
		f.taskStates = map[string]fakeTaskState{}
	}
	if state == "" {
		delete(f.taskStates, service)
		return
	}
	f.taskStates[service] = fakeTaskState{state: state, errText: errText}
}

// SetTaskStateWhileImage is SetTaskState for the writes whose image contains imageContains: a service written with another image
// has tasks that run. A test uses it to make a bad version fail and the version put back by a rollback run, with no move of the
// clock between the two.
func (f *Fake) SetTaskStateWhileImage(service, imageContains, state, errText string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taskStates == nil {
		f.taskStates = map[string]fakeTaskState{}
	}
	f.taskStates[service] = fakeTaskState{state: state, errText: errText, whileImage: imageContains}
}

// SetServiceTasksWhileImage is SetServiceTasks for the writes whose image contains imageContains, like SetTaskStateWhileImage.
func (f *Fake) SetServiceTasksWhileImage(service, imageContains string, tasks ...FakeServiceTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taskStates == nil {
		f.taskStates = map[string]fakeTaskState{}
	}
	if len(tasks) == 0 {
		delete(f.taskStates, service)
		return
	}
	f.taskStates[service] = fakeTaskState{tasks: slices.Clone(tasks), whileImage: imageContains}
}

// SetServiceTasks scripts the tasks of the service of that name. They replace the tasks that follow the
// writes to the service, and they are listed newest first, the first being the newest. A later
// SetServiceTasks or SetTaskState for the service replaces the script; no tasks takes the script away.
func (f *Fake) SetServiceTasks(service string, tasks ...FakeServiceTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taskStates == nil {
		f.taskStates = map[string]fakeTaskState{}
	}
	if len(tasks) == 0 {
		delete(f.taskStates, service)
		return
	}
	f.taskStates[service] = fakeTaskState{tasks: slices.Clone(tasks)}
}

// LoseQuorum makes the swarm lose its quorum from now on, as QuorumLost does from the start: the
// cluster questions (/nodes, /services, /tasks) answer 500. A test uses it to cut the reads that
// follow a write.
func (f *Fake) LoseQuorum() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quorumLost = true
}

func (c FakeConfig) withQuorum(lost bool) FakeConfig {
	if lost {
		c.QuorumLost = true
	}
	return c
}

// withUpdateStates returns c with the update statuses SetUpdateState gave the services.
func (c FakeConfig) withUpdateStates(states map[string]fakeTaskState) FakeConfig {
	if len(states) == 0 {
		return c
	}
	rows := slices.Clone(c.ServiceRows)
	for i, fs := range rows {
		if o, ok := states[fs.Name]; ok {
			fs.UpdateState, fs.UpdateMessage = o.state, o.errText
			rows[i] = fs
		}
	}
	c.ServiceRows = rows
	return c
}

// SetUpdateState gives the service of that name an UpdateStatus with this state and message, as a
// swarm does while it rolls an update out and when it pauses or rolls it back. The state "" takes the
// status away.
func (f *Fake) SetUpdateState(service, state, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateStates == nil {
		f.updateStates = map[string]fakeTaskState{}
	}
	f.updateStates[service] = fakeTaskState{state: state, errText: message}
}

// followingTasks are the tasks of the services whose spec the daemon holds, when TasksFollowWrites
// is on: what a settled swarm would show for the spec each one has now.
func (c FakeConfig) followingTasks() []swarm.Task {
	if !c.TasksFollowWrites {
		return nil
	}
	var out []swarm.Task
	for _, fs := range c.ServiceRows {
		if fs.Spec == nil {
			continue
		}
		if o, ok := c.taskStates[fs.Name]; ok && o.tasks != nil && o.appliesTo(fs.Spec) {
			out = append(out, c.scriptedTasks(fs, o.tasks)...)
			continue
		}
		state, errText := swarm.TaskStateRunning, ""
		if o, ok := c.taskStates[fs.Name]; ok && o.tasks == nil && o.appliesTo(fs.Spec) {
			state, errText = swarm.TaskState(o.state), o.errText
		}
		var nodes []string
		switch {
		case fs.Spec.Mode.Global != nil:
			for _, n := range c.NodeRows {
				nodes = append(nodes, n.ID)
			}
			if len(nodes) == 0 {
				nodes = []string{"node-1"}
			}
		case fs.Spec.Mode.Replicated != nil:
			replicas := uint64(1)
			if r := fs.Spec.Mode.Replicated.Replicas; r != nil {
				replicas = *r
			}
			for range replicas {
				nodes = append(nodes, "node-1")
			}
		default:
			continue // a job runs to its end and has no task that stays
		}
		for i, node := range nodes {
			t := swarm.Task{ID: fmt.Sprintf("%s-task-%d", fs.ID, i+1), ServiceID: fs.ID, NodeID: node, DesiredState: swarm.TaskStateRunning, Spec: fs.Spec.TaskTemplate}
			if fs.Spec.Mode.Replicated != nil {
				t.Slot = i + 1
			}
			t.Status = swarm.TaskStatus{State: state, Err: errText}
			out = append(out, t)
		}
	}
	return out
}

// scriptedBase is the creation time of the newest scripted task; each next one is a second older, so
// the client's newest-first sort keeps the script's order.
var scriptedBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// scriptedTasks are the tasks SetServiceTasks gave the service fs.
func (c FakeConfig) scriptedTasks(fs FakeService, script []FakeServiceTask) []swarm.Task {
	out := make([]swarm.Task, 0, len(script))
	for i, st := range script {
		id := st.ID
		if id == "" {
			id = fmt.Sprintf("%s-scripted-%d", fs.ID, i+1)
		}
		node := st.Node
		if node == "" {
			node = "node-1"
		}
		desired := swarm.TaskState(st.Desired)
		if desired == "" {
			desired = swarm.TaskStateRunning
		}
		spec := fs.Spec.TaskTemplate
		if st.Old {
			spec.ForceUpdate++
		}
		t := swarm.Task{ID: id, ServiceID: fs.ID, NodeID: node, Slot: st.Slot, DesiredState: desired, Spec: spec}
		t.CreatedAt = scriptedBase.Add(-time.Duration(i) * time.Second)
		t.Status = swarm.TaskStatus{State: swarm.TaskState(st.State), Err: st.Err, Message: st.Message}
		for _, ref := range st.Networks {
			id := ref
			if n, ok := c.resolveNetwork(ref); ok {
				id = n.ID
			}
			t.NetworksAttachments = append(t.NetworksAttachments, swarm.NetworkAttachment{Network: swarm.Network{ID: id}})
		}
		out = append(out, t)
	}
	return out
}

// taskList is the /tasks answer. A task is listed only when every filter the request
// names matches it, as a real daemon lists only what matches: service (an ID or a name),
// node (an ID or a hostname), desired-state and id. A request with no filter at all
// lists nothing, which is not what a real daemon does (it lists every task) but keeps a
// test that forgets to filter from passing by accident.
//
// The tasks are those of AgentTasks, each a task of the service named tierplan-agent
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
		// A label filter matches the labels of the task's service, which the fake keeps as the task's own
		// annotations; "key" alone matches a task that has the key.
		for label := range f["label"] {
			key, value, hasValue := strings.Cut(label, "=")
			got, ok := t.Labels[key]
			if !ok || (hasValue && got != value) {
				return false
			}
		}
		return true
	}
	for _, ft := range c.AgentTasks {
		svc := ft.Service
		if svc == "" {
			svc = "tierplan-agent"
		}
		t := swarm.Task{ID: ft.ID, NodeID: ft.NodeID, ServiceID: svc}
		t.CreatedAt = ft.Created
		t.Status = swarm.TaskStatus{State: swarm.TaskState(ft.State), Err: ft.Error}
		if listed(t) {
			out = append(out, t)
		}
	}
	for _, t := range append(slices.Clone(c.Tasks), c.followingTasks()...) {
		if listed(t) {
			out = append(out, t)
		}
	}
	return out
}
