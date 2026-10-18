// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// What a log viewer reads (plan 06-29, D-35): one container, on the node it is on, or one
// swarm service, whose tasks the server merges into a single stream.
export type LogSource =
  | { kind: "container"; id: string; nodeId?: string }
  | { kind: "service"; id: string };

export const containerSource = (id: string, nodeId?: string): LogSource => ({
  kind: "container",
  id,
  ...(nodeId ? { nodeId } : {}),
});

export const serviceSource = (id: string): LogSource => ({ kind: "service", id });

// How a service's log is narrowed on the server: to these tasks (an ID, or the start of
// one of at least eight characters) and these nodes (a name or an ID), and whether it
// takes in the tasks that had already ended when the stream opened. Empty means all.
export interface ServiceLogFilter {
  tasks?: readonly string[];
  nodes?: readonly string[];
  stopped?: boolean;
}

// One task of the service as the ready and tasks events list it.
export interface ServiceLogTask {
  id: string;
  slot: number;
  node: string;
  state: string;
}

// A slot's task ended and (replaced) another took over, or (stopped) none has yet: the
// marker event of a service's stream (plan 06-30, D-34). at is when the old task ended, in
// the log's time layout, so the viewer puts it where that task's last line is.
export interface ServiceMarkerEvent {
  kind: "replaced" | "stopped";
  // The replica number; 0 for a task of a global service.
  slot: number;
  old: string;
  oldState: string;
  oldNode: string;
  exitCode?: number;
  new?: string;
  node?: string;
  at?: string;
}

// A node that runs a followed task cannot be reached, or can again: the node event (D-37).
export interface ServiceNodeEvent {
  kind: "down" | "disconnected" | "back";
  node: string;
  at?: string;
}

// What a marker row in the buffer says.
export type SwarmMark = Omit<ServiceMarkerEvent, "at"> | Omit<ServiceNodeEvent, "at">;

export const isSlotMark = (m: SwarmMark): m is Omit<ServiceMarkerEvent, "at"> =>
  m.kind === "replaced" || m.kind === "stopped";

// The states after which a task writes nothing more: Docker's own, as a task row gives them.
const ENDED_TASK_STATES: ReadonlySet<string> = new Set([
  "complete",
  "failed",
  "shutdown",
  "rejected",
  "remove",
  "orphaned",
]);

export const isEndedTaskState = (state: string): boolean => ENDED_TASK_STATES.has(state);
