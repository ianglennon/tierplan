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

// composeOneoff is the value of com.docker.compose.oneoff on a container left by docker compose
// run. Docker labels are compared exactly: "true" is not "True".
const composeOneoff = "True"

// needsInspect reports whether the planner needs the inspect facts of a container to judge it:
// it is exited, nothing protects it or wants it running, its labels place it, and it is beyond
// the newest KeepN of its group or its service is gone. A container that is not a swarm task is
// inspected when one of the standalone rules would remove or report it once it is old enough.
func needsInspect(ci ContainerInput) bool {
	if ci.Standalone {
		call, _, _ := standaloneRule(ci)
		return call == callGrace
	}
	c := ci.Container
	switch {
	case c.State != stateExited, c.Labels[stacks.LabelKeep] == "true", ci.DesiredRunning, ci.Group == "":
		return false
	}
	return ci.ServiceGone || ci.beyondKeep()
}

// ClassifyContainer says what the janitor makes of one container at now. listed is false when
// the container is simply not the janitor's business (it is not exited, its task is wanted
// running, it is among the newest of its group, or a compose project still owns it). The first
// rule that matches wins; the order is documented on Build.
func ClassifyContainer(in ContainerInput, now time.Time) (item Item, listed bool) {
	if in.Standalone {
		return classifyStandalone(in, now)
	}
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
	}
	item, done := in.judgeEnd(item, now)
	if done {
		return item, true
	}
	item.Class = ClassRemovable
	item.Reason.Code = ReasonTaskBeyondKeep
	if in.ServiceGone {
		item.Reason.Code = ReasonTaskServiceGone
	}
	return item, true
}

// judgeEnd applies what an inspect says to an item that passed every other rule: the container
// must have been inspected, must not have finished in the future, and must have been stopped for
// the whole grace period. When it fails one of these it returns the item as cant_tell or grace
// with done true; otherwise it returns the item with its size and age set and done false, for the
// caller to give its final class.
func (in ContainerInput) judgeEnd(item Item, now time.Time) (out Item, done bool) {
	if !in.Inspected {
		item.Class, item.Reason.Code = ClassCantTell, ReasonInspectCapped
		return item, true
	}
	item.Size = in.End.SizeRw
	finished := in.End.FinishedAt
	if finished.IsZero() {
		finished = in.Container.Created
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
	return item, false
}

// standaloneCall is what the standalone rules make of a container before its end time is known.
type standaloneCall int

const (
	// callSkip: not the janitor's business.
	callSkip standaloneCall = iota
	// callFinal: the class and reason are settled without an end time (protected, cant_tell).
	callFinal
	// callGrace: the container is a candidate. Once it has been stopped for the grace period it is
	// removable or reported, as the call says; before that it is grace.
	callGrace
)

// standaloneRule says what the janitor makes of a container that is not the container of a swarm
// task (a plain docker run container, or a container of a compose project), before its end time
// is looked at. class is the final class for callFinal and the class once past grace for
// callGrace; code is the reason either way. The first rule that matches wins:
//
//  1. not exited: skip
//  2. carries tierplan.keep=true: final, protected (kept_label)
//  3. carries a compose project or a TierPlan stack ID and the stacks were not read: final,
//     cant_tell (stack_unknown), because who owns the project is not known
//  4. a TierPlan stack owns it (its ID, or a stack named as the project): only an old one-off
//     beyond the newest KeepN of its (project, service) is a candidate (compose_oneoff)
//  5. it carries the ID of a TierPlan stack that is gone: a candidate (stack_gone) unless a
//     container of the project, or of the stack, is anything but exited
//  6. a one-off beyond the newest KeepN of its (project, service): a candidate (compose_oneoff)
//  7. a compose project TierPlan does not manage: any other container of the project, in any state,
//     means the project lives and it is skipped; a lone container is reported (lone_compose_container)
//     until the environment opts in to removing stopped containers, then a candidate
//     (compose_project_gone)
//  8. any other container: reported (opt_in_off_containers) until the opt-in, then a candidate
//     (stopped_standalone)
func standaloneRule(in ContainerInput) (call standaloneCall, class Class, code ReasonCode) {
	c := in.Container
	compose := in.Project != "" || in.StackID != ""
	switch {
	case c.State != stateExited:
		return callSkip, "", ""
	case c.Labels[stacks.LabelKeep] == "true":
		return callFinal, ClassProtected, ReasonKeptLabel
	case compose && !in.StacksRead:
		return callFinal, ClassCantTell, ReasonStackUnknown
	case in.StackOwned:
		if in.Oneoff && in.beyondKeep() {
			return callGrace, ClassRemovable, ReasonComposeOneoff
		}
		return callSkip, "", ""
	case in.StackGone:
		if in.ProjectBusy {
			return callSkip, "", ""
		}
		return callGrace, ClassRemovable, ReasonStackGone
	case in.Oneoff:
		if in.beyondKeep() {
			return callGrace, ClassRemovable, ReasonComposeOneoff
		}
		return callSkip, "", ""
	case in.Project != "":
		switch {
		case in.ProjectOthers:
			return callSkip, "", ""
		case in.OptIn:
			return callGrace, ClassRemovable, ReasonComposeProjectGone
		}
		return callGrace, ClassReported, ReasonLoneComposeContainer
	case in.OptIn:
		return callGrace, ClassRemovable, ReasonStoppedStandalone
	}
	return callGrace, ClassReported, ReasonOptInOffContainers
}

// classifyStandalone is ClassifyContainer for a container that is not a swarm task. Every rule that
// ends in removable or reported waits for the whole grace period, measured from when the container
// stopped.
func classifyStandalone(in ContainerInput, now time.Time) (Item, bool) {
	call, class, code := standaloneRule(in)
	if call == callSkip {
		return Item{}, false
	}
	c := in.Container
	item := Item{
		Type: KindContainer, NodeID: c.NodeID, Node: c.Node, Ref: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Size: -1,
		Reason: Reason{Code: code, Service: c.Labels[stacks.LabelComposeService], Project: in.Project, Rank: in.Rank},
	}
	if call == callFinal {
		item.Class = class
		return item, true
	}
	item, done := in.judgeEnd(item, now)
	if done {
		return item, true
	}
	item.Class, item.Reason.Code = class, code
	return item, true
}
