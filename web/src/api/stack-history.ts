// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import {
  DeployRequestError,
  invalidateStacks,
  isStackGone,
  type StackDeployed,
  StacksLoadError,
  stackJobsKey,
  stackKey,
} from "@/api/stacks";

export type StackRevision = components["schemas"]["StackRevisionItem"];
export type StackRevisionList = components["schemas"]["StackRevisionListBody"];
export type StackRevisionDetail = components["schemas"]["StackRevisionDetailBody"];
export type StackRevisionImageChange = components["schemas"]["StackRevisionImageChange"];
export type StackRevisionChanges = components["schemas"]["StackRevisionChangesBody"];

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

// The query key of one revision of a stack. It extends the key of the list, so whatever refreshes the
// history refreshes an open revision too.
export function revisionKey(envId: string, stackId: string, seq: number) {
  return [...revisionsKey(envId, stackId), seq] as const;
}

// isRevisionGone says whether the error is the server answering that the stack holds no such revision: it
// was never taken, or retention deleted it.
export function isRevisionGone(error: unknown): boolean {
  return (
    error instanceof StacksLoadError && error.status === 404 && error.type === "revision_not_found"
  );
}

// One revision with its stored rendering, files, variables, pinned images and the YAML difference from the
// deployed revision. A secret variable reads <hidden> and a secret file has no content in it: History has no
// way to reveal either. A revision that is gone, or a stack that is, is not retried.
export function revisionQuery(envId: string, stackId: string, seq: number) {
  return queryOptions({
    queryKey: revisionKey(envId, stackId, seq),
    queryFn: async (): Promise<StackRevisionDetail> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/revisions/{seq}", {
        params: { path: { envId, stackId, seq } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && !isRevisionGone(error) && count < 3,
    refetchInterval: () => (document.visibilityState === "visible" ? LIST_REFETCH_MS : false),
  });
}

// The query key of what rolling back to a revision would change. It extends the revision's key, so whatever
// refreshes the history refreshes an open Changes tab too: what runs now is what it is compared with.
export function revisionChangesKey(envId: string, stackId: string, seq: number) {
  return [...revisionKey(envId, stackId, seq), "changes"] as const;
}

// What rolling back to a revision would change against what runs now. The server redacts the live settings of the
// running services for a caller who may not reveal secrets, so nothing here needs to hide them. It is read once
// and again when the history is refreshed, not on a timer: each read asks the swarm, and the server allows a user
// two at a time (429 check_busy). A stack or revision that is gone, a revision that cannot be rolled back to and a
// busy server are not retried: the person presses Retry.
export function revisionChangesQuery(envId: string, stackId: string, seq: number) {
  return queryOptions({
    queryKey: revisionChangesKey(envId, stackId, seq),
    queryFn: async (): Promise<StackRevisionChanges> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/revisions/{seq}/changes", {
        params: { path: { envId, stackId, seq } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) =>
      !isStackGone(error) &&
      !(error instanceof StacksLoadError && [404, 409, 429].includes(error.status)) &&
      count < 3,
  });
}

export type RollbackToRevisionInput = components["schemas"]["StackRollbackRequest"] & {
  revision: number;
};

// useRollbackToRevision starts a roll back to the revision the person chose and reviewed: the revision, and the
// deployed revision the review was made against, so the server refuses (409 deployed_changed) a stack that was
// deployed again since. It is never retried: a roll back whose answer was lost may have started, and the stack's
// page, read again, shows whether it did. The stack, its history and the jobs list are read again whatever the
// answer.
export function useRollbackToRevision(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDeployed, DeployRequestError, RollbackToRevisionInput>({
    mutationFn: async (body) => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/rollback", {
          params: { path: { envId, stackId } },
          body,
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
      void queryClient.invalidateQueries({ queryKey: revisionsKey(envId, stackId) });
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
  });
}
