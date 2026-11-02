// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
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

// ContainerInput is everything ClassifyContainer decides on for one swarm task container.
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
}

// taskContainers builds the input of every swarm task container of a snapshot, with its rank in
// its group. Containers that are not exited are in the result (ClassifyContainer drops them) but
// are not ranked, so they never count towards a group's KeepN.
func taskContainers(s Snapshot, p Policy) []ContainerInput {
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
		ci := ContainerInput{
			Container: c, KeepN: p.keepN(), Grace: p.grace(),
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
	for _, idx := range groups {
		slices.SortStableFunc(idx, func(a, b int) int {
			return cmp.Or(out[a].Container.Created.Compare(out[b].Container.Created), strings.Compare(out[a].Container.ID, out[b].Container.ID))
		})
		for rank, i := range idx {
			out[i].Rank, out[i].Total = rank+1, len(idx)
		}
	}
	return out
}

// beyondKeep reports whether a ranked container is older than the newest KeepN of its group.
func (ci ContainerInput) beyondKeep() bool {
	return ci.Rank > 0 && ci.Rank <= ci.Total-ci.KeepN
}
