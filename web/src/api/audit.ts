// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { type AuditSearch, DEFAULT_RANGE, rangeFor } from "@/features/audit/filters";

export type AuditEntry = components["schemas"]["AuditEntryDTO"];

// Entries per page: the server default, stated here so the screen and its
// copy agree (UI-SPEC: 50 at a time).
export const AUDIT_PAGE_SIZE = 50;

// The user filter's suggestions: the actor names the log has seen.
export const auditActorsQuery = queryOptions({
  queryKey: ["audit", "actors"],
  queryFn: async () => {
    const { data } = await api.GET("/api/audit/actors");
    if (data === undefined) throw new Error("GET /api/audit/actors failed");
    return data ?? [];
  },
  staleTime: 60_000,
});

// The list is keyed by the filters, not by the time they were read at: a
// preset such as "Last hour" is turned into a from time inside the query
// function, so one view is one cache entry however long it stays open. A page
// that fails shows the inline error with Retry, so there is no automatic retry.
export function auditListQuery(search: AuditSearch) {
  return infiniteQueryOptions({
    queryKey: [
      "audit",
      "list",
      search.user ?? null,
      search.action ?? null,
      search.range ?? DEFAULT_RANGE,
      search.from ?? null,
      search.to ?? null,
    ],
    initialPageParam: undefined as number | undefined,
    queryFn: async ({ pageParam }) => {
      const { from, to } = rangeFor(search, new Date());
      const { data } = await api.GET("/api/audit", {
        params: {
          query: {
            user: search.user,
            action: search.action,
            from,
            to,
            cursor: pageParam,
          },
        },
      });
      if (!data) throw new Error("GET /api/audit failed");
      return data;
    },
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    retry: false,
    staleTime: 30_000,
  });
}
