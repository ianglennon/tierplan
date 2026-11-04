// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

export type SearchBody = components["schemas"]["SearchBody"];
export type SearchResult = components["schemas"]["SearchResult"];
export type SearchCounts = components["schemas"]["SearchCounts"];
export type SearchType = SearchResult["type"];
export type SearchTypeFilter = "all" | SearchType;
// page answers with up to 200 rows, palette with up to 5 of each type. The counts are the full totals.
export type SearchLimit = "page" | "palette";

// The server takes a trimmed query of 2 to 100 characters (422 otherwise). The palette and the
// results page keep to the same bounds, so the request is never one it would refuse.
export const SEARCH_MIN = 2;
export const SEARCH_MAX = 100;

// The kinds of thing search finds, in the order the palette and the page list them.
export const SEARCH_TYPES: readonly SearchType[] = [
  "stack",
  "service",
  "container",
  "image",
  "network",
];

// Characters, not UTF-16 units: a name with an emoji in it is one character to the server.
function codePoints(s: string): string[] {
  return Array.from(s);
}

// clampQuery cuts typed or pasted text to 100 characters. It never cuts a character in half.
export function clampQuery(value: string): string {
  const chars = codePoints(value);
  return chars.length > SEARCH_MAX ? chars.slice(0, SEARCH_MAX).join("") : value;
}

// searchable says whether a trimmed query is long enough to ask the server about.
export function searchable(trimmed: string): boolean {
  const n = codePoints(trimmed).length;
  return n >= SEARCH_MIN && n <= SEARCH_MAX;
}

// Every search hangs under this key.
export const SEARCH_KEY = ["search"] as const;

// A live fan-out is cheap for the server (it keeps a snapshot for a few seconds), and a search the
// person repeats a moment later is the same answer.
const SEARCH_STALE_MS = 5_000;

export function searchQuery(
  q: string,
  limit: SearchLimit,
  type: SearchTypeFilter = "all",
  env: readonly string[] = [],
) {
  const trimmed = q.trim();
  return queryOptions({
    queryKey: [...SEARCH_KEY, limit, type, [...env].sort(), trimmed],
    queryFn: async ({ signal }): Promise<SearchBody> => {
      const { data } = await api.GET("/api/search", {
        params: {
          query: {
            q: trimmed,
            limit,
            ...(type === "all" ? {} : { type }),
            ...(env.length > 0 ? { env: [...env] } : {}),
          },
        },
        signal,
      });
      if (!data) throw new Error("GET /api/search failed");
      return data;
    },
    staleTime: SEARCH_STALE_MS,
    // The person sees the failure with a "Retry search" button; a silent second try would only
    // keep the spinner up longer.
    retry: false,
  });
}

// useSearch asks the server for q, once the trimmed text is 2 to 100 characters. The caller
// debounces the typing (200 ms); this only decides whether the text is worth a request.
export function useSearch(
  q: string,
  limit: SearchLimit,
  type: SearchTypeFilter = "all",
  env: readonly string[] = [],
) {
  return useQuery({ ...searchQuery(q, limit, type, env), enabled: searchable(q.trim()) });
}
