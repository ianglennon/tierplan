// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type Problem, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type StackHistorySettings = components["schemas"]["StackHistorySettingsBody"];
export type StackHistoryPrunePreview = components["schemas"]["StackHistoryPreviewBody"];

// How many revisions of each stack TierPlan keeps (D-01). Read with settings.read, changed with
// settings.write.
export const stackHistorySettingsQuery = queryOptions({
  queryKey: ["settings", "stack-history"],
  queryFn: async (): Promise<StackHistorySettings> => {
    const { data } = await api.GET("/api/settings/stack-history");
    if (!data) throw new Error("GET /api/settings/stack-history failed");
    return data;
  },
});

export function useStackHistorySettings() {
  return useQuery(stackHistorySettingsQuery);
}

// What a limit of keep would delete now. It is a count taken at the moment of asking, so it is never
// cached: the confirm that shows it must not show an older answer. It needs settings.write.
export function stackHistoryPrunePreviewQuery(keep: number) {
  return queryOptions({
    queryKey: ["settings", "stack-history", "preview", keep],
    queryFn: async (): Promise<StackHistoryPrunePreview> => {
      const { data } = await api.GET("/api/settings/stack-history/preview", {
        params: { query: { keep } },
      });
      if (!data) throw new Error("GET /api/settings/stack-history/preview failed");
      return data;
    },
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });
}

// StackHistorySettingsError is a change the server refused, with the problem it answered, or status 0
// when it could not be reached.
export class StackHistorySettingsError extends Error {
  readonly problem?: Problem;
  constructor(problem?: Problem) {
    super(`PATCH settings/stack-history failed (${problem?.status ?? 0})`);
    this.name = "StackHistorySettingsError";
    this.problem = problem;
  }
}

// useUpdateStackHistorySettings saves the limit. It is never retried: a second request would repeat a
// deletion. Lowering the limit deletes revisions at once, so every stack's history (the list and each
// open revision) is read again, and a revision that went reads as gone.
export function useUpdateStackHistorySettings() {
  const queryClient = useQueryClient();
  return useMutation<StackHistorySettings, StackHistorySettingsError, { kept: number }>({
    mutationFn: async (body) => {
      try {
        const { data, error, response } = await api.PATCH("/api/settings/stack-history", { body });
        if (!data) throw new StackHistorySettingsError(readProblem(error, response));
        return data;
      } catch (e) {
        throw e instanceof StackHistorySettingsError ? e : new StackHistorySettingsError();
      }
    },
    retry: false,
    onSuccess: (data) => {
      queryClient.setQueryData(stackHistorySettingsQuery.queryKey, data);
      void queryClient.invalidateQueries({
        predicate: (q) => q.queryKey[0] === "env" && q.queryKey.includes("revisions"),
      });
    },
  });
}
