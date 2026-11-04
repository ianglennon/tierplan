// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { clampQuery, SEARCH_TYPES, type SearchType } from "@/api/search";
import { envIds } from "@/features/fleet/fleet-search";
import { z } from "@/lib/zod-config";

// The results page's address (UI-SPEC "Routes"): the query, the type chip and the environments
// live in the URL, so a search is a link. Nothing here is kept in browser storage.
export interface SearchPageSearch {
  q?: string;
  type?: SearchType;
  env?: string[];
}

// parseSearchPage keeps the values it knows and drops the rest, so a stale or hand-edited link
// shows the default view and not an error. q is trimmed and cut to 100 characters; type is one of
// the five kinds ("all" is the default and is left out); env is a list of environment IDs (UUIDs).
// Every key is given, undefined where it was dropped: the router keeps what the validator does not
// mention, so a dropped parameter has to be overwritten to be gone.
export function parseSearchPage(search: Record<string, unknown>): SearchPageSearch {
  // The router reads ?q=123 as a number and ?q=true as a boolean; the person typed text.
  const raw =
    typeof search.q === "string"
      ? search.q
      : typeof search.q === "number" || typeof search.q === "boolean"
        ? String(search.q)
        : "";
  const q = clampQuery(raw.trim()).trim();
  return {
    q: q === "" ? undefined : q,
    type: SEARCH_TYPES.find((t) => t === search.type),
    env: envIds(search.env),
  };
}

export const searchPageSchema = z
  .object({ q: z.unknown(), type: z.unknown(), env: z.unknown() })
  .partial()
  .catch({})
  .transform((search): SearchPageSearch => parseSearchPage(search));
