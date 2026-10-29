// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api } from "@/api/client";
import { type GitRefKind, GitRequestError } from "@/api/git";
import { type Problem, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import {
  draftKey,
  invalidateStacks,
  isStackGone,
  type StackDetail,
  StacksLoadError,
  stackKey,
  stackQuery,
} from "@/api/stacks";
import { ENV_WARNINGS_KEY } from "@/api/warnings";

export type StackGit = components["schemas"]["StackGitSummary"];
// What a pull did to the draft: the version to review and deploy, and the commit it holds.
export type StackGitPull = components["schemas"]["StackGitPullBody"];
// What a change of source did to the draft: the version to review and deploy, and the commit it holds.
export type StackGitSourceChanged = components["schemas"]["StackGitSourceBody"];
// Where a git stack is to follow from now on, with the version of the draft the person read.
export interface StackGitSourceRequest {
  draftVersion: number;
  repoId: string;
  refKind: GitRefKind;
  ref: string;
  folder: string;
  composeFiles: string[];
}
// What pointing an inline or adopted stack at a repository did to its draft: the version to review and deploy, and the
// commit it holds.
export type StackGitConnected = components["schemas"]["StackGitConnectBody"];
// One file of the stack as it is today: kept as an overlay file, or dropped.
export interface StackGitConnectFile {
  path: string;
  keep: boolean;
}
// Where an inline or adopted stack is to follow a repository, what becomes of its files, and the values to set over its
// variables, with the version of the draft the person read.
export interface StackGitConnectRequest {
  draftVersion: number;
  repoId: string;
  refKind: GitRefKind;
  ref: string;
  folder: string;
  composeFiles: string[];
  files: StackGitConnectFile[];
  vars?: { name: string; value: string; secret: boolean }[];
}
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
    onSettled: () => {
      invalidateStacks(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
    },
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

  // The check has ended: a sync error it raised or cleared is read by the bell now, not at its next poll.
  useEffect(() => {
    if (!settled) return;
    setPolling(false);
    void queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
  }, [settled, queryClient]);

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

// StackGitAutoDeployError is a change of auto-deploy the server refused or that got no answer. The status is 0 when the
// server could not be reached; the type is the problem type it named (owner_lacks_permission, stack_not_git).
export class StackGitAutoDeployError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`PUT git/auto-deploy failed (${status})`);
    this.name = "StackGitAutoDeployError";
    this.status = status;
    this.type = type;
  }
}

// useSetStackGitAutoDeploy turns a git stack's automatic deploys on or off (D-18). Turning them on registers the caller as
// the person they run for and resumes a paused one, so the caller confirms first; it is never retried. The answer is the
// stack's git summary, which replaces the one held; the stack, its lists and the bell are read again.
export function useSetStackGitAutoDeploy(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackGit, StackGitAutoDeployError, boolean>({
    mutationFn: async (enabled) => {
      try {
        const { data, error, response } = await api.PUT(
          "/api/env/{envId}/stacks/{stackId}/git/auto-deploy",
          { params: { path: { envId, stackId } }, body: { enabled } },
        );
        if (!data) {
          const problem = readProblem(error, response);
          throw new StackGitAutoDeployError(problem.status, problem.type);
        }
        return data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof StackGitAutoDeployError ? e : new StackGitAutoDeployError(0);
      }
    },
    retry: false,
    onSuccess: (data) => {
      queryClient.setQueryData(stackGitKey(envId, stackId), data);
    },
    onSettled: () => {
      invalidateStacks(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
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

// useChangeStackGitSource saves a new source (repository, ref, folder and compose files) into the stack's draft as a
// pending source (D-11, D-12): nothing is applied, and the stack follows what it followed until a deploy of the draft ends
// good. draftVersion is the version of the draft the person read when they began, so a draft saved by someone else since
// is refused as draft_changed, with who saved it in the error's fields (savedBy). It is never retried. The stack, its git
// summary and its lists are read again, whatever the answer.
export function useChangeStackGitSource(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackGitSourceChanged, GitRequestError, StackGitSourceRequest>({
    mutationFn: async (body) => {
      try {
        const { data, error, response } = await api.POST(
          "/api/env/{envId}/stacks/{stackId}/git/source",
          { params: { path: { envId, stackId } }, body },
        );
        if (!data) {
          const problem = readProblem(error, response);
          throw new GitRequestError(problem.status, problem.type, problem.fields, "POST");
        }
        return data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof GitRequestError ? e : new GitRequestError(0, undefined, [], "POST");
      }
    },
    retry: false,
    gcTime: 0,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: stackGitKey(envId, stackId) });
      invalidateStacks(queryClient, envId);
    },
  });
}

// useConnectStackGit writes the draft of an inline or adopted stack as its first sync with a repository (D-24): the
// repository's files, the files chosen to be kept as overlay files and the stack's variables. Nothing is applied: the stack
// becomes a git stack when a deploy of the draft ends good, and discarding the draft leaves it as it was. draftVersion is the
// version the person read when the page opened, so a draft saved by someone else since is refused as draft_changed (savedBy
// names who). It is never retried. The stack and its lists are read again, whatever the answer.
export function useConnectStackGit(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackGitConnected, GitRequestError, StackGitConnectRequest>({
    mutationFn: async (body) => {
      try {
        const { data, error, response } = await api.POST(
          "/api/env/{envId}/stacks/{stackId}/connect-git",
          { params: { path: { envId, stackId } }, body },
        );
        if (!data) {
          const problem = readProblem(error, response);
          throw new GitRequestError(problem.status, problem.type, problem.fields, "POST");
        }
        return data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof GitRequestError ? e : new GitRequestError(0, undefined, [], "POST");
      }
    },
    retry: false,
    gcTime: 0,
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

// The webhook a git stack may have, as the git route summarises it: no secret and no routing token, only the
// address with the token masked, the last four characters of the secret and the last delivery.
export type StackGitWebhook = components["schemas"]["StackGitWebhookSummary"];
export type StackGitWebhookFlavour = StackGitWebhook["flavour"];
export type StackGitWebhookPlacement = StackGitWebhook["placement"];
// What a create or a rotate answers once: the full address and the secret. It is read from the answer into component
// state by the caller and is never kept by a query.
export type StackGitWebhookSecret = components["schemas"]["StackGitWebhookBody"];

// StackGitWebhookError is a create, rotate or remove of a webhook the server refused or that got no answer. The status
// is 0 when the server could not be reached; the type is the problem type it named (webhook_exists, webhook_not_found,
// webhook_placement, stack_not_git).
export class StackGitWebhookError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(method: string, status: number, type?: string) {
    super(`${method} git/webhook failed (${status})`);
    this.name = "StackGitWebhookError";
    this.status = status;
    this.type = type;
  }
}

export interface StackGitWebhookInput {
  flavour: StackGitWebhookFlavour;
  // Only for the generic flavour: path puts the secret in the address, header expects Authorization: Bearer.
  placement?: "path" | "header";
}

// The options every webhook mutation shares. The answer of a create or a rotate holds the secret, so it must not
// outlive the call: the mutation cache drops it as soon as nothing observes the mutation (gcTime 0), and the caller
// resets the mutation once it has taken the answer. It is not retried either (P2 D-42). The git summary is read
// again whatever the answer, which brings the masked rows.
function useWebhookMutation<TData, TVariables>(
  envId: string,
  stackId: string,
  run: (variables: TVariables) => Promise<TData>,
) {
  const queryClient = useQueryClient();
  return useMutation<TData, StackGitWebhookError, TVariables>({
    mutationFn: async (variables) => {
      try {
        return await run(variables);
      } catch (e) {
        // A network failure has no status.
        throw e instanceof StackGitWebhookError ? e : new StackGitWebhookError("POST", 0);
      }
    },
    retry: false,
    gcTime: 0,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: stackGitKey(envId, stackId) });
    },
  });
}

// useCreateStackGitWebhook gives the stack its webhook (D-15). The answer is the address and the secret, once.
export function useCreateStackGitWebhook(envId: string, stackId: string) {
  return useWebhookMutation<StackGitWebhookSecret, StackGitWebhookInput>(
    envId,
    stackId,
    async (body) => {
      const { data, error, response } = await api.POST(
        "/api/env/{envId}/stacks/{stackId}/git/webhook",
        { params: { path: { envId, stackId } }, body },
      );
      if (!data) {
        const problem = readProblem(error, response);
        throw new StackGitWebhookError("POST", problem.status, problem.type);
      }
      return data;
    },
  );
}

// useRotateStackGitWebhook replaces the webhook's address and secret and answers the new ones, once.
export function useRotateStackGitWebhook(envId: string, stackId: string) {
  return useWebhookMutation<StackGitWebhookSecret, void>(envId, stackId, async () => {
    const { data, error, response } = await api.POST(
      "/api/env/{envId}/stacks/{stackId}/git/webhook/rotate",
      { params: { path: { envId, stackId } } },
    );
    if (!data) {
      const problem = readProblem(error, response);
      throw new StackGitWebhookError("POST", problem.status, problem.type);
    }
    return data;
  });
}

// useRemoveStackGitWebhook deletes the webhook; its address answers 404 from then on.
export function useRemoveStackGitWebhook(envId: string, stackId: string) {
  return useWebhookMutation<void, void>(envId, stackId, async () => {
    const { error, response } = await api.DELETE("/api/env/{envId}/stacks/{stackId}/git/webhook", {
      params: { path: { envId, stackId } },
    });
    if (!response.ok) {
      const problem = readProblem(error, response);
      throw new StackGitWebhookError("DELETE", problem.status, problem.type);
    }
  });
}

// StackGitDetachError is a detach the server refused or that got no answer. The status is 0 when the server could not be
// reached; the type is the problem type it named (stack_not_git when the stack is not from a repository any more).
export class StackGitDetachError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST git/detach failed (${status})`);
    this.name = "StackGitDetachError";
    this.status = status;
    this.type = type;
  }
}

// useDetachStackGit turns a git stack back into an inline stack (D-25): the files of the deployed revision become its
// draft, and Bayplan stops checking the repository, removes the webhook and turns off auto-deploy. Nothing running
// changes. It is never retried. Once the server has answered, the stack is an inline one: the page's copy says so at once
// (the Git tab has nothing left to read, and leads to the Overview), the git summary is dropped rather than read again,
// and the draft, the lists and the warnings are read again.
export function useDetachStackGit(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, StackGitDetachError, void>({
    mutationFn: async () => {
      try {
        const { error, response } = await api.POST("/api/env/{envId}/stacks/{stackId}/git/detach", {
          params: { path: { envId, stackId } },
        });
        if (!response.ok) {
          const problem = readProblem(error, response);
          throw new StackGitDetachError(problem.status, problem.type);
        }
      } catch (e) {
        // A network failure has no status.
        throw e instanceof StackGitDetachError ? e : new StackGitDetachError(0);
      }
    },
    retry: false,
    onSuccess: () => {
      queryClient.setQueryData(stackKey(envId, stackId), (old: StackDetail | undefined) =>
        old ? { ...old, source: "inline", git: undefined } : old,
      );
      queryClient.removeQueries({ queryKey: stackGitKey(envId, stackId) });
      void queryClient.invalidateQueries({ queryKey: stackKey(envId, stackId), exact: true });
      void queryClient.invalidateQueries({ queryKey: draftKey(envId, stackId) });
      invalidateStacks(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
    },
    onError: (e) => {
      // The stack is not a git stack (it was detached already, by someone else): read it again to say so.
      if (e.type === "stack_not_git") {
        void queryClient.invalidateQueries({ queryKey: stackKey(envId, stackId) });
      }
    },
  });
}
