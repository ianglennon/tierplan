// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type StackItem = components["schemas"]["StackItem"];
export type StackDetail = components["schemas"]["StackDetail"];
export type StackDraft = components["schemas"]["StackDraftBody"];
export type StackCreateInput = components["schemas"]["StackCreateRequest"];
export type StackState = StackItem["state"];

// The query key of an environment's stacks list, and of every read that extends it: one stack's
// page is under it, so a change to the list refreshes the page too.
export function stacksKey(envId: string) {
  return listKey(envId, "stacks");
}

// The query key of one stack's page.
export function stackKey(envId: string, stackId: string) {
  return [...stacksKey(envId), stackId] as const;
}

// StacksLoadError is a stacks read the server refused or could not answer. The status and the
// problem type tell a stack that is gone (404 stack_not_found) from every other failure.
export class StacksLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET stacks failed (${status})`);
    this.name = "StacksLoadError";
    this.status = status;
    this.type = type;
  }
}

// isStackGone says whether the error is the server answering that the stack does not exist on
// this environment: it was removed, or the address names a stack of another environment.
export function isStackGone(error: unknown): boolean {
  return (
    error instanceof StacksLoadError && error.status === 404 && error.type === "stack_not_found"
  );
}

// The stacks of an environment, by name.
export function stacksQuery(envId: string) {
  return queryOptions({
    queryKey: stacksKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/stacks", { params: { path: { envId } } });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}

// One stack. A stack that is gone is not retried: it will not come back on a second try.
export function stackQuery(envId: string, stackId: string) {
  return queryOptions({
    queryKey: stackKey(envId, stackId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 3,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// StackRequestError is a create the server refused or that got no answer. The status is 0 when
// the server could not be reached; the type is the problem type it named (stack_name_invalid,
// stack_name_taken ...).
export class StackRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST stacks failed (${status})`);
    this.name = "StackRequestError";
    this.status = status;
    this.type = type;
  }
}

// postStack creates one stack. It never retries: a create whose answer was lost may have
// happened (P2 D-42), and the list, read again, shows whether it did.
export async function postStack(envId: string, body: StackCreateInput): Promise<StackDetail> {
  try {
    const res = await api.POST("/api/env/{envId}/stacks", { params: { path: { envId } }, body });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new StackRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof StackRequestError ? e : new StackRequestError(0);
  }
}

// useCreateStack is the mutation behind the Create stack dialog. It does not retry, and reads
// the list again whatever the answer.
export function useCreateStack(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDetail, StackRequestError, StackCreateInput>({
    mutationFn: (body) => postStack(envId, body),
    retry: false,
    onSettled: () => void queryClient.invalidateQueries({ queryKey: stacksKey(envId) }),
  });
}
