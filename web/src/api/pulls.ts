// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQuery } from "@tanstack/react-query";

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
  return useMutation<PullStarted, PullRequestError, PullStartRequest>({
    mutationFn: (body) => postPull(envId, body),
    retry: false,
  });
}

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
