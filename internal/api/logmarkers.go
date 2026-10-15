// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

// The marks a service's log shows between its lines (plan 06-30, OBS-02, D-34, D-37): a task
// was replaced, a slot is waiting for a task, a node cannot be reached.

const (
	markerReplaced = "replaced"
	markerStopped  = "stopped"

	noticeDown         = "down"
	noticeDisconnected = "disconnected"
	noticeBack         = "back"
)

// slotMarker says what happened to the task that last ran in a slot. A replaced marker names
// the task that took over and the node it starts on; a stopped one has no successor yet.
type slotMarker struct {
	Kind string `json:"kind"`
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

// slotMarkers returns the markers current has that previous did not: a slot's old task turned
// failed, shutdown or rejected and a newer task is in its slot (replaced), or no newer task
// exists yet (stopped). A global service's tasks are paired by node. With nothing before it,
// it returns every marker the tasks show.
func slotMarkers(previous, current []taskInfo) []slotMarker {
	return nil
}

// nodeNotices returns the changes in whether nodes can be reached. A node previous did not hold
// counts as having been ready, so a node that is down when the stream opens is told once.
func nodeNotices(previous, current []nodeInfo, at string) []nodeNotice {
	return nil
}
