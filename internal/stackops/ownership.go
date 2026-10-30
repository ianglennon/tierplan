// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"slices"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/rebuild"
)

// Ownership decides which objects of an engine belong to a stack. It is the one place that does, so the
// overview, the resources and the logs serve every kind of stack with the same rule (D-09, D-13).
//
// A managed stack owns what carries its namespace or project and its own coaming.stack.id: a name alone
// is never ownership, since another stack, or a person, can make an object under the same name. A
// discovered stack (Stack.Discovered) is a name found on the engine and has no ID, so it owns everything
// under its namespace or project label, whoever made it.
//
// An adopted stack (plan 08-13) is a stack Coaming took over without touching the engine, so its objects do not
// carry its ID yet. Until its first deploy (Pending) its services and containers are owned when they carry its
// namespace or project and either no coaming.stack.id or the stale one recorded at adoption (D-11); its networks
// and volumes are owned by the names recorded at adoption, for as long as the stack lives (D-18). A
// coaming.stack.id alone never links an object to a stack: another ID is another stack's (T-08-47).
//
// A container on a host is in one more class for the drift check: an unlabelled one, with no coaming.stack.id, in a
// project the stack owns a container of. The person added it from the host CLI, so it is not owned (OwnsWorkload
// stays false), and it is not another stack's either (SplitProject).
type Ownership struct {
	// StackID is the stored stack's ID; empty for a discovered stack.
	StackID string
	// Name is the swarm namespace or the Compose project.
	Name string
	// Target says which label the name is on: the namespace on a swarm, the project on a host.
	Target stacks.Target
	// Discovered is true for a stack found by its labels that Coaming does not hold.
	Discovered bool
	// Adopted is true for a stack Coaming took over (source kind adopted), Pending until its first deploy has put
	// Coaming's labels on what it runs, and PriorID the coaming.stack.id its objects carried when it was adopted.
	Adopted, Pending bool
	PriorID          string
	// Networks and Volumes are the engine names of the networks and volumes an adopted stack owns, and
	// Secrets and Configs the names of the existing secrets and configs it references as external.
	Networks, Volumes, Secrets, Configs []string
}

// OwnershipOf is the ownership rule of a stack.
func OwnershipOf(st Stack) Ownership {
	o := Ownership{StackID: st.ID, Name: st.Name, Target: st.Target, Discovered: st.Discovered}
	if st.Source.Kind == stacks.SourceAdopted {
		a := st.Adoption
		o.Adopted, o.Pending, o.PriorID = true, a.Pending, a.PriorID
		o.Networks, o.Volumes, o.Secrets, o.Configs = a.Networks, a.Volumes, a.Secrets, a.Configs
	}
	return o
}

// labelKey is the label the stack's name is on.
func (o Ownership) labelKey() string {
	if o.Target == stacks.TargetSwarm {
		return stacks.LabelSwarmNamespace
	}
	return stacks.LabelComposeProject
}

// inName says whether an object states the stack's namespace or project as its own.
func (o Ownership) inName(name string) bool { return name == o.Name && o.Name != "" }

// ownsExactly is the managed rule: the object states the stack's name and carries the stack's own ID. A stored
// stack with no ID owns nothing this way: an object with no coaming.stack.id must never match it.
func (o Ownership) ownsExactly(name string, labels map[string]string) bool {
	if !o.inName(name) {
		return false
	}
	if o.Discovered {
		return true
	}
	return o.StackID != "" && labels[stacks.LabelStackID] == o.StackID
}

// owns applies the rule to a namespace or project as a service or a container states it, and its labels. An
// adopted stack that has not been deployed yet also owns the workloads that carry no ID, or the ID recorded at
// adoption.
func (o Ownership) owns(name string, labels map[string]string) bool {
	if o.ownsExactly(name, labels) {
		return true
	}
	if !o.Adopted || !o.Pending || o.Discovered || o.StackID == "" || !o.inName(name) {
		return false
	}
	id := labels[stacks.LabelStackID]
	return id == "" || (o.PriorID != "" && id == o.PriorID)
}

// OwnsWorkload says whether a service or a container of the engine is the stack's, from its labels.
func (o Ownership) OwnsWorkload(labels map[string]string) bool {
	return o.owns(labels[o.labelKey()], labels)
}

// Claims says whether an object that states the stack's namespace or project as name, and the coaming.stack.id id (empty
// for none), is linked to the stack: it carries the stack's own ID, or the stack is adopted and pending and the object
// carries no ID or the one recorded at adoption. It is OwnsWorkload for a caller that holds the two values a page
// shows and not the labels. An ID is a claim, never a fact: another stack's ID under this name links to nothing.
func (o Ownership) Claims(name, id string) bool {
	return o.owns(name, map[string]string{stacks.LabelStackID: id})
}

// ClaimsObject is Claims for a secret or a config, which a stack owns by its own ID alone (OwnsObject).
func (o Ownership) ClaimsObject(name, id string) bool {
	return o.ownsExactly(name, map[string]string{stacks.LabelStackID: id})
}

// OwnsService is OwnsWorkload for a swarm service row, whose namespace the engine reader has already taken
// from the label.
func (o Ownership) OwnsService(r dockerx.ServiceRow) bool {
	return o.owns(r.Stack, r.Labels)
}

// OwnsNetwork says whether a network is the stack's: it carries the stack's name and ID, or the stack was
// adopted and the network is one of those recorded then (D-18), whatever its labels say.
func (o Ownership) OwnsNetwork(name string, labels map[string]string) bool {
	return o.ownsExactly(labels[o.labelKey()], labels) || (o.Adopted && o.StackID != "" && slices.Contains(o.Networks, name))
}

// OwnsVolume is OwnsNetwork for a volume.
func (o Ownership) OwnsVolume(name string, labels map[string]string) bool {
	return o.ownsExactly(labels[o.labelKey()], labels) || (o.Adopted && o.StackID != "" && slices.Contains(o.Volumes, name))
}

// OwnsObject says whether a swarm secret or config is the stack's, from its labels: it states the stack's namespace
// and carries the stack's own ID. It is the label rule alone: a secret or config an adopted stack only references
// is Knows, never owned, so the deploy cannot remove it (T-08-62).
func (o Ownership) OwnsObject(labels map[string]string) bool {
	return o.ownsExactly(labels[o.labelKey()], labels)
}

// Knows says whether an adopted stack recorded the object when it was adopted: a network or a volume it owns, or a
// secret or a config it references as external. Such an object is the stack's own business and never foreign to
// it, whatever labels it carries. kind is one of rebuild.KindNetwork, KindVolume, KindSecret and KindConfig.
func (o Ownership) Knows(kind, name string) bool {
	if !o.Adopted {
		return false
	}
	switch kind {
	case rebuild.KindNetwork:
		return slices.Contains(o.Networks, name)
	case rebuild.KindVolume:
		return slices.Contains(o.Volumes, name)
	case rebuild.KindSecret:
		return slices.Contains(o.Secrets, name)
	case rebuild.KindConfig:
		return slices.Contains(o.Configs, name)
	}
	return false
}

// foreignNetwork says whether a network found under the stack's name is somebody else's: it is neither the
// stack's nor one the stack recorded when it was adopted.
func (o Ownership) foreignNetwork(name string, labels map[string]string) bool {
	return !o.OwnsNetwork(name, labels) && !o.Knows(rebuild.KindNetwork, name)
}

// foreignVolume is foreignNetwork for a volume.
func (o Ownership) foreignVolume(name string, labels map[string]string) bool {
	return !o.OwnsVolume(name, labels) && !o.Knows(rebuild.KindVolume, name)
}

// SplitProject sorts the containers of an engine into the three classes the stack has under its name, from their
// labels (G-08-6). It is the one place the rule lives, so the drift check and the drift worker's fingerprint agree.
//
//   - A container whose project or namespace (labelKey) is not the stack's name is in no list.
//   - owned: OwnsWorkload says it is the stack's.
//   - unlabelled: on a host, a stack Coaming holds (not Discovered, with an ID) meets a container of its project that
//     carries no coaming.stack.id, and owns another container of the same Compose service. It is what the person adds
//     to the project from the host CLI (docker compose up --scale), and counts as the stack's wherever the stack
//     compares scale.
//   - foreign: every other container under the name. Another non-empty ID is always another stack's (T-08-103). A
//     container with no ID in a project the stack owns nothing of is a project Coaming did not deploy or adopt, so it
//     is not taken over silently (T-08-104, P7 D-12). Nor is a container with no ID of a service no container of the
//     stack runs: an unrelated project that resolves to the same name looks just like it, and Deploy, Restore and
//     Remove would delete it as an orphan (WR-03, T-08-113).
//
// A swarm stack has no unlabelled class: its services state the namespace the stack deploy wrote.
func SplitProject[T any](o Ownership, all []T, labels func(T) map[string]string) (owned, unlabelled, foreign []T) {
	var noID []T
	for _, c := range all {
		l := labels(c)
		switch {
		case !o.inName(l[o.labelKey()]):
		case o.OwnsWorkload(l):
			owned = append(owned, c)
		case l[stacks.LabelStackID] == "" && o.Target != stacks.TargetSwarm && !o.Discovered && o.StackID != "":
			noID = append(noID, c)
		default:
			foreign = append(foreign, c)
		}
	}
	services := map[string]bool{}
	for _, c := range owned {
		if svc := labels(c)[stacks.LabelComposeService]; svc != "" {
			services[svc] = true
		}
	}
	for _, c := range noID {
		if services[labels(c)[stacks.LabelComposeService]] {
			unlabelled = append(unlabelled, c)
		} else {
			foreign = append(foreign, c)
		}
	}
	return owned, unlabelled, foreign
}
