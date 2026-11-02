// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { NodeItem } from "@/api/nodes";
import { MAX_QUERY, type SortDir } from "@/features/containers/list-search";
import { z } from "@/lib/zod-config";

// The status chips, in the order they show and in the order a comma list is written, so the same
// choice is always the same link. The server's own names for them (status= on GET /nodes).
export const NODE_STATUS_VALUES = ["managers", "workers", "drained", "down"] as const;
export type NodeStatusValue = (typeof NODE_STATUS_VALUES)[number];

export const NODE_SORT_VALUES = ["name", "status", "role", "tasks"] as const;
export type NodeSortKey = (typeof NODE_SORT_VALUES)[number];

export type { SortDir };

// nodeStatusList reads a comma list into the known statuses, once each, in chip order. A word that
// is not a status is dropped rather than sent, since the server answers an unknown status with a
// refusal.
export function nodeStatusList(value: string | undefined): NodeStatusValue[] {
  if (!value) return [];
  const wanted = new Set(value.split(",").map((s) => s.trim().toLowerCase()));
  return NODE_STATUS_VALUES.filter((s) => wanted.has(s));
}

// nodeStatusParam is the comma list written back, or undefined when nothing is chosen, so an empty
// choice leaves the URL clean.
export function nodeStatusParam(values: readonly NodeStatusValue[]): string | undefined {
  const chosen = NODE_STATUS_VALUES.filter((s) => values.includes(s));
  return chosen.length > 0 ? chosen.join(",") : undefined;
}

// The nodes route's search (UI-SPEC "Nodes"): the search text, the status chips, and how the rows
// are sorted. Every value that does not fit is dropped, not sent. With no sort the rows keep the
// server's order, host name then ID.
export const nodeSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  status: z
    .string()
    .max(64)
    .transform((s) => nodeStatusParam(nodeStatusList(s)))
    .optional()
    .catch(undefined),
  sort: z.enum(NODE_SORT_VALUES).optional().catch(undefined),
  dir: z.enum(["asc", "desc"]).optional().catch(undefined),
});

export type NodeSearch = z.output<typeof nodeSearchSchema>;

// A change to the search: a key set to undefined is taken out of the URL.
export type NodeSearchPatch = {
  [K in keyof NodeSearch]?: NodeSearch[K] | undefined;
};

// The order the status column sorts in: ready nodes first, then the ones the swarm cannot name,
// then disconnected, then down.
const STATE_RANK: Record<string, number> = { ready: 0, unknown: 1, disconnected: 2, down: 3 };

type SortableNode = Pick<NodeItem, "id" | "hostname" | "role" | "state" | "tasksRunning">;

function compareBy(key: NodeSortKey) {
  return (a: SortableNode, b: SortableNode): number => {
    let primary = 0;
    if (key === "status") primary = (STATE_RANK[a.state] ?? 1) - (STATE_RANK[b.state] ?? 1);
    else if (key === "role") primary = a.role === b.role ? 0 : a.role === "manager" ? -1 : 1;
    else if (key === "tasks") primary = a.tasksRunning - b.tasksRunning;
    if (primary !== 0) return primary;
    // Ties, and the name sort itself, go by host name then ID: the server's order.
    if (a.hostname !== b.hostname) return a.hostname < b.hostname ? -1 : 1;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  };
}

// sortNodes returns the rows in the order the list shows them. Without a sort the rows keep the
// order the server sent, host name then ID (RES-05); a descending sort reverses the ascending one.
export function sortNodes<T extends SortableNode>(
  rows: readonly T[],
  key: NodeSortKey | undefined,
  dir: SortDir | undefined,
): T[] {
  if (key === undefined && dir !== "desc") return [...rows];
  const out = [...rows].sort(compareBy(key ?? "name"));
  return dir === "desc" ? out.reverse() : out;
}
