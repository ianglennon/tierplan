// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { invalidateStacks, isStackGone, StacksLoadError, stackKey, stackQuery } from "@/api/stacks";

export type StackGit = components["schemas"]["StackGitSummary"];
// Why a git stack's sync is in error, or why it is behind with a note: the union the server's enum generates.
export type StackGitReason = NonNullable<StackGit["reasonCode"]>;

// How often a stack is read again after Check now, and for how long at most (UI-SPEC "Polling cadence").
export const STACK_GIT_POLL_MS = 2_000;
export const STACK_GIT_POLL_MAX_MS = 30_000;

// The query key of a stack's git summary. It extends the stack's key, so whatever refreshes the page refreshes it.
export function stackGitKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "git"] as const;
}

// Where a git stack stands against its repository, as the stack's git route answers. A stack that is gone is not
// retried.
export function stackGitQuery(envId: string, stackId: string) {
  return queryOptions({
    queryKey: stackGitKey(envId, stackId),
    queryFn: async (): Promise<StackGit> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/git", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 3,
  });
}

// StackGitCheckError is a Check now the server refused or that got no answer. The status is 0 when the server could
// not be reached.
export class StackGitCheckError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST git/check failed (${status})`);
    this.name = "StackGitCheckError";
    this.status = status;
    this.type = type;
  }
}

interface CheckOptions {
  // Called with the error of a Check now that did not start.
  onError?: (error: StackGitCheckError) => void;
}

export interface StackGitCheck {
  // Asks the server to check the stack now. Nothing happens while a check is already being waited for.
  check: () => void;
  // True from the click until the check has ended: the request is on its way, or the stack is being read every 2 s
  // for the answer.
  running: boolean;
}

// useCheckStackGit asks the server to fetch the stack's ref and compare it with what is deployed (GIT-03). The server
// answers 202 at once and checks in the background, so on success the stack is read again every 2 s until its git
// summary says it is no longer checking, and for 30 s at most. A request while one runs joins it on the server, and
// here it is ignored. The stack, its lists and the bell are read again whatever the answer.
export function useCheckStackGit(
  envId: string,
  stackId: string,
  { onError }: CheckOptions = {},
): StackGitCheck {
  const queryClient = useQueryClient();
  const [polling, setPolling] = useState(false);
  // How many answers the stack had when the check was accepted: only a later one can say the check has ended.
  const startCount = useRef(0);
  const mutation = useMutation<void, StackGitCheckError>({
    mutationFn: async () => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/git/check", {
          params: { path: { envId, stackId } },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new StackGitCheckError(problem.status, problem.type);
        }
      } catch (e) {
        // A network failure has no status.
        throw e instanceof StackGitCheckError ? e : new StackGitCheckError(0);
      }
    },
    retry: false,
    onSuccess: () => {
      startCount.current =
        queryClient.getQueryState(stackKey(envId, stackId))?.dataUpdateCount ?? 0;
      setPolling(true);
    },
    onError: (e) => onError?.(e),
    onSettled: () => invalidateStacks(queryClient, envId),
  });

  // While waiting, the stack is read every 2 s by an observer of its own: the page's observer keeps its 30 s.
  const stack = useQuery({
    ...stackQuery(envId, stackId),
    enabled: polling,
    refetchInterval: STACK_GIT_POLL_MS,
  });
  const answers = queryClient.getQueryState(stackKey(envId, stackId))?.dataUpdateCount ?? 0;
  const settled =
    polling &&
    !stack.isFetching &&
    answers > startCount.current &&
    stack.data?.git?.checking !== true;

  useEffect(() => {
    if (settled) setPolling(false);
  }, [settled]);

  useEffect(() => {
    if (!polling) return;
    const timer = setTimeout(() => setPolling(false), STACK_GIT_POLL_MAX_MS);
    return () => clearTimeout(timer);
  }, [polling]);

  return {
    check: () => {
      if (!mutation.isPending && !polling) mutation.mutate();
    },
    running: mutation.isPending || polling,
  };
}
