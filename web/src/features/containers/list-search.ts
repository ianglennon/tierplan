// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { z } from "@/lib/zod-config";

// The status chips, in the order they show and in the order a comma list is
// written, so the same choice is always the same link (D-02).
export const STATUS_VALUES = ["running", "exited", "paused", "restarting", "unhealthy"] as const;
export type StatusValue = (typeof STATUS_VALUES)[number];

export const GROUP_VALUES = ["stack", "node", "none"] as const;
export type GroupBy = (typeof GROUP_VALUES)[number];

export const SORT_VALUES = ["name", "state", "created"] as const;
export type SortKey = (typeof SORT_VALUES)[number];

export type SortDir = "asc" | "desc";

// What the server accepts as a search: 128 characters (T-04-31).
export const MAX_QUERY = 128;

// statusList reads a comma list into the known statuses, once each, in chip
// order. A word that is not a status is dropped rather than sent, since the
// server answers an unknown status with a refusal (T-04-31).
export function statusList(value: string | undefined): StatusValue[] {
  if (!value) return [];
  const wanted = new Set(value.split(",").map((s) => s.trim().toLowerCase()));
  return STATUS_VALUES.filter((s) => wanted.has(s));
}

// statusParam is the comma list written back, or undefined when nothing is
// chosen, so an empty choice leaves the URL clean.
export function statusParam(values: readonly StatusValue[]): string | undefined {
  const chosen = STATUS_VALUES.filter((s) => values.includes(s));
  return chosen.length > 0 ? chosen.join(",") : undefined;
}

// The containers route's search (UI-SPEC "Screens"): the node, the search
// text, the status chips, how the rows are grouped and sorted. Every value
// that does not fit is dropped, not sent, and a missing group means the
// default, stack or project.
export const containerSearchSchema = nodeSearchSchema.extend({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  status: z
    .string()
    .max(64)
    .transform((s) => statusParam(statusList(s)))
    .optional()
    .catch(undefined),
  group: z.enum(GROUP_VALUES).optional().catch(undefined),
  sort: z.enum(SORT_VALUES).optional().catch(undefined),
  dir: z.enum(["asc", "desc"]).optional().catch(undefined),
  // The live-stats switch: stats=1 adds the CPU, Memory and Network columns.
  stats: z.literal(1).optional().catch(undefined),
});

export type ContainerSearch = z.output<typeof containerSearchSchema>;

// A change to the search: a key set to undefined is taken out of the URL.
export type ContainerSearchPatch = {
  [K in keyof ContainerSearch]?: ContainerSearch[K] | undefined;
};
