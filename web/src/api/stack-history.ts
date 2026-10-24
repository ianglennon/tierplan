// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import { isStackGone, StacksLoadError, stackKey } from "@/api/stacks";

export type StackRevision = components["schemas"]["StackRevisionItem"];
export type StackRevisionList = components["schemas"]["StackRevisionListBody"];
export type StackRevisionImageChange = components["schemas"]["StackRevisionImageChange"];

// The query key of a stack's revisions. It extends the stack's key, so the events feed that refreshes the
// stacks list for any event carrying the stack's label, and every action that refreshes the stack's page,
// refresh the history too.
export function revisionsKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "revisions"] as const;
}

// The revisions of a stack, newest first. It is read again every 30 s while the page is visible and not at
// all while it is hidden. A stack that is gone is not retried: it will not come back on a second try.
export function revisionsQuery(envId: string, stackId: string) {
  return queryOptions({
    queryKey: revisionsKey(envId, stackId),
    queryFn: async (): Promise<StackRevisionList> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/revisions", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 3,
    refetchInterval: () => (document.visibilityState === "visible" ? LIST_REFETCH_MS : false),
  });
}
