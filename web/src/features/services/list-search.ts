// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { MAX_QUERY, type SortDir, UPDATES_VALUES } from "@/features/containers/list-search";
import { z } from "@/lib/zod-config";

// The status chips, in the order they show and in the order a comma list is written, so the
// same choice is always the same link. The server's own names for them (status= on
// GET /services): Update problem is "problem" and Restart loop is "loop".
export const SERVICE_STATUS_VALUES = ["degraded", "updating", "problem", "loop"] as const;
export type ServiceStatusValue = (typeof SERVICE_STATUS_VALUES)[number];

export const SERVICE_GROUP_VALUES = ["stack", "none"] as const;
export type ServiceGroupBy = (typeof SERVICE_GROUP_VALUES)[number];

export const SERVICE_SORT_VALUES = ["name", "state", "updated"] as const;
export type ServiceSortKey = (typeof SERVICE_SORT_VALUES)[number];

export type { SortDir };

// serviceStatusList reads a comma list into the known statuses, once each, in chip order. A
// word that is not a status is dropped rather than sent, since the server answers an unknown
// status with a refusal.
export function serviceStatusList(value: string | undefined): ServiceStatusValue[] {
  if (!value) return [];
  const wanted = new Set(value.split(",").map((s) => s.trim().toLowerCase()));
  return SERVICE_STATUS_VALUES.filter((s) => wanted.has(s));
}

// serviceStatusParam is the comma list written back, or undefined when nothing is chosen, so
// an empty choice leaves the URL clean.
export function serviceStatusParam(values: readonly ServiceStatusValue[]): string | undefined {
  const chosen = SERVICE_STATUS_VALUES.filter((s) => values.includes(s));
  return chosen.length > 0 ? chosen.join(",") : undefined;
}

// The services route's search (UI-SPEC "Services list"): the search text, the status chips,
// how the rows are grouped and sorted. Every value that does not fit is dropped, not sent,
// and a missing group means the default, stack.
export const serviceSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  status: z
    .string()
    .max(64)
    .transform((s) => serviceStatusParam(serviceStatusList(s)))
    .optional()
    .catch(undefined),
  group: z.enum(SERVICE_GROUP_VALUES).optional().catch(undefined),
  sort: z.enum(SERVICE_SORT_VALUES).optional().catch(undefined),
  dir: z.enum(["asc", "desc"]).optional().catch(undefined),
  // The Updates available toggle: updates=available, or nothing. Anything else is dropped (T-12-24).
  updates: z.enum(UPDATES_VALUES).optional().catch(undefined),
  // The live-stats switch: stats=1 adds the CPU, Memory and Network columns.
  stats: z.literal(1).optional().catch(undefined),
});

export type ServiceSearch = z.output<typeof serviceSearchSchema>;

// A change to the search: a key set to undefined is taken out of the URL.
export type ServiceSearchPatch = {
  [K in keyof ServiceSearch]?: ServiceSearch[K] | undefined;
};
