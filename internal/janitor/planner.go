// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// Kind is the type of object an item is.
type Kind string

// The kinds, in the order a plan lists them.
const (
	KindContainer Kind = "container"
	KindNetwork   Kind = "network"
	KindImage     Kind = "image"
)

// kindRank orders the kinds of a plan: containers, networks, images.
func kindRank(k Kind) int {
	switch k {
	case KindContainer:
		return 0
	case KindNetwork:
		return 1
	default:
		return 2
	}
}

// Class is what the planner made of one object. Only ClassRemovable items are removed; every
// other class is why an object was looked at and left.
type Class string

// The classes.
const (
	// ClassRemovable: the janitor would remove it now.
	ClassRemovable Class = "removable"
	// ClassReported: it is worth a person's eye, but the policy does not remove it (an opt-in is
	// off).
	ClassReported Class = "reported"
	// ClassProtected: a person or a label asked for it to be kept.
	ClassProtected Class = "protected"
	// ClassGrace: it qualifies, but has not been stopped or idle for the whole grace period.
	ClassGrace Class = "grace"
	// ClassCantTell: the planner cannot be sure, so it leaves the object alone.
	ClassCantTell Class = "cant_tell"
)

// ReasonCode says why an object has its class. The whole set is declared here so that the API's
// enum is stable from the first release of the route; a later plan starts using a code, it does
// not add one.
type ReasonCode string

// The reason codes.
const (
	// ReasonTaskBeyondKeep: an exited swarm task container older than the newest KeepN of its
	// service slot (of its node, for a global service).
	ReasonTaskBeyondKeep ReasonCode = "task_beyond_keep"
	// ReasonTaskServiceGone: an exited swarm task container whose service no longer exists.
	ReasonTaskServiceGone ReasonCode = "task_service_gone"
	// ReasonStackGone: an object labelled with a swarm stack that no longer exists.
	ReasonStackGone ReasonCode = "stack_gone"
	// ReasonComposeProjectGone: an object labelled with a compose project that no longer exists.
	ReasonComposeProjectGone ReasonCode = "compose_project_gone"
	// ReasonComposeOneoff: a container left by docker compose run.
	ReasonComposeOneoff ReasonCode = "compose_oneoff"
	// ReasonStoppedStandalone: a stopped container no stack or project ever owned.
	ReasonStoppedStandalone ReasonCode = "stopped_standalone"
	// ReasonDanglingImage: an image with no tag, no digest and no user.
	ReasonDanglingImage ReasonCode = "dangling_image"
	// ReasonNetworkOrphaned: a network whose stack or project is gone.
	ReasonNetworkOrphaned ReasonCode = "network_orphaned"
	// ReasonNetworkUnused: a network no stack ever owned, with nothing attached.
	ReasonNetworkUnused ReasonCode = "network_unused"
	// ReasonOptInOffContainers and ReasonOptInOffNetworks: the object qualifies, but the opt-in
	// that removes this kind of object is off.
	ReasonOptInOffContainers ReasonCode = "opt_in_off_containers"
	ReasonOptInOffNetworks   ReasonCode = "opt_in_off_networks"
	// ReasonLoneComposeContainer: a stopped compose container whose project still has others.
	ReasonLoneComposeContainer ReasonCode = "lone_compose_container"
	// ReasonKeptLabel: the object carries tierplan.keep=true.
	ReasonKeptLabel ReasonCode = "kept_label"
	// ReasonKeptRow: a person kept it from the janitor's list.
	ReasonKeptRow ReasonCode = "kept_row"
	// ReasonStackExcluded: the stack that owns it is excluded from the janitor.
	ReasonStackExcluded ReasonCode = "stack_excluded"
	// ReasonStackDeclaredKeep: the stack's own compose file marks the object kept.
	ReasonStackDeclaredKeep ReasonCode = "stack_declared_keep"
	// ReasonYoungerThanGrace: stopped or idle for less than the grace period.
	ReasonYoungerThanGrace ReasonCode = "younger_than_grace"
	// ReasonNodeNotReporting: the node that holds it did not report.
	ReasonNodeNotReporting ReasonCode = "node_not_reporting"
	// ReasonNetworksWithheld: no network is judged while a node that could hold an attachment is
	// missing.
	ReasonNetworksWithheld ReasonCode = "networks_withheld"
	// ReasonStackUnknown: a stack did not load, so what it owns is not known.
	ReasonStackUnknown ReasonCode = "stack_unknown"
	// ReasonInspectCapped: the container was not inspected (over the cap, or the inspect failed),
	// so its end time and size are not known.
	ReasonInspectCapped ReasonCode = "inspect_capped"
	// ReasonClockSkew: the container finished in the future by the host's clock.
	ReasonClockSkew ReasonCode = "clock_skew"
	// ReasonTaskLabelsMissing: a swarm task container without the labels that place it in a
	// service slot.
	ReasonTaskLabelsMissing ReasonCode = "task_labels_missing"
	// ReasonServicesUnread: no manager answered, so which services exist is not known.
	ReasonServicesUnread ReasonCode = "services_unread"
)

// Reasons lists every reason code, for the API's enum.
func Reasons() []ReasonCode {
	return []ReasonCode{
		ReasonTaskBeyondKeep, ReasonTaskServiceGone, ReasonStackGone, ReasonComposeProjectGone, ReasonComposeOneoff,
		ReasonStoppedStandalone, ReasonDanglingImage, ReasonNetworkOrphaned, ReasonNetworkUnused,
		ReasonOptInOffContainers, ReasonOptInOffNetworks, ReasonLoneComposeContainer, ReasonKeptLabel, ReasonKeptRow,
		ReasonStackExcluded, ReasonStackDeclaredKeep, ReasonYoungerThanGrace, ReasonNodeNotReporting,
		ReasonNetworksWithheld, ReasonStackUnknown, ReasonInspectCapped, ReasonClockSkew, ReasonTaskLabelsMissing,
		ReasonServicesUnread,
	}
}

// Reason is why an item has its class, with the figures a sentence about it needs.
type Reason struct {
	Code ReasonCode
	// Service is the swarm service, Slot its replica slot ("" for a global service), Project the
	// compose project, Stack the swarm stack, Node the node a global service's group is on.
	Service, Slot, Project, Stack, Node string
	// Rank is the container's place among the exited containers of its group, 1 for the oldest.
	Rank int
	// Age is how long the object has been stopped or idle. Remaining is how much of the grace
	// period is left, for ClassGrace.
	Age, Remaining time.Duration
}

// Item is one object the planner looked at.
type Item struct {
	Type Kind
	// NodeID and Node are the node the object is on; Ref is the engine's ID of it and Name its name.
	NodeID, Node, Ref, Name string
	Class                   Class
	Reason                  Reason
	// Age is how long the object has been stopped or idle, zero when that is not known.
	Age time.Duration
	// Size is the bytes the object holds that removing it frees, -1 when not known.
	Size int64
}

// Plan is what a janitor run would do at one moment.
type Plan struct {
	// Items are the objects it would remove, NotRemoved the ones it looked at and left.
	Items, NotRemoved []Item
	// WithheldNodes are the nodes that did not report, whose objects are in neither list.
	WithheldNodes []string
	// NetworksWithheld is true when no network was judged because a node that could hold an
	// attachment did not report.
	NetworksWithheld bool
	// NodesChecked is how many nodes reported.
	NodesChecked int
	ReadAt       time.Time
}

// Summary counts the items by kind and adds up the space the removable items hold. known is false
// when none of them has a known size, so a caller says the estimate is unavailable and not zero.
func (p Plan) Summary() (containers, networks, images int, estBytes int64, known bool) {
	for _, it := range p.Items {
		switch it.Type {
		case KindContainer:
			containers++
		case KindNetwork:
			networks++
		case KindImage:
			images++
		}
		if it.Size >= 0 {
			estBytes += it.Size
			known = true
		}
	}
	return containers, networks, images, estBytes, known
}

// Input is everything Build decides on. Ends are the inspect facts of the containers Candidates
// named, by container ID. Networks is the orphan check's judgement of every network, nil when
// networks were not read: Build then lists none and withholds none.
type Input struct {
	Snapshot Snapshot
	Ends     map[string]ContainerEnd
	Networks *NetworkReport
}

// Build says what the janitor would remove at now, and why it leaves the rest. It is pure: the
// same input, policy and time give the same plan, in the same order.
//
// The rules for a swarm task container, in the order ClassifyContainer tries them, are the safety
// of the check: a container is left out for every reason that could make removing it wrong before
// it is ever judged removable.
//
//  1. not exited: left out of the plan altogether
//  2. carries tierplan.keep=true: protected (kept_label)
//  3. its task is one the swarm wants running: left out
//  4. its swarm labels cannot place it: cant_tell (task_labels_missing)
//  5. among the newest KeepN of its service slot, or of its node for a global service: left out
//  6. not inspected: cant_tell (inspect_capped)
//  7. finished in the future: cant_tell (clock_skew)
//  8. finished less than the grace period ago: grace (at exactly the grace period it is removable)
//  9. its service is gone, and a manager said so: removable (task_service_gone)
//  10. otherwise: removable (task_beyond_keep)
//
// A network is judged by ClassifyNetwork from the orphan check's report.
//
// The nodes that did not report are named in the plan and none of their objects is judged.
func Build(in Input, p Policy, now time.Time) Plan {
	s := in.Snapshot
	plan := Plan{
		Items: []Item{}, NotRemoved: []Item{}, ReadAt: s.ReadAt, NodesChecked: len(s.Nodes),
		WithheldNodes: slices.Clone(s.NotReporting),
	}
	if plan.WithheldNodes == nil {
		plan.WithheldNodes = []string{}
	}
	slices.Sort(plan.WithheldNodes)

	for _, ci := range taskContainers(s, p) {
		end, inspected := in.Ends[ci.Container.ID]
		ci.End, ci.Inspected = end, inspected
		item, listed := ClassifyContainer(ci, now)
		switch {
		case !listed:
		case item.Class == ClassRemovable:
			plan.Items = append(plan.Items, item)
		default:
			plan.NotRemoved = append(plan.NotRemoved, item)
		}
	}
	if in.Networks != nil {
		addNetworks(&plan, *in.Networks, p, now)
	}
	sortItems(plan.Items)
	sortItems(plan.NotRemoved)
	return plan
}

// addNetworks adds the networks of the orphan check to the plan. A report that is not complete
// withholds every network: the plan says so and names the nodes the check could not read, and no
// network in it is removable.
func addNetworks(plan *Plan, rep NetworkReport, p Policy, now time.Time) {
	for _, row := range rep.Rows {
		item, listed := ClassifyNetwork(row, rep.Complete, p, now)
		switch {
		case !listed:
		case item.Class == ClassRemovable:
			plan.Items = append(plan.Items, item)
		default:
			plan.NotRemoved = append(plan.NotRemoved, item)
		}
	}
	if rep.Complete {
		return
	}
	plan.NetworksWithheld = true
	plan.WithheldNodes = append(plan.WithheldNodes, rep.WithheldNodes...)
	slices.Sort(plan.WithheldNodes)
	plan.WithheldNodes = slices.Compact(plan.WithheldNodes)
}

// sortItems puts items in the order a plan promises: kind (containers, networks, images), then
// node name, name and ID.
func sortItems(items []Item) {
	slices.SortStableFunc(items, func(a, b Item) int {
		return cmp.Or(
			cmp.Compare(kindRank(a.Type), kindRank(b.Type)),
			strings.Compare(a.Node, b.Node),
			strings.Compare(a.Name, b.Name),
			strings.Compare(a.Ref, b.Ref),
		)
	})
}

// Candidates names the containers that need an inspect before the planner can judge them: the
// exited swarm task containers beyond the newest KeepN of their group, or of a service that is
// gone, that nothing protects. Only these are inspected, and at most MaxInspectsPerRun of them,
// the oldest first; a candidate over the cap is cant_tell in the plan.
func Candidates(s Snapshot, p Policy) []ContainerRef {
	var cands []ContainerInput
	for _, ci := range taskContainers(s, p) {
		if needsInspect(ci) {
			cands = append(cands, ci)
		}
	}
	slices.SortStableFunc(cands, func(a, b ContainerInput) int {
		return cmp.Or(a.Container.Created.Compare(b.Container.Created), strings.Compare(a.Container.ID, b.Container.ID))
	})
	if len(cands) > MaxInspectsPerRun {
		cands = cands[:MaxInspectsPerRun]
	}
	refs := make([]ContainerRef, 0, len(cands))
	for _, ci := range cands {
		refs = append(refs, ContainerRef{ID: ci.Container.ID, NodeID: ci.Container.NodeID})
	}
	return refs
}
