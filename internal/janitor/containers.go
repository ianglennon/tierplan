// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// stateExited is the engine's state of a container that ran and stopped. It is the only state
// the janitor removes a container in: a container that is created, running, paused, restarting,
// removing or dead is somebody's, or on its way to being somebody's.
const stateExited = "exited"

// needsInspect reports whether the planner needs the inspect facts of a container to judge it:
// it is exited, nothing protects it or wants it running, its labels place it, and it is beyond
// the newest KeepN of its group or its service is gone.
func needsInspect(ci ContainerInput) bool {
	c := ci.Container
	switch {
	case c.State != stateExited, c.Labels[stacks.LabelKeep] == "true", ci.DesiredRunning, ci.Group == "":
		return false
	}
	return ci.ServiceGone || ci.beyondKeep()
}

// ClassifyContainer says what the janitor makes of one swarm task container at now. listed is
// false when the container is simply not the janitor's business (it is not exited, its task is
// wanted running, or it is among the newest of its group). The first rule that matches wins; the
// order is documented on Build.
func ClassifyContainer(in ContainerInput, now time.Time) (item Item, listed bool) {
	c := in.Container
	item = Item{
		Type: KindContainer, NodeID: c.NodeID, Node: c.Node, Ref: c.ID, Name: strings.TrimPrefix(c.Name, "/"),
		Size: -1,
		Reason: Reason{
			Service: c.Labels[labelSwarmServiceName], Slot: in.Slot, Stack: c.Labels[labelStackNamespace], Rank: in.Rank,
		},
	}
	if in.Slot == "" && in.Group != "" {
		item.Reason.Node = c.Node
	}
	switch {
	case c.State != stateExited:
		return Item{}, false
	case c.Labels[stacks.LabelKeep] == "true":
		item.Class, item.Reason.Code = ClassProtected, ReasonKeptLabel
		return item, true
	case in.DesiredRunning:
		return Item{}, false
	case in.Group == "":
		item.Class, item.Reason.Code = ClassCantTell, ReasonTaskLabelsMissing
		return item, true
	case !in.ServiceGone && !in.beyondKeep():
		return Item{}, false
	case !in.Inspected:
		item.Class, item.Reason.Code = ClassCantTell, ReasonInspectCapped
		return item, true
	}

	item.Size = in.End.SizeRw
	finished := in.End.FinishedAt
	if finished.IsZero() {
		finished = c.Created
	}
	if finished.After(now.Add(ClockSkewTolerance)) {
		item.Class, item.Reason.Code = ClassCantTell, ReasonClockSkew
		return item, true
	}
	age := max(now.Sub(finished), 0)
	item.Age, item.Reason.Age = age, age
	if remaining := finished.Add(in.Grace).Sub(now); remaining > 0 {
		item.Class, item.Reason.Code, item.Reason.Remaining = ClassGrace, ReasonYoungerThanGrace, remaining
		return item, true
	}
	item.Class = ClassRemovable
	item.Reason.Code = ReasonTaskBeyondKeep
	if in.ServiceGone {
		item.Reason.Code = ReasonTaskServiceGone
	}
	return item, true
}
