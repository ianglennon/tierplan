// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { MAX_QUERY, type SortDir } from "@/features/containers/list-search";
import { z } from "@/lib/zod-config";

// The status chips, in the order they show and in the order a comma list is written, so the same
// choice is always the same link. They are the server's own names for status= on the stack lists.
export const STACK_STATUS_VALUES = [
  "degraded",
  "stopped",
  "draft",
  "deploy_failed",
  "drifted",
  "behind",
  "sync-error",
] as const;
export type StackStatusValue = (typeof STACK_STATUS_VALUES)[number];

// The Show toggle-group: only the stacks Bayplan manages or only the ones it found. Absent means both.
export const STACK_KIND_VALUES = ["managed", "unmanaged"] as const;
export type StackKindValue = (typeof STACK_KIND_VALUES)[number];

export const STACK_SORT_VALUES = ["name", "state", "lastDeployed"] as const;
export type StackSortKey = (typeof STACK_SORT_VALUES)[number];

export const STACK_GROUP_VALUES = ["env", "none"] as const;
export type StackGroupBy = (typeof STACK_GROUP_VALUES)[number];

export type { SortDir };

// stackStatusList reads a comma list into the known statuses, once each, in chip order. A word that is
// not a status is dropped rather than sent, since the server answers an unknown status with a refusal.
export function stackStatusList(value: string | undefined): StackStatusValue[] {
  if (!value) return [];
  const wanted = new Set(value.split(",").map((s) => s.trim().toLowerCase()));
  return STACK_STATUS_VALUES.filter((s) => wanted.has(s));
}

// stackStatusParam is the comma list written back, or undefined when nothing is chosen, so an empty
// choice leaves the URL clean.
export function stackStatusParam(values: readonly StackStatusValue[]): string | undefined {
  const chosen = STACK_STATUS_VALUES.filter((s) => values.includes(s));
  return chosen.length > 0 ? chosen.join(",") : undefined;
}

// A comma list of environment IDs, once each. The IDs are whatever the URL holds: the server ignores
// one it does not know or the caller cannot read, so nothing here needs to know them.
export function envList(value: string | undefined): string[] {
  if (!value) return [];
  return [...new Set(value.split(",").map((s) => s.trim()))].filter(Boolean);
}

export function envParam(values: readonly string[]): string | undefined {
  const chosen = [...new Set(values)].filter(Boolean);
  return chosen.length > 0 ? chosen.join(",") : undefined;
}

// The search both stack lists share: the search text, the status chips, the kind, the sort, and new=1, which
// opens the Create stack dialog. A value that does not fit is dropped, not sent.
const shared = {
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  status: z
    .string()
    .max(64)
    .transform((s) => stackStatusParam(stackStatusList(s)))
    .optional()
    .catch(undefined),
  kind: z.enum(STACK_KIND_VALUES).optional().catch(undefined),
  sort: z.enum(STACK_SORT_VALUES).optional().catch(undefined),
  dir: z.enum(["asc", "desc"]).optional().catch(undefined),
  new: z.literal(1).optional().catch(undefined),
};

export const stackListSearchSchema = z.object(shared);
export type StackListSearch = z.output<typeof stackListSearchSchema>;

// The All stacks list adds the environments chosen and how the rows are grouped (D-21): env is a comma
// list of IDs, and a missing group means the default, Environment.
export const globalStackSearchSchema = z.object({
  ...shared,
  env: z
    .string()
    .max(2048)
    .transform((s) => envParam(envList(s)))
    .optional()
    .catch(undefined),
  group: z.enum(STACK_GROUP_VALUES).optional().catch(undefined),
});
export type GlobalStackSearch = z.output<typeof globalStackSearchSchema>;

// A change to the search: a key set to undefined is taken out of the URL.
export type StackListSearchPatch = {
  [K in keyof StackListSearch]?: StackListSearch[K] | undefined;
};
export type GlobalStackSearchPatch = {
  [K in keyof GlobalStackSearch]?: GlobalStackSearch[K] | undefined;
};
