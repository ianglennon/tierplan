// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type StackItem = components["schemas"]["StackItem"];
export type StackDetail = components["schemas"]["StackDetail"];
export type StackDraft = components["schemas"]["StackDraftBody"];
export type StackDraftSaveInput = components["schemas"]["StackDraftSaveRequest"];
export type StackDraftSaved = components["schemas"]["StackDraftSaveBody"];
export type StackCreateInput = components["schemas"]["StackCreateRequest"];
export type StackState = StackItem["state"];
export type StackCheck = components["schemas"]["StackCheckBody"];
export type StackProblem = components["schemas"]["StackProblemBody"];
export type StackCheckInput = components["schemas"]["StackCheckRequest"];

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

// The query key of one stack's draft. It is deliberately not under stackKey: a refresh of the list
// or the page (the events feed, a poll) must not read the draft again behind an editor that holds
// the version it loaded.
export function draftKey(envId: string, stackId: string) {
  return ["stack-draft", envId, stackId] as const;
}

// A stack's draft: its files and variables with the version to send back, who saved it last and
// when. The value of a secret is <hidden>. The editor reads it once; it is read again only when
// the person asks (Reload draft), and not kept when the editor closes, so opening the editor
// again always shows what is saved now.
export function draftQuery(envId: string, stackId: string) {
  return queryOptions({
    queryKey: draftKey(envId, stackId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/draft", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 3,
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

// DraftRequestError is a draft save or discard the server refused or that got no answer. The
// status is 0 when the server could not be reached; the type is the problem type it named; the
// fields are its per-item codes (the path of a refused file, who saved a draft that moved).
export class DraftRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  constructor(method: string, status: number, type?: string, fields: ProblemField[] = []) {
    super(`${method} draft failed (${status})`);
    this.name = "DraftRequestError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }
}

// useSaveDraft saves the working copy as the stack's draft. It never retries: a save whose answer
// was lost may have happened, and the editor, told so, reads the draft again before it saves
// (P2 D-42). On success the draft in the cache follows what was saved, and the list and the page
// are read again.
export function useSaveDraft(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDraftSaved, DraftRequestError, StackDraftSaveInput>({
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/env/{envId}/stacks/{stackId}/draft", {
          params: { path: { envId, stackId } },
          body,
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new DraftRequestError("PUT", problem.status, problem.type, problem.fields);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof DraftRequestError ? e : new DraftRequestError("PUT", 0);
      }
    },
    retry: false,
    onSuccess: (answer, body) => {
      queryClient.setQueryData(draftKey(envId, stackId), (old: StackDraft | undefined) =>
        old
          ? {
              ...old,
              version: answer.version,
              savedBy: answer.savedBy,
              savedAt: answer.savedAt,
              files: body.files,
              vars: body.vars,
            }
          : old,
      );
      // The page and the list show the Draft badge: stacksKey is the prefix of both.
      void queryClient.invalidateQueries({ queryKey: stacksKey(envId) });
    },
  });
}

// useDiscardDraft deletes the stack's saved draft. The answer is 204 whether there was a draft or
// not, so a second press is harmless; it is never retried all the same. On success the list and
// the page are read again; the caller reads the draft again, which is now what is deployed.
export function useDiscardDraft(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, DraftRequestError, void>({
    mutationFn: async () => {
      try {
        const res = await api.DELETE("/api/env/{envId}/stacks/{stackId}/draft", {
          params: { path: { envId, stackId } },
        });
        if (!res.response.ok) {
          const problem = readProblem(res.error, res.response);
          throw new DraftRequestError("DELETE", problem.status, problem.type, problem.fields);
        }
      } catch (e) {
        // A network failure has no status.
        throw e instanceof DraftRequestError ? e : new DraftRequestError("DELETE", 0);
      }
    },
    retry: false,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: stacksKey(envId) }),
  });
}

// CheckRequestError is a check the server refused or that got no answer. The status is 0 when the
// server could not be reached; 429 with the type check_busy means the person already has two
// checks running.
export class CheckRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST check failed (${status})`);
    this.name = "CheckRequestError";
    this.status = status;
    this.type = type;
  }
}

// postStackCheck sends the working copy to be checked. The server reads nothing but the bytes it is
// sent and writes nothing, so a check is safe to repeat; it is never retried all the same, because
// the editor sends the next one when the person types again. The answer's lists are never null.
export async function postStackCheck(
  envId: string,
  stackId: string,
  body: StackCheckInput,
  signal?: AbortSignal,
): Promise<{ problems: StackProblem[]; unusedFiles: string[] }> {
  try {
    const res = await api.POST("/api/env/{envId}/stacks/{stackId}/check", {
      params: { path: { envId, stackId } },
      body,
      signal,
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new CheckRequestError(problem.status, problem.type);
    }
    return { problems: res.data.problems ?? [], unusedFiles: res.data.unusedFiles ?? [] };
  } catch (e) {
    // A network failure has no status.
    throw e instanceof CheckRequestError ? e : new CheckRequestError(0);
  }
}
