// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"net/netip"
	"slices"
	"time"
)

// The IDs of the virtual IP's bell items (Phase 26, A-07, UI-SPEC S4). They are environment warnings like the rest: derived
// from live state, never stored, and gone with their cause.
const (
	// WarnVIPNoHolder is a virtual IP that no node holds for more than VIPNoHolderAfter. It is the one error of them.
	WarnVIPNoHolder = "vip_no_holder"
	// WarnVIPMultipleHolders is a virtual IP that more than one node reports holding.
	WarnVIPMultipleHolders = "vip_multiple_holders"
	// WarnVIPHolderUnheard is a virtual IP whose only holder TierPlan has not heard from for more than VIPUnheardAfter while it
	// hears another node: the holder is still shown, since not hearing it is no proof that it let go, but nothing confirms it.
	WarnVIPHolderUnheard = "vip_holder_unheard"
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
	// VIPUnheardAfter is how long the only holder may go unheard before the bell says so. It is longer than an agent takes to
	// come back after an update or a blip.
	VIPUnheardAfter = time.Minute
	// VIPMovedWindow is how far back a recorded move still raises a bell item (A-07).
	VIPMovedWindow = time.Hour
)

// VIPHolderFact is one node that reports holding the address and when TierPlan last heard from it.
type VIPHolderFact struct {
	NodeID, Host string
	LastHeard    time.Time
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
	// LastHeard is when TierPlan last heard from any node of the virtual IP, the zero time when it never has.
	LastHeard time.Time
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
	if len(f.Holders) > 1 {
		w := base
		w.ID, w.Severity, w.Count = WarnVIPMultipleHolders, SeverityWarning, len(f.Holders)
		w.VIP = &VIPWarningFacts{Address: f.Address}
		out = append(out, w)
	}
	if w, ok := vipHolderUnheard(base, f, now); ok {
		out = append(out, w)
	}
	if w, ok := vipMoved(base, f, now); ok {
		out = append(out, w)
	}
	if f.ForeignRouter {
		w := base
		w.ID, w.Severity = WarnVIPForeignRouter, SeverityWarning
		w.VIP = &VIPWarningFacts{Address: f.Address, RouterID: f.RouterID, Sender: validSender(f.ForeignSender)}
		out = append(out, w)
	}
	out = append(out, vipClockItems(base, f, agents)...)
	if len(f.PrivilegedLost) > 0 {
		w := base
		w.ID, w.Severity, w.Count = WarnVIPPrivilegedLost, SeverityWarning, len(f.PrivilegedLost)
		if len(f.PrivilegedLost) == 1 {
			w.Node = cleanText(f.PrivilegedLost[0], maxNodeLen)
		}
		w.VIP = &VIPWarningFacts{Address: f.Address}
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

// vipHolderUnheard is the item for an address whose only holder cannot be heard: its host may be gone with no other node able
// to take the address. While no node is heard at all there is nothing to compare with and the environment's own items say it.
// Another node that takes the address ends it, since the silent one is then no holder.
func vipHolderUnheard(base EnvWarning, f VIPFacts, now time.Time) (EnvWarning, bool) {
	if len(f.Holders) != 1 {
		return EnvWarning{}, false
	}
	h := f.Holders[0]
	if h.LastHeard.IsZero() || now.Sub(h.LastHeard) <= VIPUnheardAfter || now.Sub(f.LastHeard) > VIPUnheardAfter {
		return EnvWarning{}, false
	}
	w := base
	w.ID, w.Severity, w.Since, w.Node = WarnVIPHolderUnheard, SeverityWarning, h.LastHeard, cleanText(h.Host, maxNodeLen)
	w.VIP = &VIPWarningFacts{Address: f.Address}
	return w, true
}

// vipMoveReasons are the reasons of a move that raise a bell item. A move with reason disabled raises none: the virtual IP is
// gone and there is nothing to act on (U-06). Any other value reads as unknown, so a reason that is not on the list never
// reaches the bell.
var vipMoveReasons = []string{"holder_failed", "probe_failing", "drain_handoff", "unknown"}

// vipMoved is the one item for the moves of the last VIPMovedWindow, with the count and the latest move's facts. One item per
// environment however many moves there were, so a flapping address cannot flood the bell (T-26-49).
func vipMoved(base EnvWarning, f VIPFacts, now time.Time) (EnvWarning, bool) {
	var latest VIPMoveFact
	count := 0
	for _, m := range f.Moves {
		if m.Reason == "disabled" || now.Sub(m.At) > VIPMovedWindow {
			continue
		}
		if count == 0 || !m.At.Before(latest.At) {
			latest = m
		}
		count++
	}
	if count == 0 {
		return EnvWarning{}, false
	}
	reason := latest.Reason
	if !slices.Contains(vipMoveReasons, reason) {
		reason = "unknown"
	}
	w := base
	w.ID, w.Severity, w.Count, w.Since = WarnVIPMoved, SeverityWarning, count, latest.At
	w.VIP = &VIPWarningFacts{
		Address: f.Address, MoveReason: reason,
		From: cleanText(latest.From, maxNodeLen), To: cleanText(latest.To, maxNodeLen),
	}
	return w, true
}

// validSender is a foreign router's address when it parses as one, and "" otherwise: the text comes from a network log line.
func validSender(s string) string {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return ""
	}
	return a.String()
}

// vipClockItems are the clock items, one per node: a node whose helper reports clock skew, and a node whose agent's clock is
// at or above half the authentication window out of step with the server's (two clocks that far apart on opposite sides are
// a whole window apart). A node both name is one item, with the agent's offset.
func vipClockItems(base EnvWarning, f VIPFacts, agents []AgentState) []EnvWarning {
	window := int(f.AuthWindow / time.Second)
	items := map[string]EnvWarning{}
	item := func(node string) EnvWarning {
		w, ok := items[node]
		if !ok {
			w = base
			w.ID, w.Severity, w.Node = WarnVIPClockSkew, SeverityWarning, node
			w.VIP = &VIPWarningFacts{Address: f.Address, WindowSeconds: window}
		}
		return w
	}
	for _, c := range f.ClockNodes {
		node := cleanText(c.Host, maxNodeLen)
		items[node] = item(node)
	}
	if f.AuthWindow > 0 {
		for _, a := range agents {
			if a.Revoked || abs(a.ClockOffset) < f.AuthWindow/2 {
				continue
			}
			node := cleanText(a.Node, maxNodeLen)
			w := item(node)
			w.AgentID, w.Offset, w.Standby = a.ID, a.ClockOffset, a.Standby
			items[node] = w
		}
	}
	out := make([]EnvWarning, 0, len(items))
	for _, w := range items {
		out = append(out, w)
	}
	return out
}
