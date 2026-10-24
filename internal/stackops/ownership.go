// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Ownership decides which objects of an engine belong to a stack. It is the one place that does, so the
// overview, the resources and the logs serve every kind of stack with the same rule (D-09, D-13).
//
// A managed stack owns what carries its namespace or project and its own bayplan.stack.id: a name alone
// is never ownership, since another stack, or a person, can make an object under the same name. A
// discovered stack (Stack.Discovered) is a name found on the engine and has no ID, so it owns everything
// under its namespace or project label, whoever made it. The adopted form arrives with plan 08-13.
type Ownership struct {
	// StackID is the stored stack's ID; empty for a discovered stack.
	StackID string
	// Name is the swarm namespace or the Compose project.
	Name string
	// Target says which label the name is on: the namespace on a swarm, the project on a host.
	Target stacks.Target
	// Discovered is true for a stack found by its labels that Bayplan does not hold.
	Discovered bool
}

// OwnershipOf is the ownership rule of a stack.
func OwnershipOf(st Stack) Ownership {
	return Ownership{StackID: st.ID, Name: st.Name, Target: st.Target, Discovered: st.Discovered}
}

// labelKey is the label the stack's name is on.
func (o Ownership) labelKey() string {
	if o.Target == stacks.TargetSwarm {
		return stacks.LabelSwarmNamespace
	}
	return stacks.LabelComposeProject
}

// owns applies the rule to a namespace or project as an object states it, and its labels.
func (o Ownership) owns(name string, labels map[string]string) bool {
	if name != o.Name || o.Name == "" {
		return false
	}
	if o.Discovered {
		return true
	}
	// A stored stack with no ID owns nothing: an object with no bayplan.stack.id must never match it.
	return o.StackID != "" && labels[stacks.LabelStackID] == o.StackID
}

// OwnsWorkload says whether a service or a container of the engine is the stack's, from its labels.
func (o Ownership) OwnsWorkload(labels map[string]string) bool {
	return o.owns(labels[o.labelKey()], labels)
}

// OwnsService is OwnsWorkload for a swarm service row, whose namespace the engine reader has already taken
// from the label.
func (o Ownership) OwnsService(r dockerx.ServiceRow) bool {
	return o.owns(r.Stack, r.Labels)
}

// OwnsNetwork says whether a network is the stack's, from its labels. The name is not used yet: it is for
// the adopted form.
func (o Ownership) OwnsNetwork(_ string, labels map[string]string) bool {
	return o.OwnsWorkload(labels)
}

// OwnsVolume says whether a volume is the stack's, from its labels. The name is not used yet: it is for the
// adopted form.
func (o Ownership) OwnsVolume(_ string, labels map[string]string) bool {
	return o.OwnsWorkload(labels)
}
