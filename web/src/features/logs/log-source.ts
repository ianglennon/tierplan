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
