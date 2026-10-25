// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import { revisionsKey } from "@/api/stack-history";
import {
  DeployRequestError,
  invalidateStacks,
  isStackGone,
  type StackDeployed,
  StacksLoadError,
  stackJobsKey,
  stackKey,
} from "@/api/stacks";

export type StackDrift = components["schemas"]["StackDriftBody"];
export type StackDriftService = components["schemas"]["StackDriftServiceBody"];
export type StackDriftSummary = components["schemas"]["StackDriftSummaryBody"];
export type StackDriftCause = components["schemas"]["StackDriftCauseBody"];

// The query key of a stack's drift. It extends the stack's key, so whatever refreshes the stack's page refreshes its
// drift too. The key of a fresh check extends it again, so a check made on purpose and the newest one the server
// holds are two reads of two things.
export function driftKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "drift"] as const;
}

interface DriftOptions {
  // True makes the server read the stack again instead of answering the newest check it holds.
  fresh?: boolean;
}

// How a stack compares with its deployed revision (STK-14). Without fresh it is the newest check the server holds,
// read again every 30 s while the page is visible. With fresh the server reads the stack now: the query runs when it
// is used and not on a timer or a focus, so a check is made when a person opens the page or asks for one, and by
// nothing else. A stack that is gone is not retried.
export function driftQuery(envId: string, stackId: string, { fresh = false }: DriftOptions = {}) {
  return queryOptions({
    queryKey: fresh ? ([...driftKey(envId, stackId), "fresh"] as const) : driftKey(envId, stackId),
    queryFn: async (): Promise<StackDrift> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/drift", {
        params: { path: { envId, stackId }, query: fresh ? { fresh: true } : {} },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 3,
    ...(fresh
      ? { refetchOnWindowFocus: false, refetchOnReconnect: false, gcTime: 5 * 60_000 }
      : {
          refetchInterval: () => (document.visibilityState === "visible" ? LIST_REFETCH_MS : false),
        }),
  });
}

// What a page knows about a stack's drift: the newest result, whether a fresh check is running, whether the last
// one failed, and the way to make another.
export interface DriftView {
  data: StackDrift | undefined;
  // A check that was asked for is running.
  checking: boolean;
  // The check failed (the server could not read the stack, or the request failed).
  failed: boolean;
  // When the fresh check last finished, in milliseconds; 0 before it has.
  checkedAt: number;
  // How many fresh checks have finished well: it goes up by one with each, though the answer be the same.
  checks: number;
  recheck: () => void;
}

// useDrift reads a stack's drift and checks it afresh once, when the page that uses it opens (UI-SPEC "Drift page":
// "The page runs a fresh check on open and shows the last cached result under a skeleton while it does"). The result
// of the fresh check is shown when there is one, and the newest one the server held until then.
export function useDrift(envId: string, stackId: string): DriftView {
  const queryClient = useQueryClient();
  const cached = useQuery(driftQuery(envId, stackId));
  const freshOptions = driftQuery(envId, stackId, { fresh: true });
  const fresh = useQuery({ ...freshOptions, staleTime: 0 });
  return {
    data: fresh.data ?? cached.data,
    checking: fresh.isFetching,
    failed: fresh.isError && !fresh.isFetching,
    checkedAt: fresh.dataUpdatedAt,
    checks: queryClient.getQueryState(freshOptions.queryKey)?.dataUpdateCount ?? 0,
    recheck: () => void fresh.refetch(),
  };
}

export interface RestoreStackInput {
  // The deployed revision the review was made of, which the server checks is still the deployed one.
  revision: number;
}

// useRestoreStack puts the deployed revision back (D-23): it redeploys exactly what the review showed, the compose
// file, files, variables and stored digests, and the server records it as a new revision. It is only ever called from
// the restore review's button, a person's explicit click: nothing restores drift by itself. It is never retried: a
// restore whose answer was lost may have started, and the stack's page, read again, shows whether it did. The stack,
// its drift, its history and the jobs list are read again whatever the answer.
export function useRestoreStack(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDeployed, DeployRequestError, RestoreStackInput>({
    mutationFn: async ({ revision }) => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/rollback", {
          params: { path: { envId, stackId } },
          body: { mode: "restore", revision },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new DeployRequestError(problem.status, problem.type, problem.fields);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof DeployRequestError ? e : new DeployRequestError(0);
      }
    },
    retry: false,
    onSettled: () => {
      invalidateStacks(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: driftKey(envId, stackId) });
      void queryClient.invalidateQueries({ queryKey: revisionsKey(envId, stackId) });
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
  });
}
