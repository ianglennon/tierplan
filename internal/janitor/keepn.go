// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// The labels Docker puts on the container of a swarm task.
const (
	labelSwarmPrefix      = "com.docker.swarm."
	labelSwarmTaskID      = "com.docker.swarm.task.id"
	labelSwarmTaskName    = "com.docker.swarm.task.name"
	labelSwarmServiceID   = "com.docker.swarm.service.id"
	labelSwarmServiceName = "com.docker.swarm.service.name"
	// labelStackNamespace is the label docker stack deploy puts on a swarm service's containers.
	labelStackNamespace = "com.docker.stack.namespace"
)

// isSwarmTask reports whether a container is the container of a swarm task: it carries any label
// of the swarm's own. A container that carries some of them and not the rest is still one, and the
// planner leaves it alone (task_labels_missing) instead of guessing.
func isSwarmTask(c Container) bool {
	for k := range c.Labels {
		if strings.HasPrefix(k, labelSwarmPrefix) {
			return true
		}
	}
	return false
}

// taskGroup places a swarm task container in its group: the exited containers of one slot of a
// replicated service, or of one node for a global service. key identifies the group, slot is the
// replica slot of a replicated service and empty for a node group. ok is false when the labels do
// not place the container.
//
// The task name is {service name}.{slot or node ID}.{task ID}. The service name may hold dots, so
// the middle is what is left after the labelled service name and task ID are cut from the ends.
// global is true when the manager lists the service as global; without a manager the middle
// decides, because a slot is a number and a node ID never is.
func taskGroup(c Container, global bool) (key, slot string, ok bool) {
	svcID, svcName := c.Labels[labelSwarmServiceID], c.Labels[labelSwarmServiceName]
	taskID, taskName := c.Labels[labelSwarmTaskID], c.Labels[labelSwarmTaskName]
	if svcID == "" || svcName == "" || taskID == "" || taskName == "" {
		return "", "", false
	}
	rest, found := strings.CutPrefix(taskName, svcName+".")
	if !found {
		return "", "", false
	}
	middle, found := strings.CutSuffix(rest, "."+taskID)
	if !found || middle == "" {
		return "", "", false
	}
	if n, err := strconv.Atoi(middle); err == nil && n >= 0 && !global {
		return svcID + "/slot/" + middle, middle, true
	}
	return svcID + "/node/" + c.NodeID, "", true
}

// ContainerInput is everything ClassifyContainer decides on for one container: a swarm task
// container, or (Standalone) any other.
type ContainerInput struct {
	Container Container
	// Group is the key of the container's group, empty when its labels do not place it. Slot is the
	// replica slot, empty for a node group.
	Group, Slot string
	// Rank is the container's place among the exited containers of its group, 1 for the oldest;
	// Total is how many there are.
	Rank, Total int
	// KeepN is how many of the newest of a group are kept.
	KeepN int
	// DesiredRunning: the swarm wants this container's task running.
	DesiredRunning bool
	// ServiceGone: a manager answered and the container's service is not among its services.
	ServiceGone bool
	// End and Inspected are the inspect facts, and whether the container was inspected at all.
	End       ContainerEnd
	Inspected bool
	Grace     time.Duration

	// Standalone is true for a container that is not the container of a swarm task. The fields below
	// are its own; a task container leaves them zero.
	Standalone bool
	// Project is the compose project the container is labelled with, StackID the TierPlan stack it
	// is labelled with; both are empty for a plain docker run container. Oneoff is true for a
	// container left by docker compose run.
	Project, StackID string
	Oneoff           bool
	// StacksRead is true when TierPlan's stack records were read. StackOwned: a TierPlan stack that
	// exists owns the container's project (by the stack ID, or a stack named as the project).
	// StackGone: the container carries the ID of a TierPlan stack that no longer exists.
	StacksRead, StackOwned, StackGone bool
	// ProjectBusy: another container of the project (or of the stack) is in any state but exited.
	// ProjectOthers: another container of the project exists, in any state.
	ProjectBusy, ProjectOthers bool
	// OptIn is the policy's "also remove stopped standalone containers".
	OptIn bool
	// NodeOff: the janitor is switched off for the container's swarm node, or the environment has node
	// settings and the node is not known yet as a swarm node. Nothing on it is inspected or removed, and
	// Build lists what it would have judged as protected (node_disabled or node_unmapped).
	NodeOff bool
}

// taskContainers builds the input of every swarm task container of a snapshot, with its rank in
// its group. Containers that are not exited are in the result (ClassifyContainer drops them) but
// are not ranked, so they never count towards a group's KeepN.
func taskContainers(s Snapshot, p Policy, nodes map[string]NodeOverrides) []ContainerInput {
	wanted := map[string]bool{}
	for _, t := range s.Tasks {
		if t.DesiredState != "running" {
			continue
		}
		if t.ID != "" {
			wanted[t.ID] = true
		}
		if t.ContainerID != "" {
			wanted[t.ContainerID] = true
		}
	}
	var out []ContainerInput
	groups := map[string][]int{}
	for _, c := range s.Containers {
		if !isSwarmTask(c) {
			continue
		}
		svcID := c.Labels[labelSwarmServiceID]
		svc, known := s.Services[svcID]
		// KeepN is the node's own where it sets one. The group is still the whole slot, ranked across
		// the nodes it ran on, so each container is judged against the N of the node it is on.
		ci := ContainerInput{
			Container: c, KeepN: nodePolicy(s, p, nodes, c.NodeID).keepN(), Grace: p.grace(),
			DesiredRunning: wanted[c.ID] || (c.Labels[labelSwarmTaskID] != "" && wanted[c.Labels[labelSwarmTaskID]]),
			ServiceGone:    s.ManagerRead && s.Services != nil && svcID != "" && !known,
		}
		if key, slot, ok := taskGroup(c, svc.Global); ok {
			ci.Group, ci.Slot = key, slot
			if c.State == stateExited {
				groups[key] = append(groups[key], len(out))
			}
		}
		out = append(out, ci)
	}
	rankGroups(out, groups)
	return out
}

// rankGroups sets the Rank and Total of every container of every group, the oldest first. groups
// maps a group key to the indexes in out of its exited containers.
func rankGroups(out []ContainerInput, groups map[string][]int) {
	for _, idx := range groups {
		slices.SortStableFunc(idx, func(a, b int) int {
			return cmp.Or(out[a].Container.Created.Compare(out[b].Container.Created), strings.Compare(out[a].Container.ID, out[b].Container.ID))
		})
		for rank, i := range idx {
			out[i].Rank, out[i].Total = rank+1, len(idx)
		}
	}
}

// standaloneContainers builds the input of every container that is not the container of a swarm
// task, with the facts the standalone rules decide on. The one-offs of a compose service are
// ranked per node, because a compose project lives on one engine: the newest KeepN of them stay.
func standaloneContainers(s Snapshot, p Policy) []ContainerInput {
	// How many containers share each compose project and each TierPlan stack ID, and how many of
	// those are anything but exited. The container being judged is exited, so it adds to the first
	// count and never to the second.
	type tally struct{ all, live int }
	projects, stackIDs := map[string]tally{}, map[string]tally{}
	for _, c := range s.Containers {
		live := 0
		if c.State != stateExited {
			live = 1
		}
		count := func(m map[string]tally, key string) {
			if key == "" {
				return
			}
			t := m[key]
			t.all++
			t.live += live
			m[key] = t
		}
		count(projects, c.Labels[stacks.LabelComposeProject])
		count(stackIDs, c.Labels[stacks.LabelStackID])
	}
	var out []ContainerInput
	groups := map[string][]int{}
	for _, c := range s.Containers {
		if isSwarmTask(c) {
			continue
		}
		project, stackID := c.Labels[stacks.LabelComposeProject], c.Labels[stacks.LabelStackID]
		ci := ContainerInput{
			Container: c, Standalone: true, KeepN: p.keepN(), Grace: p.grace(), OptIn: p.RemoveStoppedContainers,
			Project: project, StackID: stackID, Oneoff: c.Labels[stacks.LabelComposeOneoff] == composeOneoff,
			StacksRead: s.StacksRead,
		}
		if s.StacksRead {
			ci.StackOwned = (stackID != "" && s.StackIDs[stackID]) || (project != "" && s.StackNames[project])
			ci.StackGone = stackID != "" && !ci.StackOwned
		}
		ci.ProjectBusy = (project != "" && projects[project].live > 0) || (stackID != "" && stackIDs[stackID].live > 0)
		ci.ProjectOthers = project != "" && projects[project].all > 1
		if ci.Oneoff && c.State == stateExited {
			ci.Group = strings.Join([]string{"oneoff", c.NodeID, project, c.Labels[stacks.LabelComposeService]}, "/")
			groups[ci.Group] = append(groups[ci.Group], len(out))
		}
		out = append(out, ci)
	}
	rankGroups(out, groups)
	return out
}

// allContainers is the input of every container of a snapshot: the swarm's task containers, then
// the rest. nodes are the overrides of the swarm nodes by swarm node ID, nil when none sets any.
func allContainers(s Snapshot, p Policy, nodes map[string]NodeOverrides) []ContainerInput {
	out := append(taskContainers(s, p, nodes), standaloneContainers(s, p)...)
	if len(nodes) > 0 {
		for i := range out {
			out[i].NodeOff = heldNode(s, nodes, out[i].Container.NodeID) != ""
		}
	}
	return out
}

// heldNode is why the node settings make the janitor leave the snapshot node nodeID alone: node_disabled
// when the janitor is switched off for its swarm node, and node_unmapped when the environment is a swarm with
// node settings and no agent has reported which swarm node nodeID is yet. Such a node would otherwise follow
// its environment, and a node a person switched off could be cleaned in the moment before its agent reports.
// "" when neither holds.
func heldNode(s Snapshot, nodes map[string]NodeOverrides, nodeID string) ReasonCode {
	if len(nodes) == 0 || nodeID == "" {
		return ""
	}
	if _, own := nodes[nodeID]; s.Swarm && s.SwarmNodeOf[nodeID] == "" && !own {
		return ReasonNodeUnmapped
	}
	if nodeOverrides(s, nodes, nodeID).Disabled() {
		return ReasonNodeDisabled
	}
	return ""
}

// nodeOverrides are the overrides of the swarm node that a snapshot node is: the node IDs of a snapshot
// are the ones the engines are reached by, and Snapshot.SwarmNodeOf maps them to the swarm node IDs the
// overrides are stored under. A node the map does not know is looked up by its own ID.
func nodeOverrides(s Snapshot, nodes map[string]NodeOverrides, nodeID string) NodeOverrides {
	if len(nodes) == 0 {
		return NodeOverrides{}
	}
	if id := s.SwarmNodeOf[nodeID]; id != "" {
		return nodes[id]
	}
	return nodes[nodeID]
}

// beyondKeep reports whether a ranked container is older than the newest KeepN of its group.
func (ci ContainerInput) beyondKeep() bool {
	return ci.Rank > 0 && ci.Rank <= ci.Total-ci.KeepN
}
