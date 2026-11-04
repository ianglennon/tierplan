// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { SORT_KEYS, type SortDir, type SortKey } from "@/features/environments/sort";
import { z } from "@/lib/zod-config";

// The Overview's address (UI-SPEC "Screens and Layout Contract"): the tab, the environments the
// fleet tabs are narrowed to and the Updates tab's Show chip live in the URL, so a view is a link.
// Nothing here is kept in browser storage.

export const FLEET_TABS = ["environments", "failing", "updates", "pools"] as const;
export type FleetTab = (typeof FLEET_TABS)[number];

export const FLEET_SHOWS = ["updates", "problems", "ignored", "all"] as const;
export type FleetShow = (typeof FLEET_SHOWS)[number];

export interface FleetSearch {
  tab: FleetTab;
  env?: string[];
  show?: Exclude<FleetShow, "updates">;
  sort?: SortKey;
  dir?: SortDir;
}

// An environment ID is a UUID. Anything else in the address is somebody's typing and is dropped.
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
// The server refuses more than this many (T-12-79), so the address does not carry more.
export const MAX_ENV_FILTER = 100;

function envIds(value: unknown): string[] | undefined {
  const raw = Array.isArray(value) ? value : value === undefined ? [] : [value];
  const ids: string[] = [];
  for (const v of raw) {
    if (typeof v !== "string" || !UUID.test(v) || ids.includes(v)) continue;
    ids.push(v);
    if (ids.length === MAX_ENV_FILTER) break;
  }
  return ids.length > 0 ? ids : undefined;
}

// parseFleetSearch keeps the values it knows. The tab is always present (the Environments tab by
// default), so a link to a tab can tell which one is showing. A parameter that does not belong to
// the tab is dropped: the sort to the Environments tab, the Show chip to the Updates tab. The
// default Show value is left out, as is the Environments tab's unsorted order.
export function parseFleetSearch(search: Record<string, unknown>): FleetSearch {
  const tab = FLEET_TABS.find((t) => t === search.tab) ?? "environments";
  const show = tab === "updates" ? FLEET_SHOWS.find((s) => s === search.show) : undefined;
  const sort = tab === "environments" ? SORT_KEYS.find((key) => key === search.sort) : undefined;
  const dir =
    tab === "environments" && (search.dir === "asc" || search.dir === "desc")
      ? search.dir
      : undefined;
  // Every key is given, undefined where it was dropped: the router keeps what the validator does
  // not mention, so a dropped parameter has to be overwritten to be gone.
  return {
    tab,
    env: envIds(search.env),
    show: show === "updates" ? undefined : show,
    sort,
    dir,
  };
}

// fleetSearchSchema is parseFleetSearch as a validator for the route. What a link may give is any
// of the parameters, all optional; what the page gets back is a FleetSearch, with the tab always
// there.
export const fleetSearchSchema = z
  .object({
    tab: z.unknown(),
    env: z.unknown(),
    show: z.unknown(),
    sort: z.unknown(),
    dir: z.unknown(),
  })
  .partial()
  .catch({})
  .transform((search): FleetSearch => parseFleetSearch(search));
