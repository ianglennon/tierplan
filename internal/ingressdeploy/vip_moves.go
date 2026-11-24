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
	// VIPMoveHolderFailed: the previous holder's last line was MASTER and it fell silent, or its task is gone.
	VIPMoveHolderFailed = "holder_failed"
	// VIPMoveProbeFailing: the previous holder's last state was FAULT with the reason probe_failing.
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

// vipNode is what the server knows of one node's keepalived helper, from the helper's own lines (D-21).
type vipNode struct {
	NodeID   string
	Hostname string
	// State is the last VRRP state the node reported: MASTER, BACKUP, FAULT or STOP; "" before any, and again after the
	// helper's start line, which says the earlier run is over.
	State string
	// Reason is the reason of the last FAULT state or fault event, "" otherwise.
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
	// LastHeard is the last pass that read the node's engine without a failure.
	LastHeard time.Time
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
}

// vipEnvState is what is known of one environment's virtual IP: every node, the node that held the address last and
// when the address last lost its holder.
type vipEnvState struct {
	Nodes map[string]vipNode
	// LastHolder is the node that reported MASTER last, "" before any has. It stays when the holder leaves, so the
	// next MASTER is a move from it, and is never a move from a node that is not there.
	LastHolder string
	// HolderlessSince is when the last holder left; the zero time while one holds the address, and while it is not known.
	HolderlessSince time.Time
}

// clone copies the state, so detectMoves never changes what it was given.
func (st vipEnvState) clone() vipEnvState {
	out := vipEnvState{Nodes: make(map[string]vipNode, len(st.Nodes)), LastHolder: st.LastHolder, HolderlessSince: st.HolderlessSince}
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

// holders returns every node whose last state is MASTER, sorted by host name then ID. One is the healthy case; none and
// several are what the status reports as a problem.
func holders(nodes map[string]vipNode) []vipNode {
	var out []vipNode
	for _, n := range nodes {
		if n.State == ingressbundle.VIPStateMaster {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, compareVIPNodes)
	return out
}

func compareVIPNodes(a, b vipNode) int {
	return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.NodeID, b.NodeID))
}

// detectMoves applies the events to the state in time order, ties by node ID then sequence number, and returns the new
// state and the moves of the address, oldest first (D-23). It changes nothing it is given.
//
// A move is a MASTER state on a node while the node that held the address last is another. The node that held it last
// is remembered after it leaves, so STOP on A and then MASTER on B is one move from A to B, and a node that goes MASTER,
// BACKUP, MASTER with no other holder between records none. The very first MASTER is no move, since there is no node it
// came from. Two lines with the same node, sequence number and instant are one line read twice.
func detectMoves(prev vipEnvState, events []vipEvent, marks vipMarks) (vipEnvState, []ingressstore.VIPMove) {
	st := prev.clone()
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
	for _, ev := range ordered {
		k := key{ev.NodeID, ev.Seq, ev.At}
		if seen[k] {
			continue
		}
		seen[k] = true
		hadHolder := len(holders(st.Nodes)) > 0
		n, ok := st.Nodes[ev.NodeID]
		if !ok {
			n = vipNode{NodeID: ev.NodeID, Faults: map[string]bool{}}
		}
		if ev.Hostname != "" {
			n.Hostname = ev.Hostname
		}
		n.LastEvent = ev.At
		switch ev.Event {
		case ingressbundle.VIPEventStart:
			// The helper started: whatever the run before it said is over.
			n.TaskStarted = ev.At
			n.State, n.Reason = "", ""
			n.Faults = map[string]bool{}
			n.Foreign, n.ForeignSender, n.ForeignCounter, n.ClockSkew = false, "", "", false
		case ingressbundle.VIPEventState:
			n.State = ev.State
			if ev.State == ingressbundle.VIPStateFault {
				n.Reason = ev.Reason
				if ev.Reason != "" {
					n.Faults[ev.Reason] = true
				}
			} else {
				n.Reason = ""
				n.Faults = map[string]bool{}
			}
			if ev.State == ingressbundle.VIPStateMaster {
				n.HeldAt = ev.At
				if st.LastHolder != "" && st.LastHolder != ev.NodeID {
					from := st.Nodes[st.LastHolder]
					moves = append(moves, ingressstore.VIPMove{
						At: ev.At, FromNodeID: from.NodeID, FromNodeName: from.Hostname,
						ToNodeID: ev.NodeID, ToNodeName: n.Hostname, Reason: moveReason(from, ev.At, marks),
					})
				}
				st.LastHolder = ev.NodeID
				// A node that cannot be heard and was MASTER can no longer say it is not, so the new holder takes its
				// place. A node that can be heard and still says MASTER stays one: two holders are reported as they are.
				for id, other := range st.Nodes {
					if id != ev.NodeID && other.State == ingressbundle.VIPStateMaster && marks.Silent[id] {
						other.State = ""
						st.Nodes[id] = other
					}
				}
			}
		case ingressbundle.VIPEventFault:
			n.Reason = ev.Reason
			n.Faults[ev.Reason] = true
		case ingressbundle.VIPEventForeignRouter:
			n.Foreign = ev.Active
			n.ForeignSender, n.ForeignCounter = "", ""
			if ev.Active {
				n.ForeignSender, n.ForeignCounter = ev.Addr, ev.Counter
			}
		case ingressbundle.VIPEventClockSkew:
			n.ClockSkew = ev.Active
		}
		st.Nodes[ev.NodeID] = n
		switch has := len(holders(st.Nodes)) > 0; {
		case has:
			st.HolderlessSince = time.Time{}
		case hadHolder:
			st.HolderlessSince = ev.At
		}
	}
	return st, moves
}

// moveReason names why the address left from, at the instant of the move. The checks run in the order the list of
// reasons is written for: a switch-off explains everything while it runs, a handoff the server asked for explains the
// move next, then what the old holder itself reported, then what is only inferred from its silence.
func moveReason(from vipNode, at time.Time, marks vipMarks) string {
	if marks.Disabling {
		return VIPMoveDisabled
	}
	if m, ok := marks.Handoff[from.NodeID]; ok && at.Sub(m).Abs() <= vipHandoffWindow {
		return VIPMoveDrainHandoff
	}
	if (from.State == ingressbundle.VIPStateFault && from.Reason == ingressbundle.VIPReasonProbeFailing) ||
		from.Faults[ingressbundle.VIPReasonProbeFailing] {
		return VIPMoveProbeFailing
	}
	if from.State == ingressbundle.VIPStateMaster && marks.Silent[from.NodeID] {
		return VIPMoveHolderFailed
	}
	return VIPMoveUnknown
}
