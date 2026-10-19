// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import { HIDDEN } from "@/features/spec-editor/diff";

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
export type StackVariable = components["schemas"]["StackVariableBody"];
export type StackRender = components["schemas"]["StackRenderBody"];
export type StackReview = components["schemas"]["StackReviewBody"];
export type StackReviewService = components["schemas"]["StackReviewServiceBody"];
export type StackReviewImage = components["schemas"]["StackReviewImageBody"];
export type StackReviewObject = components["schemas"]["StackReviewObjectBody"];
export type StackTextDiff = components["schemas"]["StackTextDiffBody"];
export type StackDiffHunk = components["schemas"]["StackDiffHunkBody"];
export type StackDiffLine = components["schemas"]["StackDiffLineBody"];
export type StackReviewSummary = components["schemas"]["StackReviewSummaryBody"];
export type StackDeployInput = components["schemas"]["StackDeployRequest"];
export type StackDeployed = components["schemas"]["StackDeployBody"];
export type StackJob = components["schemas"]["StackJobBody"];
export type StackJobItem = components["schemas"]["StackJobItemBody"];

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
      // The files the server sealed, which includes a file it found the compose file uses under
      // secrets:, are <hidden> in the cache whatever the body said.
      const sealed = new Set(answer.secretFiles ?? []);
      queryClient.setQueryData(draftKey(envId, stackId), (old: StackDraft | undefined) =>
        old
          ? {
              ...old,
              version: answer.version,
              savedBy: answer.savedBy,
              savedAt: answer.savedAt,
              // The cache holds what the server would answer, not what was typed: a secret's
              // text never enters it (UI-SPEC "Design System").
              files: (body.files ?? []).map((f) =>
                f.secret || sealed.has(f.path) ? { ...f, secret: true, content: HIDDEN } : f,
              ),
              vars: (body.vars ?? []).map((v) => (v.secret ? { ...v, value: HIDDEN } : v)),
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
): Promise<{
  problems: StackProblem[];
  unusedFiles: string[];
  variables: StackVariable[];
  secretFiles: string[];
}> {
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
    return {
      problems: res.data.problems ?? [],
      unusedFiles: res.data.unusedFiles ?? [],
      variables: res.data.variables ?? [],
      secretFiles: res.data.secretFiles ?? [],
    };
  } catch (e) {
    // A network failure has no status.
    throw e instanceof CheckRequestError ? e : new CheckRequestError(0);
  }
}

// RevealRequestError is a reveal the server refused or that got no answer. The status is 0 when
// the server could not be reached; 403 means the caller does not hold secrets.reveal.
export class RevealRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST reveal failed (${status})`);
    this.name = "RevealRequestError";
    this.status = status;
    this.type = type;
  }
}

// revealVariable asks for the saved value of a secret variable. It is a plain function and not a
// query on purpose: the value must never enter the query cache (UI-SPEC "Design System"). The
// caller holds it in component state for a minute and drops it. Each call is audited by the
// server, so it is never retried and never made ahead of a click.
export async function revealVariable(
  envId: string,
  stackId: string,
  name: string,
): Promise<string> {
  try {
    const res = await api.POST("/api/env/{envId}/stacks/{stackId}/variables/{name}/reveal", {
      params: { path: { envId, stackId, name } },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new RevealRequestError(problem.status, problem.type);
    }
    return res.data.value;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof RevealRequestError ? e : new RevealRequestError(0);
  }
}

// RenderRequestError is a render the server refused or that got no answer. The status is 0 when
// the server could not be reached; 422 with the type stack_invalid means the working copy has
// errors, so there is no stack to render yet; 429 check_busy means two checks are already running.
export class RenderRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST render failed (${status})`);
    this.name = "RenderRequestError";
    this.status = status;
    this.type = type;
  }
}

// renderStack asks for the stack as it will be deployed: the working copy with its variables
// filled in and its images shown as the pins they will run, every secret as <hidden>. It is a
// plain function and not a query: the answer is the first full view of the resolved values, so it
// is held in component state and never enters the query cache. It reads and writes nothing on the
// server, so it is safe to repeat; it is never retried, because the pane asks again when the copy
// changes.
export async function renderStack(
  envId: string,
  stackId: string,
  copy: StackCheckInput,
  signal?: AbortSignal,
): Promise<StackRender> {
  try {
    const res = await api.POST("/api/env/{envId}/stacks/{stackId}/render", {
      params: { path: { envId, stackId } },
      body: copy,
      signal,
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new RenderRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof RenderRequestError ? e : new RenderRequestError(0);
  }
}

// revealFile asks for the saved content of a secret stack file. Like revealVariable it is a plain
// function and not a query, so the content never enters the query cache.
export async function revealFile(envId: string, stackId: string, path: string): Promise<string> {
  try {
    const res = await api.POST("/api/env/{envId}/stacks/{stackId}/files/reveal", {
      params: { path: { envId, stackId } },
      body: { path },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new RevealRequestError(problem.status, problem.type);
    }
    return res.data.content;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof RevealRequestError ? e : new RevealRequestError(0);
  }
}

// What a review is of: the saved draft (deploy), or a newly looked-up digest for some services
// (update).
export type ReviewMode = "deploy" | "update";

// ReviewRequestError is a review the server refused or that got no answer. The status is 0 when
// the server could not be reached; the type is the problem type it named (target_changed,
// target_engine_missing ...).
export class ReviewRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST review failed (${status})`);
    this.name = "ReviewRequestError";
    this.status = status;
    this.type = type;
  }
}

// The query key of one review. Under its own prefix and not under stackKey: a refresh of the page
// (a poll, the events feed) must not look the digests up again behind a review that holds the
// ones the person is looking at.
export function reviewKey(envId: string, stackId: string, mode: ReviewMode, services: string[]) {
  return ["stack-review", envId, stackId, mode, services.join(",")] as const;
}

// The review of a stack: what a deploy would change against what is running, the images with the
// digests it would use, and what stops it. It saves nothing and sends nothing to an engine, so it
// is a read even though it is a POST. It is looked at once: it is read again only when the person
// asks (Retry loading the review), because a second answer may carry other digests than the ones
// on screen. It is never retried, and not kept when the page closes: the answer holds the specs
// of the stack, and the server has already masked its secrets (T-07-72).
export function reviewQuery(
  envId: string,
  stackId: string,
  mode: ReviewMode = "deploy",
  services: string[] = [],
) {
  return queryOptions({
    queryKey: reviewKey(envId, stackId, mode, services),
    queryFn: async ({ signal }): Promise<StackReview> => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/review", {
          params: { path: { envId, stackId } },
          body: mode === "update" ? { mode, services } : { mode },
          signal,
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new ReviewRequestError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof ReviewRequestError ? e : new ReviewRequestError(0);
      }
    },
    retry: false,
    gcTime: 0,
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

// DeployRequestError is a deploy the server refused or that got no answer. The status is 0 when
// the server could not be reached; the type is the problem type it named (draft_changed,
// pins_invalid, privileged_compose, stack_name_clash, target_changed, too_many_deploys ...).
export class DeployRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  constructor(status: number, type?: string, fields: ProblemField[] = []) {
    super(`POST deploys failed (${status})`);
    this.name = "DeployRequestError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }
}

// useDeployStack starts the deploy of what was reviewed: the version of the draft the review was
// made of and the digests it chose, so the server deploys exactly that and refuses a draft that
// moved since (409 draft_changed). It is never retried: a deploy whose answer was lost may have
// started, and the stack's page, read again, shows whether it did. The stack's page and the list
// are read again whatever the answer.
export function useDeployStack(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDeployed, DeployRequestError, StackDeployInput>({
    mutationFn: async (body) => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/deploys", {
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
    onSettled: () => void queryClient.invalidateQueries({ queryKey: stacksKey(envId) }),
  });
}

// The query key of one deploy job.
export function stackJobKey(envId: string, stackId: string, jobId: string) {
  return ["stack-job", envId, stackId, jobId] as const;
}

// isJobGone says whether the error is the server answering that the job is not listed: a finished
// job stays for ten minutes, and the address may name the job of another stack.
export function isJobGone(error: unknown): boolean {
  return (
    error instanceof StacksLoadError && error.status === 404 && error.type === "stack_job_not_found"
  );
}

// isJobRunning says whether a job is still going, so a view of it keeps asking.
export function isJobRunning(job: StackJob): boolean {
  return job.state === "running";
}

// How often a view of a running job asks for it again.
export const JOB_REFETCH_MS = 1000;

// One deploy job and its steps, asked for again every second while it runs and not after it ends.
// A job that is gone is not asked for again: it will not come back on a second try.
export function stackJobQuery(envId: string, stackId: string, jobId: string) {
  return queryOptions({
    queryKey: stackJobKey(envId, stackId, jobId),
    queryFn: async (): Promise<StackJob> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/deploys/{jobId}", {
        params: { path: { envId, stackId, jobId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isJobGone(error) && count < 3,
    refetchInterval: (query) => {
      if (isJobGone(query.state.error)) return false;
      const job = query.state.data;
      return job !== undefined && !isJobRunning(job) ? false : JOB_REFETCH_MS;
    },
    refetchOnWindowFocus: false,
  });
}
