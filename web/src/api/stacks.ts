// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type QueryClient, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

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
export type StackUpdate = components["schemas"]["StackUpdateBody"];
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
export type StackConcurrent = components["schemas"]["StackConcurrentBody"];
export type StackFileDiff = components["schemas"]["StackFileDiffBody"];
export type StackReviewSummary = components["schemas"]["StackReviewSummaryBody"];
export type StackDeployInput = components["schemas"]["StackDeployRequest"];
export type StackDeployed = components["schemas"]["StackDeployBody"];
export type StackJob = components["schemas"]["StackJobBody"];
export type StackJobItem = components["schemas"]["StackJobItemBody"];
export type StackLock = components["schemas"]["StackLockBody"];
export type StackRollback = components["schemas"]["StackRollbackBody"];
export type StackLifecycleStarted = components["schemas"]["StackLifecycleBody"];
export type RemovalPreview = components["schemas"]["RemovalPreviewBody"];
export type RemovalVolume = components["schemas"]["RemovalVolumeBody"];
export type StackRemoveInput = components["schemas"]["RemoveStackRequest"];
export type StackRemoveStarted = components["schemas"]["RemoveStackBody"];
export type StackJobSummary = components["schemas"]["StackJobSummary"];
export type StackJobOutputLine = components["schemas"]["StackJobOutputLine"];
export type StackServices = components["schemas"]["StackServicesBody"];
export type StackService = components["schemas"]["StackServiceBody"];
export type StackContainer = components["schemas"]["StackContainerBody"];
export type StackResources = components["schemas"]["StackResourcesBody"];
export type StackResourceTask = components["schemas"]["StackResourceTaskBody"];
export type StackResourceContainer = components["schemas"]["StackContainerResourceBody"];
export type StackResourceNetwork = components["schemas"]["StackNetworkResourceBody"];
export type StackResourceVolume = components["schemas"]["StackVolumeResourceBody"];
export type StackResourceObject = components["schemas"]["StackObjectResourceBody"];
export type StackUnmanaged = components["schemas"]["StackUnmanagedBody"];
export type StackKindCounts = components["schemas"]["StackKindCountsBody"];
export type AllStacks = components["schemas"]["AllStacksBody"];
export type AllStacksGroup = components["schemas"]["AllStacksGroup"];
export type FailedEnvironment = components["schemas"]["FailedEnvironmentBody"];

// The query key of an environment's stacks list, and of every read that extends it: one stack's
// page is under it, so a change to the list refreshes the page too.
export function stacksKey(envId: string) {
  return listKey(envId, "stacks");
}

// The query key of one stack's page.
export function stackKey(envId: string, stackId: string) {
  return [...stacksKey(envId), stackId] as const;
}

// The query key of the list of stack jobs the header's jobs indicator shows.
export const stackJobsKey = ["stack-jobs"] as const;

// Polled every 3 seconds while a stack job runs and every 30 otherwise, as the Pulls list is.
export const STACK_JOBS_POLL_RUNNING_MS = 3_000;
export const STACK_JOBS_POLL_IDLE_MS = 30_000;

// stackJobsListQuery lists the stack operations the caller may follow: those running or ended in the
// last ten minutes, on the environments they may read stacks on. A poll that fails keeps the last list,
// so the indicator shows no error chrome for it (the query keeps its data when a refetch fails).
export const stackJobsListQuery = queryOptions({
  queryKey: [...stackJobsKey, "list"] as const,
  queryFn: async (): Promise<StackJobSummary[]> => {
    const res = await api.GET("/api/stack-jobs");
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new StacksLoadError(problem.status, problem.type);
    }
    return res.data.jobs ?? [];
  },
  refetchInterval: (query) =>
    query.state.data?.some((j) => j.state === "running")
      ? STACK_JOBS_POLL_RUNNING_MS
      : STACK_JOBS_POLL_IDLE_MS,
  retry: false,
});

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

// What the stack lists ask the server for: the search text, the status chips as a comma list, and the
// sort. They are applied before the 500-row cap, so the server decides which rows are shown.
export interface StackFilters {
  q?: string;
  status?: string;
  // managed or unmanaged; absent lists both.
  kind?: "managed" | "unmanaged";
  sort?: "name" | "state" | "lastDeployed";
  dir?: "asc" | "desc";
  // available: only stacks with a service whose image has an update available.
  updates?: "available";
}

// The filters that are set, so an unfiltered read keeps the bare key and a filtered one extends it.
function filterSet<T extends StackFilters>(filters: T): T {
  const set = {} as T;
  for (const key of Object.keys(filters) as (keyof T)[]) {
    if (filters[key]) set[key] = filters[key];
  }
  return set;
}

// Changing the search, the chips or the sort keeps the rows on screen until the new ones arrive, so
// the table does not jump to a skeleton. It never carries rows over from another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

// The stacks of an environment, narrowed and ordered by the filters. The unfiltered list keeps the bare
// key and every filtered read extends it, so one invalidation refreshes them all.
export function stacksQuery(envId: string, filters: StackFilters = {}) {
  const set = filterSet(filters);
  const filtered = Object.keys(set).length > 0;
  return queryOptions({
    queryKey: filtered ? ([...stacksKey(envId), set] as const) : stacksKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/stacks", {
        params: { path: { envId }, query: filtered ? set : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

// The query key of the All stacks list. It is the first element "stacks", not an environment's key,
// so an environment's own events do not refresh it: it follows its own poll.
export const allStacksKey = ["stacks", "all"] as const;

// What the All stacks list asks the server for: the environments chosen (empty is all of them) and the
// same filters as an environment's list.
export interface AllStacksFilters extends StackFilters {
  env?: string;
}

// Every stack the signed-in user may read, grouped by environment, narrowed by the filters. The rows
// of the previous filters stay on screen while the new ones load.
export function allStacksQuery(filters: AllStacksFilters = {}) {
  const set = filterSet(filters);
  return queryOptions({
    queryKey: [...allStacksKey, set] as const,
    queryFn: async () => {
      const res = await api.GET("/api/stacks", {
        params: { query: Object.keys(set).length > 0 ? set : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    placeholderData: (previous) => previous,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// invalidateStacks reads an environment's stack lists and pages again, and the All stacks list too: a
// change to a stack is in both.
export function invalidateStacks(queryClient: QueryClient, envId: string): void {
  void queryClient.invalidateQueries({ queryKey: stacksKey(envId) });
  void queryClient.invalidateQueries({ queryKey: allStacksKey });
}

// How often a stack's page reads itself again while an operation runs on it.
export const STACK_BUSY_REFETCH_MS = 2_000;

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
    // The page reads itself again every 2 s while an operation runs on the stack, so its state, its
    // banner and its lock follow the operation, and at the lists' pace otherwise.
    refetchInterval: (query) =>
      query.state.data?.lastOperation?.state === "running"
        ? STACK_BUSY_REFETCH_MS
        : LIST_REFETCH_MS,
  });
}

// How often the Overview of a swarm stack reads what runs again while the page is visible: Docker
// sends no task events, so it polls (UI-SPEC "Shared state and overflow rules"), faster while a
// stack operation runs. A standalone host emits container events, which refresh it through the
// stacks key, and falls back to the lists' 30 s.
export const STACK_SERVICES_POLL_MS = 5_000;
export const STACK_SERVICES_POLL_BUSY_MS = 2_000;

// The query key of what one stack runs. It extends the stack's key, so the events feed that
// refreshes the stacks list, and every action that refreshes the page, refresh it too.
export function stackServicesKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "services"] as const;
}

interface StackServicesOptions {
  // Where the stack deploys: only a swarm needs polling.
  target: StackDetail["target"];
  // True while a stack operation runs on the environment.
  busy?: boolean;
}

// What a stack runs now, read from the engine. It polls every 5 s (2 s during an operation) on a
// swarm while the page is visible and not at all while it is hidden, and a read that fails keeps
// the last rows, so the card can say the read failed and still show what it had.
export function stackServicesQuery(envId: string, stackId: string, o: StackServicesOptions) {
  return queryOptions({
    queryKey: stackServicesKey(envId, stackId),
    queryFn: async (): Promise<StackServices> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/services", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 1,
    refetchInterval: () => {
      if (document.visibilityState !== "visible") return false;
      if (o.target !== "swarm") return LIST_REFETCH_MS;
      return o.busy ? STACK_SERVICES_POLL_BUSY_MS : STACK_SERVICES_POLL_MS;
    },
  });
}

// How often the Resources tab of a swarm stack reads again while the page is visible: Docker sends no
// task events, so it polls like the Overview does (UI-SPEC "Shared state and overflow rules"), faster
// while a stack operation runs.
export const STACK_RESOURCES_POLL_MS = 5_000;
export const STACK_RESOURCES_POLL_BUSY_MS = 2_000;

// The query key of what one stack owns. It extends the stack's key, so the events feed that refreshes the
// stacks list, and every action that refreshes the page, refresh it too.
export function stackResourcesKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "resources"] as const;
}

// What a stack owns on its environment, one card for each kind, each with its own error. It polls on a
// swarm while the page is visible and not at all while it is hidden; a read that fails keeps the last
// cards, so the page can say it failed and still show what it had.
export function stackResourcesQuery(envId: string, stackId: string, o: StackServicesOptions) {
  return queryOptions({
    queryKey: stackResourcesKey(envId, stackId),
    queryFn: async (): Promise<StackResources> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/resources", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isStackGone(error) && count < 1,
    refetchInterval: () => {
      if (document.visibilityState !== "visible") return false;
      if (o.target !== "swarm") return LIST_REFETCH_MS;
      return o.busy ? STACK_RESOURCES_POLL_BUSY_MS : STACK_RESOURCES_POLL_MS;
    },
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

// The environment a stack is created on, and what it starts from. The All stacks list's dialog chooses
// the environment, so it is a variable of the mutation and not of the hook.
export interface StackCreateVariables {
  envId: string;
  body: StackCreateInput;
}

// useCreateStack is the mutation behind the Create stack dialog. It does not retry, and reads
// the lists again whatever the answer.
export function useCreateStack() {
  const queryClient = useQueryClient();
  return useMutation<StackDetail, StackRequestError, StackCreateVariables>({
    mutationFn: ({ envId, body }) => postStack(envId, body),
    retry: false,
    onSettled: (_made, _error, { envId }) => invalidateStacks(queryClient, envId),
  });
}

export type StackNameCheck = components["schemas"]["StackNameCheckBody"];

// checkStackName asks whether a name can be used on an environment: available, taken, clash or invalid,
// with the sentence to show. It only reads. A check that cannot be answered throws, and the caller goes
// on: the create decides again.
export async function checkStackName(
  envId: string,
  name: string,
  signal?: AbortSignal,
): Promise<StackNameCheck> {
  try {
    const res = await api.GET("/api/env/{envId}/stacks/name-check", {
      params: { path: { envId }, query: { name } },
      signal,
    });
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
      invalidateStacks(queryClient, envId);
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
    onSuccess: () => invalidateStacks(queryClient, envId),
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

// What a review is of: the saved draft (deploy), a newly looked-up digest for some services (update), or
// an earlier revision with the digests it stored (rollback), or the deployed revision again to put back what was
// changed outside the stack (restore).
export type ReviewMode = "deploy" | "update" | "rollback" | "restore";

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
// A roll back review adds the revision it restores, so two revisions are two reviews.
export function reviewKey(
  envId: string,
  stackId: string,
  mode: ReviewMode,
  services: string[],
  revision?: number,
) {
  const base = ["stack-review", envId, stackId, mode, services.join(",")] as const;
  return revision === undefined ? base : ([...base, revision] as const);
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
  revision?: number,
) {
  return queryOptions({
    queryKey: reviewKey(envId, stackId, mode, services, revision),
    queryFn: async ({ signal }): Promise<StackReview> => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/review", {
          params: { path: { envId, stackId } },
          body:
            mode === "update"
              ? { mode, services }
              : mode === "rollback"
                ? { mode, revision }
                : { mode },
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
    onSettled: () => {
      invalidateStacks(queryClient, envId);
      // The jobs indicator learns of the new job at once, not on its next poll.
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
  });
}

// useRollbackStack starts a roll back to the last version that ran well. It sends nothing the person
// chose: the server decides what it restores, with that version's own images, and the answer is the
// job to open. It is never retried: a roll back whose answer was lost may have started, and the stack's
// page, read again, shows whether it did. The stack's page and the list are read again whatever the
// answer.
export function useRollbackStack(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDeployed, DeployRequestError, void>({
    mutationFn: async () => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/rollback", {
          params: { path: { envId, stackId } },
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
      // One stack's page is under the list's key, so this reads both again.
      invalidateStacks(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
  });
}

// useStopStack and useStartStack park a stack and bring it back (STK-11). Neither sends anything the person
// chose: the server decides what to stop and what to bring back, and the answer is the job. They are never
// retried: a request whose answer was lost may have started, and the stack's page, read again, shows
// whether it did. The stack's page and the list are read again whatever the answer.
function useStackLifecycle(
  envId: string,
  call: () => Promise<{ data?: StackLifecycleStarted; error?: unknown; response: Response }>,
) {
  const queryClient = useQueryClient();
  return useMutation<StackLifecycleStarted, DeployRequestError, void>({
    mutationFn: async () => {
      try {
        const res = await call();
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
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
  });
}

export function useStopStack(envId: string, stackId: string) {
  return useStackLifecycle(envId, () =>
    api.POST("/api/env/{envId}/stacks/{stackId}/stop", { params: { path: { envId, stackId } } }),
  );
}

export function useStartStack(envId: string, stackId: string) {
  return useStackLifecycle(envId, () =>
    api.POST("/api/env/{envId}/stacks/{stackId}/start", { params: { path: { envId, stackId } } }),
  );
}

// The query key of what removing one stack would delete. It extends the stack's key, so the events feed and
// every action that refreshes the page read it again.
export function removalPreviewKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "removal-preview"] as const;
}

// What a removal of the stack would delete, read from the engine when the Remove dialog opens. It is never
// kept: a list of volumes that is minutes old is not one to tick from (the server checks every name again, but
// the person should see what is there now), so it is read afresh each time and not again while the dialog stays.
export function removalPreviewQuery(envId: string, stackId: string) {
  return queryOptions({
    queryKey: removalPreviewKey(envId, stackId),
    queryFn: async (): Promise<RemovalPreview> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/removal-preview", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    gcTime: 0,
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

// useRemoveStack removes a stack: the name typed, the volumes ticked and whether the definition stays. It is
// never retried: a removal whose answer was lost may have started, and the stack's page, read again, shows
// whether it did. The stack's page and the list are read again whatever the answer.
export function useRemoveStack(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackRemoveStarted, DeployRequestError, StackRemoveInput>({
    mutationFn: async (body) => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/remove", {
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
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
  });
}

// CancelRequestError is a cancel the server refused or that got no answer. The status is 0 when the
// server could not be reached; the type is job_finished for a deploy that had already ended.
export class CancelRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST cancel failed (${status})`);
    this.name = "CancelRequestError";
    this.status = status;
    this.type = type;
  }
}

// useCancelStackJob asks a running deploy to stop at the step it has reached. It is never retried:
// a second cancel is the person's to send. The job is read again whatever the answer, so the page
// shows what became of it.
export function useCancelStackJob(envId: string, stackId: string, jobId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, CancelRequestError, void>({
    mutationFn: async () => {
      try {
        const res = await api.POST("/api/env/{envId}/stacks/{stackId}/deploys/{jobId}/cancel", {
          params: { path: { envId, stackId, jobId } },
        });
        if (!res.response.ok) {
          const problem = readProblem(res.error, res.response);
          throw new CancelRequestError(problem.status, problem.type);
        }
      } catch (e) {
        // A network failure has no status.
        throw e instanceof CancelRequestError ? e : new CancelRequestError(0);
      }
    },
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: stackJobKey(envId, stackId, jobId) });
      void queryClient.invalidateQueries({ queryKey: stackJobsKey });
    },
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
