// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"slices"
	"testing"
	"time"
)

// The marks between a service's lines (plan 06-30, OBS-02, D-34, D-37): a task was replaced, a
// slot is waiting for a task, a node cannot be reached. The functions are pure; no daemon.

var markT0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

// mtask is a task made sec seconds after markT0 that reached its state ended seconds later.
func mtask(id string, slot int, node, state string, sec int) taskInfo {
	ended := state == "failed" || state == "shutdown" || state == "rejected" || state == "complete"
	ti := taskInfo{
		ID: id, Slot: slot, NodeID: "id-" + node, Node: node, State: state, Ended: ended, NodeState: "ready",
		Created: markT0.Add(time.Duration(sec) * time.Second),
	}
	ti.Since = ti.Created.Add(5 * time.Second)
	return ti
}

func withExit(ti taskInfo, code int) taskInfo {
	ti.ExitCode = &code
	return ti
}

func stampOf(sec int) string {
	return markT0.Add(time.Duration(sec) * time.Second).Format(logTimeLayout)
}

func TestSlotMarkersAFailedTaskWithANewOneInItsSlotIsReplacedWithItsExitCode(t *testing.T) {
	t.Parallel()
	before := []taskInfo{mtask("old", 2, "node1", "running", 0), mtask("other", 1, "node1", "running", 0)}
	after := []taskInfo{
		withExit(mtask("old", 2, "node1", "failed", 0), 137),
		mtask("new", 2, "node2", "starting", 20),
		mtask("other", 1, "node1", "running", 0),
	}
	got := slotMarkers(before, after)
	if len(got) != 1 {
		t.Fatalf("markers = %+v, want one", got)
	}
	m := got[0]
	if m.Kind != markerReplaced || m.Slot != 2 || m.Old != "old" || m.OldState != "failed" || m.New != "new" || m.Node != "node2" || m.OldNode != "node1" {
		t.Errorf("marker = %+v, want old replaced by new on node2 in slot 2", m)
	}
	if m.ExitCode == nil || *m.ExitCode != 137 {
		t.Errorf("exit code = %v, want 137", m.ExitCode)
	}
	if want := stampOf(5); m.At != want {
		t.Errorf("at = %q, want the time the old task reached its state (%q)", m.At, want)
	}
}

func TestSlotMarkersCoverShutdownAndRejectedAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"shutdown", "rejected", "failed"} {
		got := slotMarkers(
			[]taskInfo{mtask("old", 1, "n1", "running", 0)},
			[]taskInfo{mtask("old", 1, "n1", state, 0), mtask("new", 1, "n1", "running", 10)},
		)
		if len(got) != 1 || got[0].Kind != markerReplaced || got[0].OldState != state {
			t.Errorf("state %s: markers = %+v, want one replaced marker", state, got)
		}
	}
	// A task that completed (a job's) or is still going is not a replacement.
	for _, state := range []string{"complete", "running", "starting", "orphaned", "remove"} {
		got := slotMarkers(
			[]taskInfo{mtask("old", 1, "n1", "running", 0)},
			[]taskInfo{mtask("old", 1, "n1", state, 0), mtask("new", 1, "n1", "running", 10)},
		)
		if len(got) != 0 {
			t.Errorf("state %s: markers = %+v, want none", state, got)
		}
	}
}

func TestSlotMarkersWithNoNewTaskYetSaysTheSlotStoppedAndLaterBecomesAReplacement(t *testing.T) {
	t.Parallel()
	running := []taskInfo{mtask("old", 3, "n1", "running", 0)}
	stopped := []taskInfo{mtask("old", 3, "n1", "failed", 0)}
	got := slotMarkers(running, stopped)
	if len(got) != 1 || got[0].Kind != markerStopped || got[0].Old != "old" || got[0].OldState != "failed" || got[0].New != "" {
		t.Fatalf("markers = %+v, want one stopped marker for old", got)
	}
	// Told once: the same tasks again say nothing.
	if again := slotMarkers(stopped, stopped); len(again) != 0 {
		t.Errorf("markers = %+v for unchanged tasks, want none", again)
	}
	// The slot gets its task: the marker is now a replacement of the same old task.
	replaced := slices.Concat(stopped, []taskInfo{mtask("new", 3, "n2", "starting", 30)})
	got = slotMarkers(stopped, replaced)
	if len(got) != 1 || got[0].Kind != markerReplaced || got[0].Old != "old" || got[0].New != "new" {
		t.Fatalf("markers = %+v, want old replaced by new", got)
	}
}

func TestSlotMarkersPairAGlobalServicesTasksByNode(t *testing.T) {
	t.Parallel()
	before := []taskInfo{mtask("a1", 0, "n1", "running", 0), mtask("b1", 0, "n2", "running", 0)}
	after := []taskInfo{
		mtask("a1", 0, "n1", "shutdown", 0), mtask("b1", 0, "n2", "running", 0),
		mtask("a2", 0, "n1", "starting", 10),
	}
	got := slotMarkers(before, after)
	if len(got) != 1 || got[0].Old != "a1" || got[0].New != "a2" || got[0].Slot != 0 || got[0].Node != "n1" {
		t.Fatalf("markers = %+v, want a1 replaced by a2 on n1 and nothing for n2", got)
	}
}

func TestSlotMarkersWithoutPreviousGiveTheWholeHistoryInTimeOrder(t *testing.T) {
	t.Parallel()
	current := []taskInfo{
		mtask("s1-b", 1, "n1", "failed", 100),
		mtask("s1-a", 1, "n1", "failed", 0),
		mtask("s1-c", 1, "n1", "running", 200),
		mtask("s2-a", 2, "n2", "shutdown", 50),
	}
	got := slotMarkers(nil, current)
	var olds []string
	for _, m := range got {
		olds = append(olds, m.Old)
	}
	// s1-a -> s1-b (ended at +5), s2-a stopped (ended at +55), s1-b -> s1-c (ended at +105).
	if want := []string{"s1-a", "s2-a", "s1-b"}; !slices.Equal(olds, want) {
		t.Fatalf("markers for %v, want %v: %+v", olds, want, got)
	}
	if got[1].Kind != markerStopped {
		t.Errorf("s2-a marker = %+v, want stopped", got[1])
	}
}

func TestSlotMarkersWithNoTimeFromTheSwarmHaveNoAt(t *testing.T) {
	t.Parallel()
	old := mtask("old", 1, "n1", "failed", 0)
	old.Created, old.Since = time.Time{}, time.Time{}
	got := slotMarkers(nil, []taskInfo{old})
	if len(got) != 1 || got[0].At != "" {
		t.Fatalf("markers = %+v, want one without a time", got)
	}
}

func TestNodeNoticesTellOfANodeGoingDownDisconnectedAndComingBack(t *testing.T) {
	t.Parallel()
	ready := []nodeInfo{{ID: "1", Name: "alpha", State: "ready"}, {ID: "2", Name: "beta", State: "ready"}}
	down := []nodeInfo{{ID: "1", Name: "alpha", State: "down"}, {ID: "2", Name: "beta", State: "ready"}}
	got := nodeNotices(ready, down, "T1")
	if len(got) != 1 || got[0].Kind != noticeDown || got[0].Node != "alpha" || got[0].At != "T1" {
		t.Fatalf("notices = %+v, want alpha down", got)
	}
	if again := nodeNotices(down, down, "T2"); len(again) != 0 {
		t.Errorf("notices = %+v for a node that stays down, want none", again)
	}
	got = nodeNotices(down, ready, "T3")
	if len(got) != 1 || got[0].Kind != noticeBack || got[0].Node != "alpha" {
		t.Fatalf("notices = %+v, want alpha back", got)
	}
	gone := []nodeInfo{{ID: "1", Name: "alpha", State: "disconnected"}, {ID: "2", Name: "beta", State: "ready"}}
	got = nodeNotices(ready, gone, "T4")
	if len(got) != 1 || got[0].Kind != noticeDisconnected || got[0].Node != "alpha" {
		t.Fatalf("notices = %+v, want alpha disconnected", got)
	}
}

func TestNodeNoticesTellOfANodeThatIsAlreadyDownWhenThereIsNothingBeforeAndSayNothingForUnknown(t *testing.T) {
	t.Parallel()
	got := nodeNotices(nil, []nodeInfo{{ID: "1", Name: "zeta", State: "down"}, {ID: "2", Name: "beta", State: "ready"}, {ID: "3", Name: "gamma", State: "unknown"}, {ID: "4", Name: "delta", State: ""}}, "T")
	if len(got) != 1 || got[0].Kind != noticeDown || got[0].Node != "zeta" {
		t.Fatalf("notices = %+v, want only zeta down", got)
	}
}

func TestNodeNoticesAreInNodeNameOrder(t *testing.T) {
	t.Parallel()
	cur := []nodeInfo{{ID: "1", Name: "zeta", State: "down"}, {ID: "2", Name: "alpha", State: "disconnected"}}
	got := nodeNotices(nil, cur, "T")
	if len(got) != 2 || got[0].Node != "alpha" || got[1].Node != "zeta" {
		t.Fatalf("notices = %+v, want alpha then zeta", got)
	}
}
