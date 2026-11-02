// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type PullStartRequest = components["schemas"]["PullStartRequest"];
export type PullStarted = components["schemas"]["PullStartBody"];
export type PullJob = components["schemas"]["PullJobBody"];
export type PullItem = components["schemas"]["PullItemBody"];
export type PullLayer = components["schemas"]["PullLayerBody"];
export type PullTargets = components["schemas"]["PullTargetsBody"];
export type PullTarget = components["schemas"]["PullTargetBody"];
export type PullSummary = components["schemas"]["PullSummaryBody"];
export type PullState = PullJob["state"];
export type PullItemState = PullItem["state"];

// PullRequestError is a pull the server refused to start, or that got no answer. The
// status is 0 when the server could not be reached. The type is the problem type it named
// (ref_invalid, too_many_pulls, agent_policy_denied, node_unavailable ...).
export class PullRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST images/pull failed (${status})`);
    this.name = "PullRequestError";
    this.status = status;
    this.type = type;
  }
}

// postPull starts a pull. It never retries: a pull whose answer was lost may have started,
// and a failed pull is retried by the person who started it (P2 D-42).
export async function postPull(envId: string, body: PullStartRequest): Promise<PullStarted> {
  try {
    const res = await api.POST("/api/env/{envId}/images/pull", {
      params: { path: { envId } },
      body,
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new PullRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof PullRequestError ? e : new PullRequestError(0);
  }
}

// useStartPull is the mutation behind the pull form. It does not retry.
export function useStartPull(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<PullStarted, PullRequestError, PullStartRequest>({
    mutationFn: (body) => postPull(envId, body),
    retry: false,
    // The Pulls indicator learns of the new job at once, not on its next poll.
    onSuccess: () => queryClient.invalidateQueries({ queryKey: pullsListKey }),
  });
}

export const pullsListKey = ["pulls"] as const;

// postCancelPull asks the server to stop the nodes of a pull that have not finished. It
// answers the job as it stands, normally already ended. It never retries: a cancel is a
// change, and one whose answer was lost may have worked (P2 D-42). The type of an error is
// the problem type: pull_finished (409) when the pull had already ended, pull_not_found.
export async function postCancelPull(jobId: string): Promise<PullJob> {
  try {
    const res = await api.POST("/api/pulls/{jobId}/cancel", { params: { path: { jobId } } });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new PullRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    throw e instanceof PullRequestError ? e : new PullRequestError(0);
  }
}

interface CancelPullHandlers {
  onSuccess?: (job: PullJob) => void;
  onError?: (error: PullRequestError) => void;
  // After either, with the Pulls list already asked to read again.
  onSettled?: () => void;
}

// useCancelPull is the mutation behind every Cancel pull button. The handlers are the
// mutation's own, so they still run when the button that sent the cancel has gone (a
// popover that closed). It does not retry, and the Pulls list is read again afterwards
// whatever the answer was.
export function useCancelPull(handlers: CancelPullHandlers = {}) {
  const queryClient = useQueryClient();
  return useMutation<PullJob, PullRequestError, string>({
    mutationFn: postCancelPull,
    retry: false,
    onSuccess: (job) => handlers.onSuccess?.(job),
    onError: (error) => handlers.onError?.(error),
    onSettled: () => {
      handlers.onSettled?.();
      return queryClient.invalidateQueries({ queryKey: pullsListKey });
    },
  });
}

// Polled every 3 seconds while a pull runs and every 30 otherwise (UI-SPEC "Pulls indicator").
export const PULLS_POLL_RUNNING_MS = 3_000;
export const PULLS_POLL_IDLE_MS = 30_000;

// pullsListQuery lists the pulls the caller may follow: running ones and those that ended in
// the last ten minutes. A poll that fails keeps the last list, so the indicator shows no
// error chrome (the query keeps its data when a refetch fails).
export const pullsListQuery = queryOptions({
  queryKey: [...pullsListKey, "list"] as const,
  queryFn: async (): Promise<PullSummary[]> => {
    const res = await api.GET("/api/pulls");
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new PullRequestError(problem.status, problem.type);
    }
    return res.data.pulls ?? [];
  },
  refetchInterval: (query) =>
    query.state.data?.some((p) => p.state === "running")
      ? PULLS_POLL_RUNNING_MS
      : PULLS_POLL_IDLE_MS,
  retry: false,
});

export const pullKey = (jobId: string) => ["pulls", jobId] as const;

// pullQuery reads one job as it stands. A job that is gone (404) or that the caller may not
// read is not retried: the answer will not change.
export const pullQuery = (jobId: string) =>
  queryOptions({
    queryKey: pullKey(jobId),
    queryFn: async (): Promise<PullJob> => {
      const res = await api.GET("/api/pulls/{jobId}", { params: { path: { jobId } } });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new PullRequestError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
  });

export const pullTargetsKey = (envId: string) => ["pull-targets", envId] as const;

// pullTargetsQuery lists the nodes of an environment with their role, architecture and
// whether they can be reached and written to now. It is read for the node chooser and for
// the read-only state of the Images page.
export const pullTargetsQuery = (envId: string) =>
  queryOptions({
    queryKey: pullTargetsKey(envId),
    queryFn: async (): Promise<PullTargets> => {
      const res = await api.GET("/api/env/{envId}/images/pull-targets", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new PullRequestError(problem.status, problem.type);
      }
      return res.data;
    },
    staleTime: 10_000,
    retry: false,
  });

// allReadOnly is true when the environment has nodes and every one is read-only: nothing
// can be pulled or removed there until its owner changes the host.
export function allReadOnly(targets: PullTargets | undefined): boolean {
  const nodes = targets?.nodes ?? [];
  return nodes.length > 0 && nodes.every((n) => n.readOnly);
}

// useAllReadOnly reads the targets for the Images page and its Pull button.
export function useAllReadOnly(envId: string): boolean {
  const { data } = useQuery(pullTargetsQuery(envId));
  return allReadOnly(data);
}
