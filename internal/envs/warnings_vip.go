// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"time"
)

// The IDs of the virtual IP's bell items (Phase 26, A-07, UI-SPEC S4). They are environment warnings like the rest: derived
// from live state, never stored, and gone with their cause.
const (
	// WarnVIPNoHolder is a virtual IP that no node holds for more than VIPNoHolderAfter. It is the one error of the six.
	WarnVIPNoHolder = "vip_no_holder"
	// WarnVIPMultipleHolders is a virtual IP that more than one node reports holding.
	WarnVIPMultipleHolders = "vip_multiple_holders"
	// WarnVIPMoved is a virtual IP that moved in the last VIPMovedWindow, several moves collapsed into one item with a count.
	WarnVIPMoved = "vip_moved"
	// WarnVIPForeignRouter is another router sending adverts with this environment's router ID that fail authentication.
	WarnVIPForeignRouter = "vip_foreign_router"
	// WarnVIPClockSkew is a node whose clock is far enough out of step to make the virtual IP's authentication refuse adverts.
	WarnVIPClockSkew = "vip_clock_skew"
	// WarnVIPPrivilegedLost is a manager whose agent allowed privileged services and now refuses.
	WarnVIPPrivilegedLost = "vip_privileged_lost"
)

const (
	// VIPNoHolderAfter is how long the address may be held by no node before the bell says so (A-07).
	VIPNoHolderAfter = 10 * time.Second
	// VIPFaultGrace is how long after its task started a node may sit in FAULT without the bell saying anything: the
	// first start of a helper is a FAULT until its proxy answers (A-07).
	VIPFaultGrace = 15 * time.Second
	// VIPMovedWindow is how far back a recorded move still raises a bell item (A-07).
	VIPMovedWindow = time.Hour
)

// VIPHolderFact is one node that reports holding the address.
type VIPHolderFact struct {
	NodeID, Host string
}

// VIPMoveFact is one recorded move of the address. An empty From or To means no node.
type VIPMoveFact struct {
	At       time.Time
	From, To string
	Reason   string
}

// VIPClockFact is one node whose helper reports that its clock disagrees with a peer's.
type VIPClockFact struct {
	NodeID, Host string
}

// VIPFaultFact is one node in FAULT and when its keepalived task started, the zero time when that is not known.
type VIPFaultFact struct {
	Host        string
	TaskStarted time.Time
}

// VIPFacts is what the bell needs to know of one environment's virtual IP. It is defined here and filled by
// ingressdeploy, which reads the reader's memory, the move history and the manager table; this package never imports it. It
// holds no key and no secret (PROH-26-03): every field is a closed-vocabulary value, a validated address, a number, a time or
// a host name the bell cleans.
type VIPFacts struct {
	// Enabled is true while the virtual IP is on; Removing is true while it is being switched off. Either makes the
	// environment quiet.
	Enabled, Removing bool
	// Address is the virtual address with its prefix length; RouterID the VRRP router ID.
	Address  string
	RouterID int
	// Holders are the nodes reporting MASTER.
	Holders []VIPHolderFact
	// NoHolderSince is when the address last lost its holder; the zero time while a node holds it or when that is not known.
	NoHolderSince time.Time
	// Faulting are the nodes in FAULT now.
	Faulting []VIPFaultFact
	// Moves are the recorded moves of the last VIPMovedWindow, in any order.
	Moves []VIPMoveFact
	// ForeignRouter is true while another router uses the router ID; ForeignSender is its address, "" when none was
	// validated.
	ForeignRouter bool
	ForeignSender string
	// ClockNodes are the nodes whose helper reports clock skew. AuthWindow is the time window of the virtual IP's
	// authentication.
	ClockNodes []VIPClockFact
	AuthWindow time.Duration
	// PrivilegedLost are the host names of the managers whose connected agent refuses privileged services.
	PrivilegedLost []string
}

// VIPWarningFacts are the facts of a virtual IP item that EnvWarning has no field for. Every value renders as text.
type VIPWarningFacts struct {
	Address  string
	RouterID int
	// From and To are the host names of the latest move; MoveReason is its reason.
	From, To, MoveReason string
	// Sender is the validated address of a foreign router, "" when none was.
	Sender string
	// WindowSeconds is the authentication window, for a clock item.
	WindowSeconds int
}

// VIPWarnings derives the bell items of one environment's virtual IP from its facts, the environment's agents (read for
// their clock offsets) and the clock. It is pure. An environment without a virtual IP, or whose virtual IP is being switched
// off, says nothing. The caller passes facts only for a caller who manages the environment's ingress.
func VIPWarnings(envID, envName string, f VIPFacts, agents []AgentState, now time.Time) []EnvWarning {
	if !f.Enabled || f.Removing {
		return nil
	}
	base := EnvWarning{EnvID: envID, EnvName: cleanText(envName, maxNodeLen)}
	var out []EnvWarning
	if w, ok := vipNoHolder(base, f, now); ok {
		out = append(out, w)
	}
	SortWarnings(out)
	return out
}

// vipNoHolder is the item for an address nobody holds. A helper in its first VIPFaultGrace is still starting, which is
// normal, so nothing is said while any node is.
func vipNoHolder(base EnvWarning, f VIPFacts, now time.Time) (EnvWarning, bool) {
	if len(f.Holders) > 0 || f.NoHolderSince.IsZero() || now.Sub(f.NoHolderSince) <= VIPNoHolderAfter {
		return EnvWarning{}, false
	}
	for _, n := range f.Faulting {
		if !n.TaskStarted.IsZero() && now.Sub(n.TaskStarted) < VIPFaultGrace {
			return EnvWarning{}, false
		}
	}
	w := base
	w.ID, w.Severity, w.Since = WarnVIPNoHolder, SeverityError, f.NoHolderSince
	w.VIP = &VIPWarningFacts{Address: f.Address}
	return w, true
}
