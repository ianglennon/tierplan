// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { components } from "@/api/schema";

export type PlacementData = components["schemas"]["PlacementBody"];
export type PlacementNode = components["schemas"]["PlacementNode"];
export type PlacementTask = components["schemas"]["PlacementTask"];
export type PlacementService = components["schemas"]["PlacementService"];

// The value of the stack filter for the services that belong to no stack. A stack that is
// really called "none" cannot be filtered to on its own; the token wins.
export const NO_STACK = "none";

export interface PlacementFilters {
  // A stack name, or NO_STACK; left out for all stacks.
  stack?: string;
  // A case-insensitive part of a service name.
  q?: string;
}

export type ColumnKind = "notPlaced" | "node" | "removed";

export interface StackGroup {
  // The stack's name; empty for the tasks of services that belong to no stack.
  stack: string;
  tasks: PlacementTask[];
}

export interface PlacementColumn {
  key: string;
  kind: ColumnKind;
  // The node of a "node" column.
  node?: PlacementNode;
  groups: StackGroup[];
  count: number;
}

// Names sort the way a person reads them: without regard to case, and wrk-2 before wrk-10.
const names = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

const byService = (a: PlacementTask, b: PlacementTask) =>
  names.compare(a.service, b.service) || a.slot - b.slot || a.id.localeCompare(b.id);

// isFiltered says whether the filters narrow the tasks at all.
export function isFiltered(filters: PlacementFilters): boolean {
  return Boolean(filters.stack) || Boolean(filters.q?.trim());
}

// stackNames are the stacks the services belong to, A to Z.
export function stackNames(data: PlacementData): string[] {
  const found = new Set<string>();
  for (const s of data.services ?? []) if (s.stack) found.add(s.stack);
  for (const t of data.tasks ?? []) if (t.stack) found.add(t.stack);
  return [...found].sort(names.compare);
}

function groupByStack(tasks: PlacementTask[]): StackGroup[] {
  const byStack = new Map<string, PlacementTask[]>();
  for (const t of tasks) {
    const list = byStack.get(t.stack);
    if (list) list.push(t);
    else byStack.set(t.stack, [t]);
  }
  // Stacks A to Z; the tasks of services in no stack come last.
  return [...byStack.entries()]
    .sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : names.compare(a, b)))
    .map(([stack, list]) => ({ stack, tasks: list.sort(byService) }));
}

function column(
  key: string,
  kind: ColumnKind,
  tasks: PlacementTask[],
  node?: PlacementNode,
): PlacementColumn {
  return { key, kind, node, groups: groupByStack(tasks), count: tasks.length };
}

// project turns the answer into the columns of the By node layout: Not placed first when it
// holds tasks, one column per node in node-name order whether or not it holds any (so an idle
// node and a down node are seen), and Removed node last when it holds tasks. The filters
// narrow the tasks and never the nodes.
export function project(data: PlacementData, filters: PlacementFilters): PlacementColumn[] {
  const q = filters.q?.trim().toLowerCase();
  const kept = (data.tasks ?? []).filter((t) => {
    if (filters.stack === NO_STACK) {
      if (t.stack !== "") return false;
    } else if (filters.stack && t.stack !== filters.stack) {
      return false;
    }
    return !q || t.service.toLowerCase().includes(q);
  });

  const allNodes = data.nodes ?? [];
  const known = new Set(allNodes.map((n) => n.id));
  const notPlaced: PlacementTask[] = [];
  const removed: PlacementTask[] = [];
  const onNode = new Map<string, PlacementTask[]>();
  for (const t of kept) {
    if (t.nodeId === "") notPlaced.push(t);
    else if (!t.nodeKnown || !known.has(t.nodeId)) removed.push(t);
    else onNode.set(t.nodeId, [...(onNode.get(t.nodeId) ?? []), t]);
  }

  const nodes = [...allNodes].sort(
    (a, b) => names.compare(a.hostname, b.hostname) || a.id.localeCompare(b.id),
  );
  const out: PlacementColumn[] = [];
  if (notPlaced.length > 0) out.push(column("not-placed", "notPlaced", notPlaced));
  for (const n of nodes) out.push(column(`node:${n.id}`, "node", onNode.get(n.id) ?? [], n));
  if (removed.length > 0) out.push(column("removed", "removed", removed));
  return out;
}
