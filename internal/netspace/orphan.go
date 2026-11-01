// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package netspace

import "time"

// GracePeriod is how long a network must have been idle before it is listed as orphaned or
// unused (D-08). It is measured from the later of the network's creation and the last time
// Coaming saw it in use, so a network that was busy until an hour ago is not listed today.
const GracePeriod = 24 * time.Hour

// Class is what the orphan check made of one network.
type Class string

// The classes, in the order Classify tries them. Only ClassOrphaned, ClassUnused and ClassCantTell
// are shown to a person; the rest are why a network is left out.
const (
	// ClassExcluded: an engine or swarm network (bridge, host, none, ingress, docker_gwbridge,
	// config-only networks and the networks they are configured from), or one a Coaming stack
	// declares external.
	ClassExcluded Class = "excluded"
	// ClassInUse: something attaches to the network now.
	ClassInUse Class = "in_use"
	// ClassKept: a person asked never to list it (the coaming.keep=true label or a keep row).
	ClassKept Class = "kept"
	// ClassSkipped: the stack that owns it holds an operation on the environment, and a deploy
	// makes networks before it makes the services that use them.
	ClassSkipped Class = "skipped"
	// ClassGrace: idle, but not yet for the whole grace period.
	ClassGrace Class = "grace"
	// ClassCantTell: Coaming cannot be sure, because a node that could hold an attachment did not
	// report, no manager answered, or a stack that may declare the network external did not load.
	ClassCantTell Class = "cant_tell"
	// ClassOwned: a stack (or compose project) that still exists owns it.
	ClassOwned Class = "owned"
	// ClassOrphaned: it carries a stack or project label and that stack is gone.
	ClassOrphaned Class = "orphaned"
	// ClassUnused: no stack or project ever labelled it, and nothing uses it.
	ClassUnused Class = "unused"
)

// OrphanInput is everything Classify decides on. The server fills it from the engines, the
// store and its stacks; nothing here is a Docker type, so the order of the rules is a table test.
type OrphanInput struct {
	// Builtin: the engine's or swarm's own network, by name or the Ingress flag, a ConfigOnly
	// network, or the target of any network's ConfigFrom.
	Builtin bool
	// InUse: something is known to attach to it (Usage.InUse).
	InUse bool
	// Known: the answer to InUse is complete (Usage.Known): every node that could hold an
	// attachment reported, and on a swarm a manager answered.
	Known bool
	// KeepLabel: the network carries coaming.keep=true. KeepRow: a keep row holds its name.
	KeepLabel, KeepRow bool
	// External: a Coaming stack of the environment declares the network external.
	External bool
	// Busy: the stack that owns the network holds the environment's operation lock.
	Busy bool
	// StackExists: the stack or project named by OwnerLabel exists, as a Coaming stack record or
	// because a container or service still carries its label (user decision 4).
	StackExists bool
	// ExternalsUnknown: a stack of the environment did not load, so what it declares external
	// is not known.
	ExternalsUnknown bool
	// OwnerLabel is the stack or compose project the network's labels name, empty for none.
	OwnerLabel string
	// Created is when the network was made, LastInUse the last time Coaming saw it in use (the
	// zero time for never).
	Created, LastInUse time.Time
}

// IdleSince is when a network's idle time starts: the later of its creation and the last time it
// was seen in use.
func IdleSince(created, lastInUse time.Time) time.Time {
	if lastInUse.After(created) {
		return lastInUse
	}
	return created
}

// Classify says what the orphan check makes of a network at now. The first rule that matches
// wins, and the order is the safety of the check: a network is left out for every reason that
// could make removing it wrong before it is ever judged, and judged only when the picture is
// complete.
//
//  1. built-in or special: excluded
//  2. in use: in use
//  3. kept by label or by row: kept
//  4. declared external by a Coaming stack: excluded
//  5. owned by the stack that holds the operation lock: skipped
//  6. idle for less than GracePeriod: grace (at exactly GracePeriod it is listed)
//  7. the picture is incomplete: cant_tell
//  8. its owner label names a stack that exists: owned
//  9. its owner label names a stack that is gone: orphaned
//  10. no owner label: unused
//
// A stack that did not load may declare any network external, so while one is unknown the
// networks that would be orphaned or unused are cant_tell instead.
func Classify(in OrphanInput, now time.Time) Class {
	switch {
	case in.Builtin:
		return ClassExcluded
	case in.InUse:
		return ClassInUse
	case in.KeepLabel || in.KeepRow:
		return ClassKept
	case in.External:
		return ClassExcluded
	case in.Busy:
		return ClassSkipped
	case now.Before(IdleSince(in.Created, in.LastInUse).Add(GracePeriod)):
		return ClassGrace
	case !in.Known:
		return ClassCantTell
	case in.OwnerLabel != "" && in.StackExists:
		return ClassOwned
	case in.ExternalsUnknown:
		return ClassCantTell
	case in.OwnerLabel != "":
		return ClassOrphaned
	default:
		return ClassUnused
	}
}
