// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"slices"
	"strings"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Who a discovered stack belongs to when it is Bayplan's own (D-49). The empty string is a stack that is
// nobody's in particular.
const (
	// OwnerBayplan is the Compose project or the swarm namespace the Bayplan server itself runs in.
	OwnerBayplan = "bayplan"
	// OwnerAgent is a group that holds a Bayplan agent service or container.
	OwnerAgent = "agent"
)

// agentLabel marks a Bayplan agent container (and service); the agent install puts it on both.
const agentLabel = "bayplan.agent"

// portainerPrefix starts the label key of every label Portainer puts on what it deploys, and
// portainerComposeDir is where it keeps the compose files of the stacks it makes on a standalone host.
const (
	portainerPrefix     = "io.portainer."
	portainerComposeDir = "/data/compose/"
)

// minSelfIDLen is the shortest prefix of a container ID taken to be Bayplan's own.
const minSelfIDLen = 12

// SelfMarks says which objects of an environment are Bayplan's own: the container the server runs in and
// the swarm namespace of that container. Either may be empty when Bayplan cannot tell.
type SelfMarks struct {
	ContainerID string
	Namespace   string
}

// Discovered is a stack the environment holds that Bayplan did not make: a swarm namespace or a Compose
// project, found by its labels alone (D-07). Nothing about it is stored; Docker is the source of truth.
type Discovered struct {
	// Name is the namespace or the project, and Target where it runs.
	Name   string
	Target stacks.Target
	// Rows are the services of the group, in name order.
	Rows []ServiceRow
	// Running and Total are the services that run and the services there are.
	Running, Total int
	// State is StateRunning, StateDegraded or StateNotRunning, counted from the services.
	State StackState
	// OrphanID is the smallest bayplan.stack.id some object of the group carries, empty for none: a stack
	// Bayplan made once, for a record this instance does not hold (D-11). It is never linked to a record.
	OrphanID string
	// Portainer is true when the group carries a Portainer marker (D-19).
	Portainer bool
	// Owner is OwnerBayplan, OwnerAgent or empty (D-49).
	Owner string
}

// PortainerMarked says whether labels carry a mark of Portainer: a key that starts io.portainer., or a
// Compose working directory under the place Portainer keeps its stacks' files.
func PortainerMarked(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, portainerPrefix) {
			return true
		}
	}
	return strings.HasPrefix(labels[stacks.LabelComposeWorkingDir], portainerComposeDir)
}

// group is what one namespace or project holds, as Discover collects it.
type group struct {
	services   []dockerx.ServiceRow
	containers []dockerx.ContainerRow
}

// Discover lists the stacks the environment holds that Bayplan does not manage, from what the lists already
// hold. On a swarm a stack is the services that share a com.docker.stack.namespace, on a standalone host the
// containers that share a com.docker.compose.project, one-off containers (docker compose run) left out;
// unlabelled services and loose containers are not stacks. A group named like one of managed is that stack,
// not a second one, which also keeps a stack Bayplan is deploying for the first time out of the list.
//
// It is a pure function of its arguments and reads no engine: the same data gives the same stacks in the
// same order, by name. A label ID is only ever a flag (OrphanID), never a key to a stored stack.
func Discover(h Held, managed []Stack, target stacks.Target, self SelfMarks) []Discovered {
	taken := make(map[string]bool, len(managed))
	for _, s := range managed {
		taken[s.Name] = true
	}
	groups := map[string]*group{}
	of := func(name string) *group {
		g := groups[name]
		if g == nil {
			g = &group{}
			groups[name] = g
		}
		return g
	}
	if target == stacks.TargetSwarm {
		for _, r := range h.Services {
			if r.Stack != "" && !taken[r.Stack] {
				g := of(r.Stack)
				g.services = append(g.services, r)
			}
		}
	} else {
		for _, c := range h.Containers {
			name := c.Labels[stacks.LabelComposeProject]
			if name == "" || taken[name] || strings.EqualFold(c.Labels[stacks.LabelComposeOneoff], "true") {
				continue
			}
			g := of(name)
			g.containers = append(g.containers, c)
		}
	}

	out := make([]Discovered, 0, len(groups))
	for name, g := range groups {
		all := func(dockerx.ServiceRow) bool { return true }
		var rows []ServiceRow
		if target == stacks.TargetSwarm {
			rows = swarmRowsWhere(name, g.services, all)
		} else {
			rows = standaloneRowsWhere(g.containers, func(dockerx.ContainerRow) bool { return true })
		}
		if len(rows) == 0 {
			continue
		}
		d := Discovered{Name: name, Target: target, Rows: rows, Total: len(rows)}
		for _, r := range rows {
			if r.State == ServiceRunning {
				d.Running++
			}
		}
		d.State, _, _ = DeriveState(StateInput{Deployed: true, Services: rows})
		d.OrphanID, d.Portainer, d.Owner = flagsOf(name, g, self)
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Discovered) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// flagsOf reads what a group's labels say about it: the smallest Bayplan stack ID, whether Portainer made it
// and whether it is Bayplan's own or its agent's.
func flagsOf(name string, g *group, self SelfMarks) (orphan string, portainer bool, owner string) {
	note := func(labels map[string]string) {
		if id := labels[stacks.LabelStackID]; id != "" && (orphan == "" || id < orphan) {
			orphan = id
		}
		if PortainerMarked(labels) {
			portainer = true
		}
	}
	agent, own := false, self.Namespace != "" && name == self.Namespace && len(g.services) > 0
	for _, s := range g.services {
		note(s.Labels)
		agent = agent || s.Agent || s.Labels[agentLabel] == "true"
	}
	for _, c := range g.containers {
		note(c.Labels)
		agent = agent || c.Labels[agentLabel] == "true"
		own = own || (len(self.ContainerID) >= minSelfIDLen && strings.HasPrefix(c.ID, self.ContainerID))
	}
	switch {
	case agent:
		owner = OwnerAgent
	case own:
		owner = OwnerBayplan
	}
	return orphan, portainer, owner
}
