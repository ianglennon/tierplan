// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"slices"
	"strconv"
)

// The marks a service's log shows between its lines (plan 06-30, OBS-02, D-34, D-37): a task
// was replaced, a slot is waiting for a task, a node cannot be reached.

const (
	markerReplaced = "replaced"
	markerStopped  = "stopped"

	// nodeStateDisconnected is the swarm's word for a node whose agent lost its manager; the
	// notice kinds down and disconnected are the swarm's own words for the node.
	nodeStateDisconnected = "disconnected"
	noticeDown            = nodeStateDown
	noticeDisconnected    = nodeStateDisconnected
	noticeBack            = "back"
)

// slotMarker says what happened to the task that last ran in a slot. A replaced marker names
// the task that took over and the node it starts on; a stopped one has no successor yet.
type slotMarker struct {
	Kind string `json:"kind"`
	// Service names the service the slot belongs to in a stack's log, which merges several; it is
	// left out of a service's own log. Unit is "container" for a container of a standalone stack
	// that a deploy replaced, and left out for a swarm task.
	Service string `json:"service,omitempty"`
	Unit    string `json:"unit,omitempty"`
	// Slot is the replica number, 0 for a task of a global service.
	Slot int `json:"slot"`
	// Old is the task that ended, OldState how (failed, shutdown or rejected), OldNode where it
	// ran and ExitCode the exit code of its container when it has one.
	Old      string `json:"old"`
	OldState string `json:"oldState"`
	OldNode  string `json:"oldNode"`
	ExitCode *int   `json:"exitCode,omitempty"`
	// New and Node are the task that took over and the node it was given, for a replaced marker.
	New  string `json:"new,omitempty"`
	Node string `json:"node,omitempty"`
	// At is when the old task ended, in the log's time layout, so the viewer can put the marker
	// where the task's last line was. Empty when the swarm gave no time.
	At string `json:"at,omitempty"`
}

// nodeInfo is a node a followed task runs on, as the swarm last described it.
type nodeInfo struct {
	ID, Name, State string
}

// nodeNotice says a node that runs a followed task cannot be reached (down or disconnected),
// or can again (back).
type nodeNotice struct {
	Kind string `json:"kind"`
	Node string `json:"node"`
	At   string `json:"at"`
}

// markerState says whether a task state is one that ends a slot's task in a way the log marks.
// A task that completed (a job's) is not replaced, and one on a node that cannot vouch for it
// reads unknown, so neither is a marker.
func markerState(state string) bool {
	switch state {
	case "failed", "shutdown", "rejected":
		return true
	}
	return false
}

// markerGroup is the key tasks are paired by: the slot of a replicated service's task, the
// node of a global service's. In a stack's log, which holds several services, the service comes
// first, so two services' slot 1 are never paired.
func markerGroup(ti taskInfo) string {
	if ti.Slot > 0 {
		return ti.Service + "/slot:" + strconv.Itoa(ti.Slot)
	}
	return ti.Service + "/node:" + ti.NodeID
}

// markerKey names a marker, so one that was told is not told again; a stopped marker that
// becomes a replacement is a new key.
func markerKey(m slotMarker) string { return m.Old + "\x00" + m.New }

// markersOf returns every marker the tasks show, oldest first by the time the old task ended.
func markersOf(tasks []taskInfo) []slotMarker {
	groups := map[string][]taskInfo{}
	for _, ti := range tasks {
		k := markerGroup(ti)
		groups[k] = append(groups[k], ti)
	}
	var out []slotMarker
	for _, g := range groups {
		slices.SortFunc(g, func(a, b taskInfo) int {
			return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
		})
		for i, old := range g {
			if !old.Ended || !markerState(old.State) {
				continue
			}
			m := slotMarker{Kind: markerStopped, Service: old.Service, Slot: old.Slot, Old: old.ID, OldState: old.State, OldNode: old.Node, ExitCode: old.ExitCode}
			if i+1 < len(g) {
				next := g[i+1]
				m.Kind, m.New, m.Node = markerReplaced, next.ID, next.Node
			}
			if when := cmp.Or(old.Since, old.Created); !when.IsZero() {
				m.At = when.UTC().Format(logTimeLayout)
			}
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b slotMarker) int {
		return cmp.Or(cmp.Compare(a.At, b.At), cmp.Compare(a.Old, b.Old))
	})
	return out
}

// slotMarkers returns the markers current has that previous did not: a slot's old task turned
// failed, shutdown or rejected and a newer task is in its slot (replaced), or no newer task
// exists yet (stopped). A global service's tasks are paired by node. With nothing before it,
// it returns every marker the tasks show.
func slotMarkers(previous, current []taskInfo) []slotMarker {
	told := map[string]bool{}
	for _, m := range markersOf(previous) {
		told[markerKey(m)] = true
	}
	var out []slotMarker
	for _, m := range markersOf(current) {
		if !told[markerKey(m)] {
			out = append(out, m)
		}
	}
	return out
}

// notReachable says whether a node state is one in which the node's tasks cannot send logs.
func notReachable(state string) bool {
	return state == nodeStateDown || state == nodeStateDisconnected
}

// nodeNotices returns the changes in whether nodes can be reached. A node previous did not hold
// counts as having been ready, so a node that is down when the stream opens is told once. A
// node the swarm does not describe, or describes as unknown, says nothing.
func nodeNotices(previous, current []nodeInfo, at string) []nodeNotice {
	before := make(map[string]string, len(previous))
	for _, n := range previous {
		before[n.ID] = n.State
	}
	var out []nodeNotice
	for _, n := range current {
		was := before[n.ID]
		switch {
		case notReachable(n.State) && was != n.State:
			out = append(out, nodeNotice{Kind: n.State, Node: n.Name, At: at})
		case n.State == nodeStateReady && notReachable(was):
			out = append(out, nodeNotice{Kind: noticeBack, Node: n.Name, At: at})
		}
	}
	slices.SortFunc(out, func(a, b nodeNotice) int { return cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.Kind, b.Kind)) })
	return out
}
