// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

var (
	movesNodeA = strings.Repeat("a", 25)
	movesNodeB = strings.Repeat("b", 25)
	movesNodeC = strings.Repeat("c", 25)
	movesT0    = time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)
)

// movesHost is the host name a node ID is shown as in these tests.
func movesHost(node string) string { return "host-" + node[:1] }

// stateEv is a state event of a node, `s` seconds after movesT0.
func stateEv(node string, seq uint64, s int, state, reason string) vipEvent {
	return vipEvent{
		At: movesT0.Add(time.Duration(s) * time.Second), NodeID: node, Hostname: movesHost(node),
		VIPEvent: ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: state, Reason: reason, Node: node, Seq: seq},
	}
}

// apply runs events over a state.
func apply(prev vipEnvState, marks vipMarks, events ...vipEvent) (vipEnvState, []ingressstore.VIPMove) {
	return detectMoves(prev, events, marks)
}

// settle returns the state of A holding the address and B backing it up, with no move recorded.
func settle(t *testing.T) vipEnvState {
	t.Helper()
	st, moves := apply(vipEnvState{}, vipMarks{},
		stateEv(movesNodeA, 1, 0, ingressbundle.VIPStateMaster, ""),
		stateEv(movesNodeB, 1, 1, ingressbundle.VIPStateBackup, ""),
	)
	if len(moves) != 0 {
		t.Fatalf("the first holder is no move, got %+v", moves)
	}
	return st
}

func holderIDs(st vipEnvState) []string {
	var out []string
	for _, n := range holders(st.Nodes) {
		out = append(out, n.NodeID)
	}
	return out
}

func TestVIPMovesAdjacency(t *testing.T) {
	t.Run("a line read twice is one event", func(t *testing.T) {
		st := settle(t)
		master := stateEv(movesNodeB, 2, 10, ingressbundle.VIPStateMaster, "")
		_, moves := apply(st, vipMarks{}, master, master)
		if len(moves) != 1 {
			t.Fatalf("%d moves for a line seen twice, want 1: %+v", len(moves), moves)
		}
	})

	t.Run("the same node and sequence number at another instant is another event", func(t *testing.T) {
		// A helper that restarted in a new container starts its numbering again, so the number alone does not name a line.
		st := settle(t)
		_, moves := apply(st, vipMarks{},
			stateEv(movesNodeB, 1, 10, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeA, 2, 11, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeB, 1, 12, ingressbundle.VIPStateMaster, ""),
		)
		if len(moves) != 3 {
			t.Fatalf("%d moves, want 3: %+v", len(moves), moves)
		}
	})

	t.Run("MASTER, BACKUP, MASTER on one node with no other holder is no move", func(t *testing.T) {
		st := settle(t)
		st, moves := apply(st, vipMarks{},
			stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateBackup, ""),
			stateEv(movesNodeA, 3, 11, ingressbundle.VIPStateMaster, ""),
		)
		if len(moves) != 0 {
			t.Fatalf("a node that took the address back is no move, got %+v", moves)
		}
		if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeA {
			t.Fatalf("holders = %v, want [A]", got)
		}
	})

	t.Run("two moves at one instant are two moves in node order", func(t *testing.T) {
		st := settle(t)
		// Given in the wrong order on purpose: the order is the node's, not the input's.
		_, moves := apply(st, vipMarks{},
			stateEv(movesNodeC, 1, 10, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeB, 2, 10, ingressbundle.VIPStateMaster, ""),
		)
		if len(moves) != 2 {
			t.Fatalf("%d moves, want 2: %+v", len(moves), moves)
		}
		if moves[0].FromNodeID != movesNodeA || moves[0].ToNodeID != movesNodeB ||
			moves[1].FromNodeID != movesNodeB || moves[1].ToNodeID != movesNodeC {
			t.Fatalf("moves = %+v, want A to B then B to C", moves)
		}
	})

	t.Run("the input is not changed", func(t *testing.T) {
		st := settle(t)
		before := st.Nodes[movesNodeA]
		_, _ = apply(st, vipMarks{}, stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing))
		after := st.Nodes[movesNodeA]
		if after.State != before.State || len(after.Faults) != len(before.Faults) {
			t.Fatalf("detectMoves changed the state it was given: %+v then %+v", before, after)
		}
	})
}

func TestVIPMoveReasons(t *testing.T) {
	masterB := stateEv(movesNodeB, 2, 20, ingressbundle.VIPStateMaster, "")
	cases := []struct {
		name   string
		before []vipEvent
		marks  vipMarks
		want   string
	}{
		{
			name:   "a FAULT with probe_failing is probe_failing",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)},
			want:   VIPMoveProbeFailing,
		},
		{
			name:  "a holder that fell silent while MASTER is holder_failed",
			marks: vipMarks{Silent: map[string]bool{movesNodeA: true}},
			want:  VIPMoveHolderFailed,
		},
		{
			name:   "a holder that stopped and was silent is not holder_failed",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, "")},
			marks:  vipMarks{Silent: map[string]bool{movesNodeA: true}},
			want:   VIPMoveUnknown,
		},
		{
			name:   "a handoff mark within 15 s is drain_handoff",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, "")},
			marks:  vipMarks{Handoff: map[string]time.Time{movesNodeA: movesT0.Add(15 * time.Second)}},
			want:   VIPMoveDrainHandoff,
		},
		{
			name:   "a handoff mark older than 15 s explains nothing",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, "")},
			marks:  vipMarks{Handoff: map[string]time.Time{movesNodeA: movesT0.Add(2 * time.Second)}},
			want:   VIPMoveUnknown,
		},
		{
			name:   "a handoff mark for another node explains nothing",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, "")},
			marks:  vipMarks{Handoff: map[string]time.Time{movesNodeC: movesT0.Add(15 * time.Second)}},
			want:   VIPMoveUnknown,
		},
		{
			name:   "a switch-off in progress is disabled",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, "")},
			marks:  vipMarks{Disabling: true},
			want:   VIPMoveDisabled,
		},
		{
			name:   "a STOP with no mark is unknown",
			before: []vipEvent{stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, "")},
			want:   VIPMoveUnknown,
		},
		{
			name:  "a holder that still says MASTER and can be heard is unknown",
			marks: vipMarks{},
			want:  VIPMoveUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := settle(t)
			events := append(append([]vipEvent{}, tc.before...), masterB)
			_, moves := apply(st, tc.marks, events...)
			if len(moves) != 1 {
				t.Fatalf("%d moves, want 1: %+v", len(moves), moves)
			}
			m := moves[0]
			if m.Reason != tc.want {
				t.Errorf("reason = %q, want %q", m.Reason, tc.want)
			}
			if m.FromNodeID != movesNodeA || m.FromNodeName != "host-a" || m.ToNodeID != movesNodeB || m.ToNodeName != "host-b" {
				t.Errorf("move = %+v, want host-a to host-b", m)
			}
			if !m.At.Equal(masterB.At) {
				t.Errorf("at = %s, want the MASTER line's %s", m.At, masterB.At)
			}
		})
	}

	t.Run("a silent holder is replaced as holder by the new one", func(t *testing.T) {
		st, _ := apply(settle(t), vipMarks{Silent: map[string]bool{movesNodeA: true}}, masterB)
		if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeB {
			t.Fatalf("holders = %v, want [B]", got)
		}
	})

	t.Run("a holder that can be heard and still says MASTER stays one", func(t *testing.T) {
		st, _ := apply(settle(t), vipMarks{}, masterB)
		if got := holderIDs(st); len(got) != 2 {
			t.Fatalf("holders = %v, want both nodes reported", got)
		}
	})

	t.Run("a move into no holder is recorded when the next MASTER arrives", func(t *testing.T) {
		st, moves := apply(settle(t), vipMarks{}, stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, ""))
		if len(moves) != 0 || len(holders(st.Nodes)) != 0 {
			t.Fatalf("a STOP with no new holder moved %+v, holders %v", moves, holderIDs(st))
		}
		st, moves = apply(st, vipMarks{}, masterB)
		if len(moves) != 1 || moves[0].FromNodeID != movesNodeA || moves[0].ToNodeID != movesNodeB || moves[0].Reason != VIPMoveUnknown {
			t.Fatalf("moves = %+v, want one from A to B, unknown", moves)
		}
		if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeB {
			t.Fatalf("holders = %v, want [B]", got)
		}
	})

	t.Run("the address coming back to the node that lost it is no move", func(t *testing.T) {
		st, _ := apply(settle(t), vipMarks{}, stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, ""))
		_, moves := apply(st, vipMarks{}, stateEv(movesNodeA, 3, 20, ingressbundle.VIPStateMaster, ""))
		if len(moves) != 0 {
			t.Fatalf("moves = %+v, want none", moves)
		}
	})
}

func TestVIPHolderNoneOneMany(t *testing.T) {
	st := vipEnvState{}
	if got := holders(st.Nodes); len(got) != 0 {
		t.Fatalf("holders of nothing = %v", got)
	}

	st, _ = apply(st, vipMarks{}, stateEv(movesNodeB, 1, 0, ingressbundle.VIPStateBackup, ""))
	if got := holderIDs(st); len(got) != 0 || !st.HolderlessSince.IsZero() {
		t.Fatalf("a backup alone: holders %v, holderless since %s", got, st.HolderlessSince)
	}

	st, _ = apply(st, vipMarks{}, stateEv(movesNodeA, 1, 1, ingressbundle.VIPStateMaster, ""))
	if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeA || !st.HolderlessSince.IsZero() {
		t.Fatalf("one holder: holders %v, holderless since %s", got, st.HolderlessSince)
	}

	st, _ = apply(st, vipMarks{}, stateEv(movesNodeA, 2, 5, ingressbundle.VIPStateStop, ""))
	if got := holderIDs(st); len(got) != 0 {
		t.Fatalf("after the holder left: holders %v", got)
	}
	if want := movesT0.Add(5 * time.Second); !st.HolderlessSince.Equal(want) {
		t.Fatalf("holderless since %s, want %s, when the last holder left", st.HolderlessSince, want)
	}

	// The instant stays the one the holder left at while no one holds the address.
	st, _ = apply(st, vipMarks{}, stateEv(movesNodeB, 2, 8, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing))
	if want := movesT0.Add(5 * time.Second); !st.HolderlessSince.Equal(want) {
		t.Fatalf("holderless since %s, want it kept at %s", st.HolderlessSince, want)
	}

	st, _ = apply(st, vipMarks{}, stateEv(movesNodeB, 3, 9, ingressbundle.VIPStateMaster, ""))
	if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeB || !st.HolderlessSince.IsZero() {
		t.Fatalf("a new holder: holders %v, holderless since %s", got, st.HolderlessSince)
	}

	// Two holders at once are both reported, sorted by host name.
	st, _ = apply(st, vipMarks{}, stateEv(movesNodeA, 3, 12, ingressbundle.VIPStateMaster, ""))
	if got := holderIDs(st); len(got) != 2 || got[0] != movesNodeA || got[1] != movesNodeB {
		t.Fatalf("two holders: %v, want [A B]", got)
	}

	// With two, one leaving leaves a holder; only the last leaving makes the address holder-less.
	st, _ = apply(st, vipMarks{}, stateEv(movesNodeB, 4, 15, ingressbundle.VIPStateStop, ""))
	if got := holderIDs(st); len(got) != 1 || !st.HolderlessSince.IsZero() {
		t.Fatalf("one of two left: holders %v, holderless since %s", got, st.HolderlessSince)
	}
	st, _ = apply(st, vipMarks{}, stateEv(movesNodeA, 4, 16, ingressbundle.VIPStateStop, ""))
	if want := movesT0.Add(16 * time.Second); !st.HolderlessSince.Equal(want) {
		t.Fatalf("holderless since %s, want %s", st.HolderlessSince, want)
	}
}

// TestVIPMovesFollowTheNodeThatKeepsTheAddress: two nodes claim the address at the first election and the later one gives it
// up again. The node that keeps it is the one that held it last, so when a drain stops that node, the move away from it
// is found by the handoff mark for it. Before, the later claimant stayed the one that held it last, the move was read as
// from a node that no longer held the address, and the history said unknown (found at the phase gate, TestSwarmShared/VirtualIP).
func TestVIPMovesFollowTheNodeThatKeepsTheAddress(t *testing.T) {
	st := settle(t)
	marks := vipMarks{Handoff: map[string]time.Time{movesNodeA: movesT0.Add(19 * time.Second)}}
	st, moves := apply(st, marks,
		stateEv(movesNodeB, 2, 10, ingressbundle.VIPStateMaster, ""),
		stateEv(movesNodeB, 3, 11, ingressbundle.VIPStateBackup, ""),
	)
	if len(moves) != 1 || moves[0].FromNodeID != movesNodeA || moves[0].ToNodeID != movesNodeB {
		t.Fatalf("the double claim stores %+v, want the one move A to B", moves)
	}
	if st.LastHolder != movesNodeA {
		t.Fatalf("the node that held it last is %q after B gave it up, want A", st.LastHolder)
	}
	if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeA {
		t.Fatalf("holders = %v, want A alone", got)
	}

	_, moves = apply(st, marks,
		stateEv(movesNodeA, 2, 20, ingressbundle.VIPStateStop, ""),
		stateEv(movesNodeB, 4, 21, ingressbundle.VIPStateMaster, ""),
	)
	if len(moves) != 1 {
		t.Fatalf("%d moves after the drain, want 1: %+v", len(moves), moves)
	}
	if m := moves[0]; m.FromNodeID != movesNodeA || m.ToNodeID != movesNodeB || m.Reason != VIPMoveDrainHandoff {
		t.Fatalf("the move = %+v, want A to B with the reason %s", m, VIPMoveDrainHandoff)
	}
}

// TestVIPMovesKeepTheLastHolderWhenNoneRemains: with nobody holding the address the node that held it last stays the one
// it was, so a STOP on A and a MASTER on B later is still a move from A (the case the double claim must not disturb).
func TestVIPMovesKeepTheLastHolderWhenNoneRemains(t *testing.T) {
	st, moves := apply(settle(t), vipMarks{},
		stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateBackup, ""),
		stateEv(movesNodeB, 2, 15, ingressbundle.VIPStateMaster, ""),
	)
	if len(moves) != 1 || moves[0].FromNodeID != movesNodeA || moves[0].ToNodeID != movesNodeB {
		t.Fatalf("moves = %+v, want the one move A to B", moves)
	}
	if st.LastHolder != movesNodeB {
		t.Fatalf("the node that held it last is %q, want B", st.LastHolder)
	}
}

// faultEv is a fault event of a node, `s` seconds after movesT0.
func faultEv(node string, seq uint64, s int, reason string, active bool) vipEvent {
	return vipEvent{
		At: movesT0.Add(time.Duration(s) * time.Second), NodeID: node, Hostname: movesHost(node),
		VIPEvent: ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: reason, Active: active, Node: node, Seq: seq},
	}
}

// TestVIPFaultLineThatClearsRecordsNoFault pins WR-06: the helper writes a fault line with active false to say a fault is
// over, so such a line takes the fault away and never records one.
func TestVIPFaultLineThatClearsRecordsNoFault(t *testing.T) {
	st, _ := apply(vipEnvState{}, vipMarks{},
		faultEv(movesNodeA, 1, 0, ingressbundle.VIPReasonAddressInUse, true),
		stateEv(movesNodeA, 2, 1, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing),
	)
	a := st.Nodes[movesNodeA]
	if !a.Faults[ingressbundle.VIPReasonAddressInUse] || !a.Faults[ingressbundle.VIPReasonProbeFailing] {
		t.Fatalf("faults = %v, want address_in_use and probe_failing standing", a.Faults)
	}

	st, _ = apply(st, vipMarks{}, faultEv(movesNodeA, 3, 2, ingressbundle.VIPReasonAddressInUse, false))
	a = st.Nodes[movesNodeA]
	if a.Faults[ingressbundle.VIPReasonAddressInUse] {
		t.Errorf("the cleared address_in_use is still recorded as standing: %v", a.Faults)
	}
	if !a.Faults[ingressbundle.VIPReasonProbeFailing] || a.Reason != ingressbundle.VIPReasonProbeFailing {
		t.Errorf("faults = %v, reason = %q: the clear of one fault must leave the other standing", a.Faults, a.Reason)
	}

	// The fault the node was last reporting is the one cleared: no reason is left shown.
	st, _ = apply(vipEnvState{}, vipMarks{},
		faultEv(movesNodeB, 1, 0, ingressbundle.VIPReasonNoInterface, true),
		faultEv(movesNodeB, 2, 1, ingressbundle.VIPReasonNoInterface, false),
	)
	if b := st.Nodes[movesNodeB]; b.Reason != "" || len(b.Faults) != 0 {
		t.Errorf("reason = %q, faults = %v after the fault was cleared, want none", b.Reason, b.Faults)
	}
}
