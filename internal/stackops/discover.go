// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Who a discovered stack belongs to when it is TierPlan's own (D-49). The empty string is a stack that is
// nobody's in particular.
const (
	// OwnerTierPlan is the Compose project or the swarm namespace the TierPlan server itself runs in.
	OwnerTierPlan = "tierplan"
	// OwnerAgent is a group that holds a TierPlan agent service or container.
	OwnerAgent = "agent"
)

// agentLabel marks a TierPlan agent container (and service); the agent install puts it on both.
const agentLabel = "tierplan.agent"

// portainerPrefix starts the label key of every label Portainer puts on what it deploys, and
// portainerComposeDir is where it keeps the compose files of the stacks it makes on a standalone host.
const (
	portainerPrefix     = "io.portainer."
	portainerComposeDir = "/data/compose/"
)

// minSelfIDLen is the shortest prefix of a container ID taken to be TierPlan's own.
const minSelfIDLen = 12

// SelfMarks says which objects of an environment are TierPlan's own: the container the server runs in and
// the swarm namespace of that container. Either may be empty when TierPlan cannot tell.
type SelfMarks struct {
	ContainerID string
	Namespace   string
}

// Discovered is a stack the environment holds that TierPlan did not make: a swarm namespace or a Compose
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
	// OrphanID is the smallest tierplan.stack.id some object of the group carries, empty for none: a stack
	// TierPlan made once, for a record this instance does not hold (D-11). It is never linked to a record.
	OrphanID string
	// Portainer is true when the group carries a Portainer marker (D-19).
	Portainer bool
	// Owner is OwnerTierPlan, OwnerAgent or empty (D-49).
	Owner string
	// NameReason is why the name cannot be a stack name on Target, empty when it can: the stack is listed
	// but not adoptable (D-53). The owner reasons (D-49) come before it, and the caller words them.
	NameReason string
}

// Adoptable says whether the stack can be adopted: it is nobody's own (D-49) and its name follows the rule
// of its target (D-48, D-53).
func (d Discovered) Adoptable() bool { return d.Owner == "" && d.NameReason == "" }

// HoldingName is the one of own that holds name on target, and whether there is one. Swarm looks object names up
// regardless of case, so on a swarm two names that differ only in case are one; a standalone host compares exactly.
// Create and adopt share this rule, so neither can take a name the other would refuse (T-08-95).
func HoldingName(own []Stack, target stacks.Target, name string) (Stack, bool) {
	i := slices.IndexFunc(own, func(s Stack) bool {
		if target == stacks.TargetSwarm {
			return strings.EqualFold(s.Name, name)
		}
		return s.Name == name
	})
	if i < 0 {
		return Stack{}, false
	}
	return own[i], true
}

// nameClashReason is the sentence for an outside stack whose name is a case variant of a managed one on a swarm: it
// is listed, not adoptable.
func nameClashReason(name, managed string) string {
	return "TierPlan can't adopt " + name + ": the stack " + managed + " is already managed here, and a swarm treats names that differ only in case as the same."
}

// NameNotAdoptableReason is the sentence for a stack whose name breaks the stack-name rule of target, built from
// stacks.NameRuleText exactly as the create refusal is, so create and adopt cannot word the rule differently
// (D-53). It is empty when the name follows the rule.
func NameNotAdoptableReason(target stacks.Target, name string) string {
	if stacks.NameValid(target, name) {
		return ""
	}
	where := "a swarm"
	if target != stacks.TargetSwarm {
		where = "a standalone host"
	}
	return "TierPlan can't adopt " + name + ": a stack name on " + where + " takes only " + stacks.NameRuleText(target) + "."
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

// Discover lists the stacks the environment holds that TierPlan does not manage, from what the lists already
// hold. On a swarm a stack is the services that share a com.docker.stack.namespace, on a standalone host the
// containers that share a com.docker.compose.project, one-off containers (docker compose run) left out;
// unlabelled services and loose containers are not stacks. A group named like one of managed is that stack,
// not a second one, which also keeps a stack TierPlan is deploying for the first time out of the list. On a swarm a
// group whose name differs from a managed one only in case is not that stack (swarm tells them apart by their labels)
// but cannot be adopted beside it: it stays in the list, read-only, with the reason (T-08-95).
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
		own := OwnershipOf(DiscoveredStack("", name, target))
		var rows []ServiceRow
		if target == stacks.TargetSwarm {
			rows = swarmRowsWhere(name, g.services, own.OwnsService)
		} else {
			rows = standaloneRowsWhere(g.containers, func(c dockerx.ContainerRow) bool { return own.OwnsWorkload(c.Labels) })
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
		d.NameReason = NameNotAdoptableReason(target, name)
		if d.NameReason == "" {
			if m, clash := HoldingName(managed, target, name); clash {
				d.NameReason = nameClashReason(name, m.Name)
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Discovered) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// flagsOf reads what a group's labels say about it: the smallest TierPlan stack ID, whether Portainer made it
// and whether it is TierPlan's own or its agent's.
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
		owner = OwnerTierPlan
	}
	return orphan, portainer, owner
}

// DiscoverNamed looks for one stack the environment holds that TierPlan did not make, live: one read of the
// cluster for a swarm, one list of the project's containers for a standalone host, never the lists' cache.
// The bool is false when nothing carries the name. A name that a stack of TierPlan's holds exactly is that stack's
// and the caller says so before it asks; managed is only used for what Discover does with it: on a swarm a case
// variant of a managed name is found, with the reason it cannot be adopted. It is a read.
func (d Deps) DiscoverNamed(ctx context.Context, envID string, target stacks.Target, name string, managed []Stack, self SelfMarks) (Discovered, bool, error) {
	var h Held
	switch target {
	case stacks.TargetSwarm:
		if d.Clusters == nil {
			return Discovered{}, false, errors.New("stackops: no cluster reader to read the swarm with")
		}
		if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
			read, e := c.Cluster(ctx)
			if e != nil {
				return e
			}
			for _, r := range read.Services {
				if r.Stack == name {
					h.Services = append(h.Services, r)
				}
			}
			return nil
		}); err != nil {
			return Discovered{}, false, err
		}
	case stacks.TargetStandalone:
		node, err := d.StandaloneNode(ctx, envID)
		if err != nil {
			return Discovered{}, false, err
		}
		if err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
			var e error
			h.Containers, e = c.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
			return e
		}); err != nil {
			return Discovered{}, false, err
		}
	default:
		return Discovered{}, false, errors.New("stackops: unknown stack target " + string(target))
	}
	for _, found := range Discover(h, managed, target, self) {
		if found.Name == name {
			return found, true, nil
		}
	}
	return Discovered{}, false, nil
}
