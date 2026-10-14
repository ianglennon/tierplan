// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { MAX_QUERY, type SortDir } from "@/features/containers/list-search";
import { z } from "@/lib/zod-config";

export const NODE_STATUS_VALUES = ["managers", "workers", "drained", "down"] as const;
export type NodeStatusValue = (typeof NODE_STATUS_VALUES)[number];

export const NODE_SORT_VALUES = ["name", "status", "role", "tasks"] as const;
export type NodeSortKey = (typeof NODE_SORT_VALUES)[number];

export type { SortDir };

export function nodeStatusList(_value: string | undefined): NodeStatusValue[] {
  return [];
}

export function nodeStatusParam(_values: readonly NodeStatusValue[]): string | undefined {
  return undefined;
}

export const nodeSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  status: z.string().optional().catch(undefined),
  sort: z.enum(NODE_SORT_VALUES).optional().catch(undefined),
  dir: z.enum(["asc", "desc"]).optional().catch(undefined),
});

export type NodeSearch = z.output<typeof nodeSearchSchema>;

export type NodeSearchPatch = {
  [K in keyof NodeSearch]?: NodeSearch[K] | undefined;
};
