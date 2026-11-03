// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"time"

	"github.com/ianglennon/tierplan/internal/netspace"
)

// ClassifyNetwork says what the janitor makes of one network the orphan check judged, at now.
// listed is false when the network is not the janitor's business: the engine's own, one a stack
// declares external, one something uses, one a stack that still exists owns, one whose stack holds
// the environment's operation lock. The orphan check (Phase 10) decides all of those; the janitor
// adds only the policy on top, in this order:
//
//  1. kept by the tierplan.keep label or by a keep row a person made: protected
//  2. inside the grace period: grace (the policy's, which the orphan check reads too)
//  3. TierPlan cannot be sure, or the picture is incomplete: cant_tell, never removable
//  4. its stack or project is gone: removable (network_orphaned)
//  5. no stack ever owned it and nothing uses it: removable (network_unused) once the environment
//     opted in, else reported (opt_in_off_networks)
//
// complete is the report's Complete: while a node is not reporting, or no manager answered, no
// network is removable whatever its class.
func ClassifyNetwork(row NetworkRow, complete bool, p Policy, now time.Time) (item Item, listed bool) {
	item = Item{
		Type: KindNetwork, NodeID: row.NodeID, Node: row.Node, Ref: row.ID, Name: row.Name, Size: -1,
	}
	if row.FormerOwner != "" {
		if row.FormerOwnerKind == "project" {
			item.Reason.Project = row.FormerOwner
		} else {
			item.Reason.Stack = row.FormerOwner
		}
	}
	idle := func() {
		if !row.IdleSince.IsZero() {
			item.Age = max(now.Sub(row.IdleSince), 0)
			item.Reason.Age = item.Age
		}
	}
	// The orphan check judges by the environment's own grace, the policy's, so the janitor and the
	// Address space page agree on when a network is old enough (D-20). A report judged by an older
	// grace (a candidate policy previewed before it is saved) is still held by the check's class.
	grace := p.grace()

	switch row.Class {
	case netspace.ClassKept:
		item.Class, item.Reason.Code = ClassProtected, ReasonKeptRow
		if row.KeptByLabel {
			item.Reason.Code = ReasonKeptLabel
		}
		return item, true
	case netspace.ClassGrace:
		idle()
		return graceItem(item, row, grace, now), true
	case netspace.ClassCantTell:
		item.Class, item.Reason.Code = ClassCantTell, ReasonNetworksWithheld
		if complete {
			// Every node answered, so what is not known is a stack that did not load.
			item.Reason.Code = ReasonStackUnknown
		}
		return item, true
	case netspace.ClassOrphaned, netspace.ClassUnused:
	default:
		return Item{}, false
	}

	idle()
	if remaining := row.IdleSince.Add(grace).Sub(now); !row.IdleSince.IsZero() && remaining > 0 {
		return graceItem(item, row, grace, now), true
	}
	if !complete {
		item.Class, item.Reason.Code = ClassCantTell, ReasonNetworksWithheld
		return item, true
	}
	switch {
	case row.Class == netspace.ClassOrphaned:
		item.Class, item.Reason.Code = ClassRemovable, ReasonNetworkOrphaned
	case p.RemoveUnusedNetworks:
		item.Class, item.Reason.Code = ClassRemovable, ReasonNetworkUnused
	default:
		item.Class, item.Reason.Code = ClassReported, ReasonOptInOffNetworks
	}
	return item, true
}

// graceItem is item held back because the network has not been idle for the whole grace period.
func graceItem(item Item, row NetworkRow, grace time.Duration, now time.Time) Item {
	item.Class, item.Reason.Code = ClassGrace, ReasonYoungerThanGrace
	item.Reason.Remaining = max(row.IdleSince.Add(grace).Sub(now), 0)
	return item
}
