// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api } from "@/api/client";
import { type Problem, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { invalidateStacks, isStackGone, StacksLoadError, stackKey, stackQuery } from "@/api/stacks";

export type StackGit = components["schemas"]["StackGitSummary"];
// What a pull did to the draft: the version to review and deploy, and the commit it holds.
export type StackGitPull = components["schemas"]["StackGitPullBody"];
// The commits that changed a git stack's folder between its deployed commit and a head, newest first.
export type StackGitCommits = components["schemas"]["StackGitCommitsBody"];
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

// StackGitUpdatesError is a change of polling the server refused, with the problem it answered, or no problem when it
// could not be reached.
export class StackGitUpdatesError extends Error {
  readonly problem?: Problem;
  constructor(problem?: Problem) {
    super(`PUT git/updates failed (${problem?.status ?? 0})`);
    this.name = "StackGitUpdatesError";
    this.problem = problem;
  }
}

export interface StackGitUpdates {
  pollEnabled: boolean;
  // Whole minutes from 1 to 1440.
  pollMinutes: number;
}

// useUpdateStackGitUpdates turns a git stack's polling on or off and sets how often it is checked (D-14). It is not
// retried. The answer is the stack's git summary, which replaces the one held; the stack, its lists and the bell are
// read again.
export function useUpdateStackGitUpdates(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackGit, StackGitUpdatesError, StackGitUpdates>({
    mutationFn: async (body) => {
      try {
        const { data, error, response } = await api.PUT(
          "/api/env/{envId}/stacks/{stackId}/git/updates",
          { params: { path: { envId, stackId } }, body },
        );
        if (!data) throw new StackGitUpdatesError(readProblem(error, response));
        return data;
      } catch (e) {
        throw e instanceof StackGitUpdatesError ? e : new StackGitUpdatesError();
      }
    },
    retry: false,
    onSuccess: (data) => {
      queryClient.setQueryData(stackGitKey(envId, stackId), data);
      invalidateStacks(queryClient, envId);
    },
  });
}

// StackGitPullError is a pull the server refused or that got no answer. The status is 0 when the server could not be
// reached; the type is the problem type it named: draft_changed when the draft moved while pulling, or the code of the
// git failure (unreachable, auth_refused, ref_missing, folder_missing ...).
export class StackGitPullError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST git/pull failed (${status})`);
    this.name = "StackGitPullError";
    this.status = status;
    this.type = type;
  }
}

// usePullStackGit fetches the newest commit of the ref a git stack follows into its draft (D-17). It reads the draft's
// version straight from the server, not from the cache an open editor holds, so a draft another person saved a moment
// ago is seen and not overwritten blind: if it moves between that read and the pull, the server answers draft_changed
// and nothing is stored. A pull deploys nothing: the caller goes on to the Review. It is never retried. The stack, its
// git summary and its lists are read again, whatever the answer.
export function usePullStackGit(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackGitPull, StackGitPullError>({
    mutationFn: async () => {
      try {
        const read = await api.GET("/api/env/{envId}/stacks/{stackId}/draft", {
          params: { path: { envId, stackId } },
        });
        if (!read.data) {
          const problem = readProblem(read.error, read.response);
          throw new StackGitPullError(problem.status, problem.type);
        }
        const { data, error, response } = await api.POST(
          "/api/env/{envId}/stacks/{stackId}/git/pull",
          { params: { path: { envId, stackId } }, body: { draftVersion: read.data.version } },
        );
        if (!data) {
          const problem = readProblem(error, response);
          throw new StackGitPullError(problem.status, problem.type);
        }
        return data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof StackGitPullError ? e : new StackGitPullError(0);
      }
    },
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: stackGitKey(envId, stackId) });
      invalidateStacks(queryClient, envId);
    },
  });
}

// The query key of the commits that changed a stack's folder up to head. It extends the git summary's key, so a pull or a
// check that refreshes the summary refreshes the list.
export function stackGitCommitsKey(envId: string, stackId: string, head: string) {
  return [...stackGitKey(envId, stackId), "commits", head] as const;
}

// The commits that changed the stack's folder between the commit it runs and head (D-13), as the commits route lists
// them. A list the server can't make comes back with available false, not as an error; a request that fails is an error
// the caller words the same way, since the list only informs.
export function stackGitCommitsQuery(envId: string, stackId: string, head: string) {
  return queryOptions({
    queryKey: stackGitCommitsKey(envId, stackId, head),
    queryFn: async (): Promise<StackGitCommits> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/git/commits", {
        params: { path: { envId, stackId }, query: { head } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
  });
}
