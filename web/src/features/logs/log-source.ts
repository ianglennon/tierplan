// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// What a log viewer reads (plan 06-29, D-35): one container, on the node it is on, one
// swarm service, whose tasks the server merges into a single stream, or a whole stack (plan
// 07-29, D-22), whose services or containers the server merges into one. A stack's id is its
// stack ID and its target says whether the lines are tasks of a swarm or containers of a host.
export type StackTarget = "swarm" | "standalone";
export type LogSource =
  | { kind: "container"; id: string; nodeId?: string }
  | { kind: "service"; id: string }
  | { kind: "stack"; id: string; target: StackTarget; unmanaged?: true };

// The path segment a source's routes are under: a stack Coaming did not make is read under its name and has its own
// routes (plan 08-09), but is the same merged log.
export function logRoute(src: LogSource): string {
  if (src.kind === "stack") return src.unmanaged ? "unmanaged-stacks" : "stacks";
  return src.kind === "service" ? "services" : "containers";
}

export const containerSource = (id: string, nodeId?: string): LogSource => ({
  kind: "container",
  id,
  ...(nodeId ? { nodeId } : {}),
});

export const serviceSource = (id: string): LogSource => ({ kind: "service", id });

export const stackSource = (id: string, target: StackTarget): LogSource => ({
  kind: "stack",
  id,
  target,
});

// unmanagedStackSource is the log of a stack Coaming did not make, which is its name and not an ID.
export const unmanagedStackSource = (
  name: string,
  target: StackTarget,
): Extract<LogSource, { kind: "stack" }> => ({
  kind: "stack",
  id: name,
  target,
  unmanaged: true,
});

// How a service's log is narrowed on the server: to these tasks (an ID, or the start of
// one of at least eight characters) and these nodes (a name or an ID), and whether it
// takes in the tasks that had already ended when the stream opened. Empty means all.
//
// A stack's log is narrowed further to these services (their names in the compose file) and,
// on a standalone host, these containers (an ID, the start of one, or a name).
export interface ServiceLogFilter {
  tasks?: readonly string[];
  nodes?: readonly string[];
  stopped?: boolean;
  services?: readonly string[];
  containers?: readonly string[];
}

// One task of the service as the ready and tasks events list it. A stack's log adds the service
// the task belongs to; on a standalone host a "task" is a container, and its slot is its number.
export interface ServiceLogTask {
  id: string;
  slot: number;
  node: string;
  state: string;
  service?: string;
}

// The stack has more tasks or containers than the stream follows at once: the cap event of a
// stack's stream (plan 07-29, D-22).
export interface StackCapEvent {
  count: number;
  cap: number;
  unit: "tasks" | "containers";
}

// A slot's task ended and (replaced) another took over, or (stopped) none has yet: the
// marker event of a service's stream (plan 06-30, D-34). at is when the old task ended, in
// the log's time layout, so the viewer puts it where that task's last line is.
export interface ServiceMarkerEvent {
  kind: "replaced" | "stopped";
  // A stack's log merges several services; the marker names the one it is about. unit is
  // "container" for a container of a standalone stack that a deploy replaced.
  service?: string;
  unit?: "container";
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

// The states after which a task writes nothing more: Docker's own, as a task row gives them,
// and the ones of a container that has ended.
const ENDED_TASK_STATES: ReadonlySet<string> = new Set([
  "complete",
  "failed",
  "shutdown",
  "rejected",
  "remove",
  "orphaned",
  "exited",
  "dead",
]);

export const isEndedTaskState = (state: string): boolean => ENDED_TASK_STATES.has(state);
