// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"cmp"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The reasons a move of the virtual IP is recorded with (D-23). The list is closed: only these strings ever reach the
// moves table, whatever a node's log says.
const (
	// VIPMoveHolderFailed: the previous holder's last line was MASTER and it fell silent, its task is gone, or its helper
	// started again without having said that it stopped.
	VIPMoveHolderFailed = "holder_failed"
	// VIPMoveProbeFailing: the previous holder left MASTER for FAULT with the reason probe_failing.
	VIPMoveProbeFailing = "probe_failing"
	// VIPMoveDrainHandoff: the server handed the address over before draining the holder's node.
	VIPMoveDrainHandoff = "drain_handoff"
	// VIPMoveDisabled: the virtual IP was being switched off.
	VIPMoveDisabled = "disabled"
	// VIPMoveUnknown: none of the above explains the move.
	VIPMoveUnknown = "unknown"
)

// vipHandoffWindow is how long a handoff mark explains a move: a move within it of the mark for the holder's node is
// a drain_handoff. The comparison is against the event's own time, so a move read long after it happened is judged by
// when it happened.
const vipHandoffWindow = 15 * time.Second

// vipSettleWindow is how long a node must be the only one claiming the address before it is the settled holder. With two or
// more backups of equal priority every election has more than one claimant: all of them go MASTER within a moment of each
// other, and the ones with the lower addresses give up on the winner's next advert, which comes within a second. The window
// is that second and as much again for the lines of two nodes being read at different instants.
const vipSettleWindow = 2 * time.Second

// vipNode is what the server knows of one node's keepalived helper, from the helper's own lines (D-21).
type vipNode struct {
	NodeID   string
	Hostname string
	// State is the last VRRP state the node reported: MASTER, BACKUP, FAULT or STOP; "" before any, and again after the
	// helper's start line, which says the earlier run is over.
	State string
	// Reason is the fault shown for the node: the most telling of Faults (shownFault), "" when none stands.
	Reason string
	// Faults are the reasons of the faults standing now. A state that is not FAULT clears them.
	Faults map[string]bool
	// Foreign is true while another router uses the environment's router ID; ForeignSender and ForeignCounter say
	// who and which authentication counter rose (A-04).
	Foreign        bool
	ForeignSender  string
	ForeignCounter string
	// ClockSkew is true while two routers' clocks disagree by more than the authentication window (A-04).
	ClockSkew bool
	// TaskStarted is when the helper last started; LastEvent when it last wrote any line.
	TaskStarted time.Time
	LastEvent   time.Time
	// HeldAt is when the node last reported MASTER; the zero time when it never has.
	HeldAt time.Time
	// Lost is why the node last left MASTER: VIPMoveProbeFailing, VIPMoveHolderFailed when its helper died without a word,
	// or VIPMoveUnknown. It is kept when the node leaves, so nothing the node does afterwards changes it, and is "" while the
	// node holds the address.
	Lost string
	// Superseded is true for a MASTER that could not be heard when another node took the address: it can no longer say that
	// it does not hold it, so it is not counted as a holder until it writes a line again.
	Superseded bool
	// LastHeard is the last pass that read the node's engine without a failure.
	LastHeard time.Time
	// Gone is true while the last pass read the node's engine successfully and found no running keepalived container
	// there: positive evidence that the helper is dead, whatever its last line said. The reader sets it at the end of every
	// pass. A MASTER that is gone is not a holder, since a helper killed without a STOP line left MASTER as its last word,
	// but State is kept as it was, so the node holds the address again the moment a running container is read. A node that
	// could not be asked (its agent is not connected, the read failed or timed out, its node ID is not known) is not gone:
	// not being able to ask says nothing about whether it holds the address.
	Gone bool
}

// vipEvent is one parsed line with the instant the engine stamped it with and the node it came from.
type vipEvent struct {
	At               time.Time
	NodeID, Hostname string
	ingressbundle.VIPEvent
}

// vipMarks are the facts the server knows that a node's log cannot say.
type vipMarks struct {
	// Handoff is, by node, when the server asked that node to hand the address over.
	Handoff map[string]time.Time
	// Disabling is true while the virtual IP is being switched off.
	Disabling bool
	// Silent is the set of nodes the pass could not hear: the agent is not connected, the read failed, or no keepalived
	// task is running there any more.
	Silent map[string]bool
	// Now is the instant of the pass, by the server's clock.
	Now time.Time
}

// vipClaim is a node that is the only one claiming the address, before it is the settled holder.
type vipClaim struct {
	NodeID string
	// At is when the node reported MASTER, which is the instant of the move if the claim settles.
	At time.Time
	// Since is when it became the only claimant, by the lines' own time; Seen is the pass that first saw that, by the
	// server's clock. Either one settles the claim once vipSettleWindow has passed.
	Since, Seen time.Time
}

// vipEnvState is what is known of one environment's virtual IP: every node, the node that held the address last and
// when the address last lost its holder.
type vipEnvState struct {
	Nodes map[string]vipNode
	// LastHolder is the settled holder: the node that last was the only claimant of the address for vipSettleWindow, ""
	// before any was. It stays when the holder leaves, so the next one to settle is a move from it, and is never a move from
	// a node that is not there.
	LastHolder string
	// Claim is the open claim, the zero value when there is none.
	Claim vipClaim
	// HolderlessSince is when the last holder left; the zero time while one holds the address, and while it is not known.
	HolderlessSince time.Time
}

// clone copies the state, so detectMoves never changes what it was given.
func (st vipEnvState) clone() vipEnvState {
	out := st
	out.Nodes = make(map[string]vipNode, len(st.Nodes))
	for id, n := range st.Nodes {
		n.Faults = cloneFaults(n.Faults)
		out.Nodes[id] = n
	}
	return out
}

func cloneFaults(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// claimOpen reports whether a node is claiming the address and the claim is not settled yet.
func (st vipEnvState) claimOpen() bool { return st.Claim.NodeID != "" }

// shownFault is the fault to show of those standing: one that keeps the node out whatever its proxy does comes before
// probe_failing, because the address_in_use check works by making the probe fail, so that FAULT line always follows it.
func shownFault(faults map[string]bool) string {
	for _, f := range []string{
		ingressbundle.VIPReasonNoInterface, ingressbundle.VIPReasonAmbiguousInterface,
		ingressbundle.VIPReasonAddressInUse, ingressbundle.VIPReasonProbeFailing,
	} {
		if faults[f] {
			return f
		}
	}
	return ""
}

// holders returns every node whose last state is MASTER and that is neither gone nor superseded, sorted by host name then
// ID. One is the healthy case; none and several are what the status reports as a problem.
func holders(nodes map[string]vipNode) []vipNode {
	var out []vipNode
	for _, n := range nodes {
		if n.State == ingressbundle.VIPStateMaster && !n.Gone && !n.Superseded {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, compareVIPNodes)
	return out
}

func compareVIPNodes(a, b vipNode) int {
	return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.NodeID, b.NodeID))
}

// claimants is the nodes that claim the address as far as the pass can tell: the holders that can be heard, and when there
// is none, the ones that cannot.
func claimants(nodes map[string]vipNode, silent map[string]bool) []vipNode {
	all := holders(nodes)
	heard := slices.DeleteFunc(slices.Clone(all), func(n vipNode) bool { return silent[n.NodeID] })
	if len(heard) > 0 {
		return heard
	}
	return all
}

// track opens, keeps or drops the claim after something changed at the instant. Only a sole claimant that is not the settled
// holder has one: while two nodes claim the address the election is not over, and a node that gave it up lost.
func (st *vipEnvState) track(at time.Time, marks vipMarks) {
	now := claimants(st.Nodes, marks.Silent)
	if len(now) != 1 || now[0].NodeID == st.LastHolder {
		st.Claim = vipClaim{}
		return
	}
	if n := now[0]; st.Claim.NodeID != n.NodeID || !st.Claim.At.Equal(n.HeldAt) {
		st.Claim = vipClaim{NodeID: n.NodeID, At: n.HeldAt, Since: at, Seen: marks.Now}
	}
}

// commit makes the claimant the settled holder and returns the move, which the very first holder is not.
func (st *vipEnvState) commit(marks vipMarks) (ingressstore.VIPMove, bool) {
	c := st.Claim
	from, had := st.Nodes[st.LastHolder]
	st.Claim, st.LastHolder = vipClaim{}, c.NodeID
	if !had {
		return ingressstore.VIPMove{}, false
	}
	return ingressstore.VIPMove{
		At: c.At, FromNodeID: from.NodeID, FromNodeName: from.Hostname,
		ToNodeID: c.NodeID, ToNodeName: st.Nodes[c.NodeID].Hostname, Reason: moveReason(from, c.At, marks),
	}, true
}

// detectMoves applies the events of one pass to the state in time order, ties by node ID then sequence number, and returns
// the new state and the moves of the address, oldest first (D-23). It changes nothing it is given.
//
// A move is the settled holder changing. A node settles once it has been the only claimant for vipSettleWindow, by the lines'
// own time when a later line shows it, and by the server's clock at the end of a pass otherwise, so a move is returned by a
// pass after the one that read its MASTER line. The claims an election makes and drops are therefore never moves, and one
// failover is one move whichever of the backups spoke first. The settled holder is remembered after it leaves, so STOP on A
// and then MASTER on B is one move from A to B, and a node that goes MASTER, BACKUP, MASTER with no other holder between
// records none. The very first holder is no move, since there is no node it came from. Two lines with the same node, sequence
// number and instant are one line read twice.
func detectMoves(prev vipEnvState, events []vipEvent, marks vipMarks) (vipEnvState, []ingressstore.VIPMove) {
	st := prev.clone()
	// Whether a node is gone is the reader's to say after the events are applied; the events themselves are applied to
	// what the nodes last said.
	for id, n := range st.Nodes {
		n.Gone = false
		st.Nodes[id] = n
	}
	ordered := slices.Clone(events)
	slices.SortStableFunc(ordered, func(a, b vipEvent) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.NodeID, b.NodeID), cmp.Compare(a.Seq, b.Seq))
	})
	type key struct {
		node string
		seq  uint64
		at   time.Time
	}
	seen := make(map[key]bool, len(ordered))
	var moves []ingressstore.VIPMove
	settle := func() {
		if m, ok := st.commit(marks); ok {
			moves = append(moves, m)
		}
	}
	for _, ev := range ordered {
		k := key{ev.NodeID, ev.Seq, ev.At}
		if seen[k] {
			continue
		}
		seen[k] = true
		if st.claimOpen() && ev.At.Sub(st.Claim.Since) >= vipSettleWindow {
			settle()
		}
		st.apply(ev, marks)
		st.track(ev.At, marks)
	}
	// A node that fell silent since the last pass changes who claims the address without a line of its own.
	if c := claimants(st.Nodes, marks.Silent); len(c) == 1 {
		st.track(c[0].HeldAt, marks)
	} else {
		st.track(time.Time{}, marks)
	}
	// While the virtual IP is being switched off the reader may not come back to the environment, so nothing is held back.
	if st.claimOpen() && (marks.Disabling || marks.Now.Sub(st.Claim.Seen) >= vipSettleWindow) {
		settle()
	}
	return st, moves
}

// apply applies one event to its node and to when the address lost its holder.
func (st *vipEnvState) apply(ev vipEvent, marks vipMarks) {
	hadHolder := len(holders(st.Nodes)) > 0
	n, ok := st.Nodes[ev.NodeID]
	if !ok {
		n = vipNode{NodeID: ev.NodeID, Faults: map[string]bool{}}
	}
	if ev.Hostname != "" {
		n.Hostname = ev.Hostname
	}
	n.LastEvent = ev.At
	wasMaster := n.State == ingressbundle.VIPStateMaster
	switch ev.Event {
	case ingressbundle.VIPEventStart:
		// The helper started: whatever the run before it said is over. A run that ended without saying so while it held
		// the address was killed.
		if wasMaster {
			n.Lost = VIPMoveHolderFailed
		}
		n.TaskStarted = ev.At
		n.State, n.Superseded = "", false
		n.Faults = map[string]bool{}
		n.Foreign, n.ForeignSender, n.ForeignCounter, n.ClockSkew = false, "", "", false
	case ingressbundle.VIPEventState:
		n.State, n.Superseded = ev.State, false
		if ev.State == ingressbundle.VIPStateFault {
			if ev.Reason != "" {
				n.Faults[ev.Reason] = true
			}
		} else {
			n.Faults = map[string]bool{}
		}
		switch {
		case ev.State == ingressbundle.VIPStateMaster:
			n.HeldAt, n.Lost = ev.At, ""
			// A node that cannot be heard and was MASTER can no longer say it is not, so the new holder takes its
			// place. A node that can be heard and still says MASTER stays one: two holders are reported as they are.
			for id, other := range st.Nodes {
				if id != ev.NodeID && other.State == ingressbundle.VIPStateMaster && marks.Silent[id] {
					other.Superseded = true
					st.Nodes[id] = other
				}
			}
		case wasMaster && n.Faults[ingressbundle.VIPReasonProbeFailing]:
			n.Lost = VIPMoveProbeFailing
		case wasMaster:
			n.Lost = VIPMoveUnknown
		}
	case ingressbundle.VIPEventFault:
		// A fault line without Active says the fault is over (no_interface, ambiguous_interface and address_in_use are
		// cleared this way), so it takes the fault away and never records one.
		if ev.Active {
			n.Faults[ev.Reason] = true
		} else {
			delete(n.Faults, ev.Reason)
		}
	case ingressbundle.VIPEventForeignRouter:
		n.Foreign = ev.Active
		n.ForeignSender, n.ForeignCounter = "", ""
		if ev.Active {
			n.ForeignSender, n.ForeignCounter = ev.Addr, ev.Counter
		}
	case ingressbundle.VIPEventClockSkew:
		n.ClockSkew = ev.Active
	}
	n.Reason = shownFault(n.Faults)
	st.Nodes[ev.NodeID] = n
	switch has := len(holders(st.Nodes)) > 0; {
	case has:
		st.HolderlessSince = time.Time{}
	case hadHolder:
		st.HolderlessSince = ev.At
	}
}

// moveReason names why the address left from, the settled holder before the move, at the instant of the move. The checks run
// in the order the list of reasons is written for: a switch-off explains everything while it runs, a handoff the server asked
// for explains the move next, then what the old holder itself reported as it left, then what is only inferred from its silence.
func moveReason(from vipNode, at time.Time, marks vipMarks) string {
	if marks.Disabling {
		return VIPMoveDisabled
	}
	if m, ok := marks.Handoff[from.NodeID]; ok && at.Sub(m).Abs() <= vipHandoffWindow {
		return VIPMoveDrainHandoff
	}
	if from.State == ingressbundle.VIPStateMaster {
		// It never said that it left, and it is not a claimant: it was silent when the new holder took the address.
		return VIPMoveHolderFailed
	}
	if from.Lost != "" {
		return from.Lost
	}
	return VIPMoveUnknown
}
