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

// movesPass is the instant of the pass that reads the events in apply; the pass that follows it is a settle window later.
var movesPass = movesT0.Add(time.Hour)

// apply runs events over a state in one pass, and then takes the pass after it, which reads nothing new: what the events left
// open is settled, as it is in the running reader.
func apply(prev vipEnvState, marks vipMarks, events ...vipEvent) (vipEnvState, []ingressstore.VIPMove) {
	st, moves := applyOnce(prev, marks, events...)
	marks.Now = movesPass.Add(vipSettleWindow)
	st, more := detectMoves(st, nil, marks)
	return st, append(moves, more...)
}

// applyOnce runs events over a state in one pass and no more.
func applyOnce(prev vipEnvState, marks vipMarks, events ...vipEvent) (vipEnvState, []ingressstore.VIPMove) {
	marks.Now = movesPass
	return detectMoves(prev, events, marks)
}

// moveText is the moves as "a>b reason", in order.
func moveText(moves []ingressstore.VIPMove) string {
	var out []string
	for _, m := range moves {
		out = append(out, m.FromNodeID[:1]+">"+m.ToNodeID[:1]+" "+m.Reason)
	}
	return strings.Join(out, ", ")
}

// msEv is a state event of a node, `ms` milliseconds after movesT0.
func msEv(node string, seq uint64, ms int, state, reason string) vipEvent {
	ev := stateEv(node, seq, 0, state, reason)
	ev.At = movesT0.Add(time.Duration(ms) * time.Millisecond)
	return ev
}

// startEv is a start line of a node, `ms` milliseconds after movesT0.
func startEv(node string, ms int) vipEvent {
	ev := msEv(node, 1, ms, "", "")
	ev.VIPEvent = ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: node, Seq: 1, Iface: "eth0"}
	return ev
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
		_, moves := apply(st, vipMarks{}, stateEv(movesNodeA, 2, 9, ingressbundle.VIPStateStop, ""), master, master)
		if len(moves) != 1 {
			t.Fatalf("%d moves for a line seen twice, want 1: %+v", len(moves), moves)
		}
	})

	t.Run("the same node and sequence number at another instant is another event", func(t *testing.T) {
		// A helper that restarted in a new container starts its numbering again, so the number alone does not name a line.
		st := settle(t)
		_, moves := apply(st, vipMarks{},
			stateEv(movesNodeA, 2, 9, ingressbundle.VIPStateStop, ""),
			stateEv(movesNodeB, 1, 10, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeB, 2, 15, ingressbundle.VIPStateStop, ""),
			stateEv(movesNodeA, 1, 16, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeA, 2, 25, ingressbundle.VIPStateStop, ""),
			stateEv(movesNodeB, 1, 26, ingressbundle.VIPStateMaster, ""),
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

	t.Run("claims at one instant are applied in node order, whatever order they are given in", func(t *testing.T) {
		st := settle(t)
		events := []vipEvent{
			stateEv(movesNodeC, 1, 10, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeB, 2, 10, ingressbundle.VIPStateMaster, ""),
			stateEv(movesNodeA, 2, 9, ingressbundle.VIPStateStop, ""),
			stateEv(movesNodeB, 3, 11, ingressbundle.VIPStateBackup, ""),
		}
		_, moves := apply(st, vipMarks{}, events...)
		if got := moveText(moves); got != "a>c unknown" {
			t.Fatalf("moves = %q, want a>c unknown: the node that kept the address", got)
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

	t.Run("a holder that can be heard and still says MASTER stays one, and nothing moved", func(t *testing.T) {
		st, moves := apply(settle(t), vipMarks{}, masterB)
		if len(moves) != 0 {
			t.Fatalf("moves = %q, want none while both nodes say they hold it", moveText(moves))
		}
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
	if len(moves) != 0 {
		t.Fatalf("the double claim stores %q, want no move: the address stayed with A (WR-05)", moveText(moves))
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

// TestVIPMovesOneFailoverIsOneMove pins what the LAN dry run of 2026-11-25 showed wrong (VIP-03, WR-05): with two backups both
// claim the address when the holder goes and the one with the higher address keeps it. Whichever claims first, and whatever
// instant the reader's pass falls at, the history holds one move, to the node that kept it, with the old holder's own reason.
func TestVIPMovesOneFailoverIsOneMove(t *testing.T) {
	m, b := ingressbundle.VIPStateMaster, ingressbundle.VIPStateBackup
	probe := msEv(movesNodeA, 2, 10000, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)
	cases := []struct {
		name   string
		events []vipEvent
		marks  vipMarks
		want   string
	}{
		{"the winner claims first", []vipEvent{probe, msEv(movesNodeC, 2, 13000, m, ""), msEv(movesNodeB, 2, 13010, m, ""), msEv(movesNodeB, 3, 13020, b, "")}, vipMarks{}, "a>c probe_failing"},
		{"the loser claims first", []vipEvent{probe, msEv(movesNodeB, 2, 13000, m, ""), msEv(movesNodeC, 2, 13010, m, ""), msEv(movesNodeB, 3, 13900, b, "")}, vipMarks{}, "a>c probe_failing"},
		{"the loser gives up before the winner claims", []vipEvent{probe, msEv(movesNodeB, 2, 13000, m, ""), msEv(movesNodeB, 3, 13300, b, ""), msEv(movesNodeC, 2, 13310, m, "")}, vipMarks{}, "a>c probe_failing"},
		{"the old holder is back as a backup before the claim is settled", []vipEvent{probe, msEv(movesNodeC, 2, 13000, m, ""), msEv(movesNodeA, 3, 13500, b, "")}, vipMarks{}, "a>c probe_failing"},
		{"a killed holder whose node is not heard", []vipEvent{msEv(movesNodeB, 2, 13000, m, ""), msEv(movesNodeC, 2, 13010, m, ""), msEv(movesNodeB, 3, 13020, b, "")}, vipMarks{Silent: map[string]bool{movesNodeA: true}}, "a>c holder_failed"},
		{"a killed holder whose new container started before the claims", []vipEvent{startEv(movesNodeA, 12000), msEv(movesNodeB, 2, 13000, m, ""), msEv(movesNodeC, 2, 13010, m, ""), msEv(movesNodeB, 3, 13020, b, "")}, vipMarks{}, "a>c holder_failed"},
		{"a killed holder whose new container started after the claims", []vipEvent{msEv(movesNodeB, 2, 13000, m, ""), msEv(movesNodeC, 2, 13010, m, ""), msEv(movesNodeB, 3, 13020, b, ""), startEv(movesNodeA, 15000)}, vipMarks{}, "a>c holder_failed"},
		{"a holder that stopped in order and started again", []vipEvent{msEv(movesNodeA, 2, 10000, ingressbundle.VIPStateStop, ""), startEv(movesNodeA, 12000), msEv(movesNodeC, 2, 13000, m, "")}, vipMarks{}, "a>c unknown"},
		{"a drained holder", []vipEvent{msEv(movesNodeA, 2, 10000, ingressbundle.VIPStateStop, ""), msEv(movesNodeB, 2, 10500, m, ""), msEv(movesNodeC, 2, 10510, m, ""), msEv(movesNodeB, 3, 10520, b, "")}, vipMarks{Handoff: map[string]time.Time{movesNodeA: movesT0.Add(10 * time.Second)}}, "a>c drain_handoff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, moves := apply(settle(t), tc.marks, tc.events...); moveText(moves) != tc.want {
				t.Errorf("read in one pass: moves = %q, want %q", moveText(moves), tc.want)
			}
			// The same lines read in two passes, cut at every place, are the same one move.
			for cut := 1; cut < len(tc.events); cut++ {
				st, first := applyOnce(settle(t), tc.marks, tc.events[:cut]...)
				_, rest := apply(st, tc.marks, tc.events[cut:]...)
				if got := moveText(append(first, rest...)); got != tc.want {
					t.Errorf("read in two passes cut at %d: moves = %q, want %q", cut, got, tc.want)
				}
			}
		})
	}
}

// TestVIPMovesAFirstElectionIsNoMove pins WR-05: however many nodes claim the address at the first election, and whoever keeps
// it, nothing moved, and the node that kept it is the one a later move is from.
func TestVIPMovesAFirstElectionIsNoMove(t *testing.T) {
	st, moves := apply(vipEnvState{}, vipMarks{},
		msEv(movesNodeA, 1, 0, ingressbundle.VIPStateMaster, ""),
		msEv(movesNodeB, 1, 10, ingressbundle.VIPStateMaster, ""),
		msEv(movesNodeC, 1, 20, ingressbundle.VIPStateMaster, ""),
		msEv(movesNodeA, 2, 30, ingressbundle.VIPStateBackup, ""),
		msEv(movesNodeB, 2, 1030, ingressbundle.VIPStateBackup, ""),
	)
	if len(moves) != 0 {
		t.Fatalf("the first election stores %q, want no move", moveText(moves))
	}
	if st.LastHolder != movesNodeC {
		t.Fatalf("the settled holder is %q, want C", st.LastHolder)
	}
}

// TestVIPMovesSettleByTheLinesOwnTime: lines read together long after they were written (the server was down) give the moves
// they show. An address that stayed two seconds or longer with a node was there; a shorter claim was a lost election.
func TestVIPMovesSettleByTheLinesOwnTime(t *testing.T) {
	_, moves := apply(settle(t), vipMarks{},
		msEv(movesNodeA, 2, 10000, ingressbundle.VIPStateStop, ""),
		msEv(movesNodeB, 2, 11000, ingressbundle.VIPStateMaster, ""),
		msEv(movesNodeB, 3, 13000, ingressbundle.VIPStateStop, ""),
		msEv(movesNodeC, 2, 14000, ingressbundle.VIPStateMaster, ""),
	)
	if got := moveText(moves); got != "a>b unknown, b>c unknown" {
		t.Fatalf("moves = %q, want a>b unknown, b>c unknown", got)
	}
	if !moves[0].At.Equal(movesT0.Add(11*time.Second)) || !moves[1].At.Equal(movesT0.Add(14*time.Second)) {
		t.Fatalf("moves are at %s and %s, want the instants of the MASTER lines", moves[0].At, moves[1].At)
	}
}

// TestVIPMovesAClaimIsNotSettledAtOnce: the pass that first reads a MASTER line stores nothing, since the line of a node that
// claims the address too may be in a log that has not been read yet.
func TestVIPMovesAClaimIsNotSettledAtOnce(t *testing.T) {
	st, moves := applyOnce(settle(t), vipMarks{},
		stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, ""),
		stateEv(movesNodeB, 2, 11, ingressbundle.VIPStateMaster, ""),
	)
	if len(moves) != 0 || !st.claimOpen() {
		t.Fatalf("moves = %q, claim open %v, want none stored and the claim open", moveText(moves), st.claimOpen())
	}
	early, moves := detectMoves(st, nil, vipMarks{Now: movesPass.Add(vipSettleWindow - time.Millisecond)})
	if len(moves) != 0 || !early.claimOpen() {
		t.Fatalf("settled before the window ended: %q", moveText(moves))
	}
	// A switch-off ends the reader's work on the environment, so a claim is not held back while one runs.
	if _, moves := applyOnce(settle(t), vipMarks{Disabling: true}, stateEv(movesNodeA, 2, 10, ingressbundle.VIPStateStop, ""), stateEv(movesNodeB, 2, 11, ingressbundle.VIPStateMaster, "")); moveText(moves) != "a>b disabled" {
		t.Fatalf("while switching off: moves = %q, want a>b disabled", moveText(moves))
	}
}

// TestVIPFaultShowsTheCause pins what the LAN dry run of 2026-11-25 showed wrong (D-11, A-05): the address_in_use check makes
// the probe fail, so keepalived's FAULT line says probe_failing after it; the fault shown is the cause and not its effect.
func TestVIPFaultShowsTheCause(t *testing.T) {
	st, _ := apply(vipEnvState{}, vipMarks{},
		startEv(movesNodeA, 0),
		faultEv(movesNodeA, 2, 1, ingressbundle.VIPReasonAddressInUse, true),
		stateEv(movesNodeA, 3, 2, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing),
	)
	if got := st.Nodes[movesNodeA].Reason; got != ingressbundle.VIPReasonAddressInUse {
		t.Fatalf("reason = %q, want address_in_use while it stands", got)
	}
	// Once the other machine is gone, what is left is the probe, until the node is a backup.
	st, _ = apply(st, vipMarks{}, faultEv(movesNodeA, 4, 20, ingressbundle.VIPReasonAddressInUse, false))
	if got := st.Nodes[movesNodeA].Reason; got != ingressbundle.VIPReasonProbeFailing {
		t.Fatalf("reason = %q after the clear, want probe_failing", got)
	}
	st, _ = apply(st, vipMarks{}, stateEv(movesNodeA, 5, 22, ingressbundle.VIPStateBackup, ""))
	if n := st.Nodes[movesNodeA]; n.Reason != "" || len(n.Faults) != 0 {
		t.Fatalf("reason = %q, faults %v on a backup, want none", n.Reason, n.Faults)
	}
}

// TestVIPMovesWhileTwoNodesClaim: two nodes that both go on saying they hold the address are shown as two holders, and nothing
// has moved whichever of them is listed first. When the old holder is no longer heard, the one that is heard has the address.
func TestVIPMovesWhileTwoNodesClaim(t *testing.T) {
	st, moves := apply(vipEnvState{}, vipMarks{},
		stateEv(movesNodeC, 1, 0, ingressbundle.VIPStateMaster, ""),
		stateEv(movesNodeA, 1, 10, ingressbundle.VIPStateMaster, ""),
		stateEv(movesNodeB, 1, 30, ingressbundle.VIPStateBackup, ""),
	)
	if len(moves) != 0 || st.LastHolder != movesNodeC || len(holderIDs(st)) != 2 {
		t.Fatalf("moves = %q, settled holder %q, holders %v; want no move, C settled and both shown", moveText(moves), st.LastHolder, holderIDs(st))
	}

	_, moves = apply(st, vipMarks{Silent: map[string]bool{movesNodeC: true}})
	if got := moveText(moves); got != "c>a holder_failed" {
		t.Fatalf("moves = %q, want c>a holder_failed once C is not heard", got)
	}
	if !moves[0].At.Equal(movesT0.Add(10 * time.Second)) {
		t.Fatalf("the move is at %s, want A's MASTER line", moves[0].At)
	}
}

// TestVIPMovesSilenceIsNotHistory pins the review of 26-35: that a node cannot be heard now says nothing of the lines of the
// past, and a node that cannot be heard is replaced only by one that took the address after it did.
func TestVIPMovesSilenceIsNotHistory(t *testing.T) {
	m, b, stop := ingressbundle.VIPStateMaster, ingressbundle.VIPStateBackup, ingressbundle.VIPStateStop
	silentA := vipMarks{Silent: map[string]bool{movesNodeA: true}}

	t.Run("a drained holder's replayed election", func(t *testing.T) {
		_, moves := apply(vipEnvState{}, silentA,
			msEv(movesNodeA, 1, 0, m, ""), msEv(movesNodeB, 1, 100, m, ""), msEv(movesNodeB, 2, 900, b, ""),
			msEv(movesNodeA, 2, 1000000, stop, ""), msEv(movesNodeB, 3, 1001000, m, ""))
		if got := moveText(moves); got != "a>b unknown" {
			t.Fatalf("moves = %q, want a>b unknown", got)
		}
	})
	t.Run("a lost claim does not hide a holder that missed one read", func(t *testing.T) {
		st, _ := applyOnce(settle(t), vipMarks{}, msEv(movesNodeA, 2, 9000, stop, ""), msEv(movesNodeB, 2, 10000, m, ""))
		silentB := vipMarks{Silent: map[string]bool{movesNodeB: true}}
		st, first := applyOnce(st, silentB, msEv(movesNodeC, 1, 10100, m, ""))
		st, second := applyOnce(st, vipMarks{}, msEv(movesNodeC, 2, 10900, b, ""))
		if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeB {
			t.Fatalf("holders = %v, want [B]", got)
		}
		_, third := apply(st, vipMarks{})
		if got := moveText(append(append(first, second...), third...)); got != "a>b unknown" {
			t.Fatalf("moves = %q, want a>b unknown", got)
		}
	})
	t.Run("an older MASTER line does not hide a newer holder", func(t *testing.T) {
		st, _ := apply(settle(t), vipMarks{}, msEv(movesNodeA, 2, 20000, stop, ""), msEv(movesNodeA, 3, 21000, m, ""))
		st, moves := apply(st, silentA, msEv(movesNodeB, 2, 11000, m, ""))
		st2, more := apply(st, silentA, msEv(movesNodeB, 3, 12000, b, ""))
		if got := holderIDs(st2); len(moves)+len(more) != 0 || len(got) != 1 || got[0] != movesNodeA {
			t.Fatalf("holders = %v, moves %q, want [A] and none", got, moveText(append(moves, more...)))
		}
	})
	t.Run("a holder that is heard again and says MASTER is a holder again", func(t *testing.T) {
		st, _ := apply(settle(t), silentA, msEv(movesNodeB, 2, 10000, m, ""))
		st, _ = apply(st, vipMarks{}, msEv(movesNodeB, 3, 60000, b, ""))
		if got := holderIDs(st); len(got) != 1 || got[0] != movesNodeA {
			t.Fatalf("holders = %v, want [A]", got)
		}
	})
	t.Run("a holder that stays silent is not one again when its successor goes", func(t *testing.T) {
		st, _ := apply(settle(t), silentA, msEv(movesNodeB, 2, 10000, m, ""))
		st, _ = apply(st, silentA, msEv(movesNodeB, 3, 60000, stop, ""))
		if got := holderIDs(st); len(got) != 0 {
			t.Fatalf("holders = %v, want none", got)
		}
	})
	t.Run("one missed read of a holder moves nothing", func(t *testing.T) {
		st, _ := apply(settle(t), vipMarks{}, msEv(movesNodeB, 2, 10000, m, ""))
		st, first := applyOnce(st, silentA)
		_, second := apply(st, vipMarks{}, msEv(movesNodeB, 3, 90000, m, ""))
		if got := moveText(append(first, second...)); got != "" {
			t.Fatalf("moves = %q, want none", got)
		}
	})
}
