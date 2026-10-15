// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type ServiceItem = components["schemas"]["ServiceItem"];
export type ServiceScaleTarget = components["schemas"]["ServiceScaleTarget"];
export type ServiceScaleResult = components["schemas"]["ServiceScaleResult"];
export type ServiceScaleCode = NonNullable<ServiceScaleResult["code"]>;
export type ServiceDetail = components["schemas"]["ServiceDetailBody"];
export type ServiceInspect = components["schemas"]["ServiceInspectBody"];

// The query key of an environment's services list, and of every read that extends it.
export function servicesKey(envId: string) {
  return listKey(envId, "services");
}

// What narrows the services list on the server, before its 500-row cap: the search text and
// the comma list of statuses (degraded, updating, problem). Each is left out when it is not
// set.
export interface ServiceFilters {
  q?: string;
  status?: string;
}

// ServicesLoadError is a services list the server refused or could not answer. The status and
// the problem type tell a standalone host (409 not_a_swarm) from every other failure.
export class ServicesLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET services failed (${status})`);
    this.name = "ServicesLoadError";
    this.status = status;
    this.type = type;
  }
}

// isNotASwarm says whether the error is the server answering that the environment is a
// standalone host, which has no services.
export function isNotASwarm(error: unknown): boolean {
  return error instanceof ServicesLoadError && error.status === 409 && error.type === "not_a_swarm";
}

// Changing the search or the chips keeps the rows on screen until the new ones arrive, so
// the table does not jump to a skeleton. It never carries rows over from another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

// The services of a swarm environment, narrowed by the filters. The unfiltered list keeps the
// bare key and every filtered read extends it, so one invalidation refreshes them all.
export function servicesQuery(envId: string, filters: ServiceFilters = {}) {
  const set: ServiceFilters = {};
  if (filters.q) set.q = filters.q;
  if (filters.status) set.status = filters.status;
  const filtered = Object.keys(set).length > 0;
  return queryOptions({
    queryKey: filtered ? ([...servicesKey(envId), set] as const) : servicesKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services", {
        params: { path: { envId }, query: filtered ? set : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServicesLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    placeholderData: keepForSameEnv(envId),
    // A standalone host will not turn into a swarm on a second try.
    retry: (count, error) => !isNotASwarm(error) && count < 3,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// ScaleRequestError is a scale the server refused as a whole (permission lost, a count out of
// range, a standalone host) or that got no answer. The status is 0 when the server could not
// be reached; the type is the problem type the server named. A service that failed is not this:
// it is a result.
export class ScaleRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST services/scale failed (${status})`);
    this.name = "ScaleRequestError";
    this.status = status;
    this.type = type;
  }
}

// isLostScale says whether a request got no answer at all. The server sees a write through, so
// it may well have happened: the list, read again after every request, shows the outcome.
export function isLostScale(err: unknown): boolean {
  return err instanceof ScaleRequestError && err.status === 0;
}

// The most targets one scale request takes (the server refuses more with 422).
export const SCALE_CHUNK = 100;

// What a scale asks for: the services and their counts, and the bulk ID the audit rows of the
// operation share (D-14) when it is more than one service.
export interface ScaleRequest {
  targets: ServiceScaleTarget[];
  bulkId?: string;
}

// postScale sets the replica count of one or more services. It never retries: a write whose
// answer was lost may have happened (P2 D-42), and the person asks again. The server takes 100
// targets at a time, so a longer list goes out in requests one after the other that share the
// bulk ID; if the first is refused as a whole that is thrown, and the targets of a later one
// that is refused are answered as failed while the earlier ones stand.
export async function postScale(
  envId: string,
  { targets, bulkId }: ScaleRequest,
): Promise<ServiceScaleResult[]> {
  const results: ServiceScaleResult[] = [];
  for (let at = 0; at < targets.length; at += SCALE_CHUNK) {
    const chunk = targets.slice(at, at + SCALE_CHUNK);
    try {
      results.push(...(await postScaleChunk(envId, chunk, bulkId)));
    } catch (e) {
      if (at === 0) throw e;
      for (const t of chunk) {
        results.push({
          serviceId: t.serviceId,
          name: t.serviceId,
          result: "failed",
          code: "scale_failed",
          replicas: t.replicas,
          changed: false,
        });
      }
    }
  }
  return results;
}

async function postScaleChunk(
  envId: string,
  targets: ServiceScaleTarget[],
  bulkId: string | undefined,
): Promise<ServiceScaleResult[]> {
  try {
    const res = await api.POST("/api/env/{envId}/services/scale", {
      params: { path: { envId } },
      body: { targets, bulkId },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ScaleRequestError(problem.status, problem.type);
    }
    return res.data.results ?? [];
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ScaleRequestError ? e : new ScaleRequestError(0);
  }
}

// useScaleServices is the mutation behind the scale dialog. It does not retry, and reads the
// list again whatever the answer: a write whose answer was lost may have happened.
export function useScaleServices(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ServiceScaleResult[], ScaleRequestError, ScaleRequest>({
    mutationKey: serviceActionKey(envId),
    mutationFn: (request) => postScale(envId, request),
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: servicesKey(envId) });
    },
  });
}

// ServiceLoadError is a service read the server refused or could not answer. The status and the
// problem type tell a service that no longer exists (404 service_not_found) from every other
// failure.
export class ServiceLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET service failed (${status})`);
    this.name = "ServiceLoadError";
    this.status = status;
    this.type = type;
  }
}

// isServiceGone says whether the error is the swarm answering that the service does not exist:
// it was removed.
export function isServiceGone(error: unknown): boolean {
  return (
    error instanceof ServiceLoadError && error.status === 404 && error.type === "service_not_found"
  );
}

// How many times a failed service read is tried again. A service that does not exist is never
// tried again. Exported so tests can set it to none.
export const serviceRetry = { attempts: 1 };

// The key of one service's detail. It extends the services list key, so the events feed and
// every action, which refresh that key, refresh an open service page too.
export function serviceKey(envId: string, id: string) {
  return [...servicesKey(envId), "detail", id] as const;
}

// The settings of one service. The server has already withheld every value the caller may not
// see.
export function serviceQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: serviceKey(envId, id),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services/{serviceId}", {
        params: { path: { envId, serviceId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServiceLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isServiceGone(error) && count < serviceRetry.attempts,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// The raw inspect document of one service, under the key of its detail, so the events feed and
// every action refresh an open Inspect tab with the page. The server has already replaced every
// value the caller may not see.
export function serviceInspectQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...serviceKey(envId, id), "inspect"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services/{serviceId}/inspect", {
        params: { path: { envId, serviceId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServiceLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isServiceGone(error) && count < serviceRetry.attempts,
    refetchInterval: LIST_REFETCH_MS,
  });
}

export type ServiceTasks = components["schemas"]["ServiceTasksBody"];

// How often an open Tasks tab reads again while the page is visible. Docker sends no task
// events, so the tab polls (UI-SPEC "Shared state and overflow rules").
export const TASKS_REFETCH_MS = 5000;

// The tasks of one service, under the key of its detail, so the events feed and every action
// that refresh the service refresh an open Tasks tab too. It polls every 5 s while the page is
// visible and not at all while it is hidden.
export function serviceTasksQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...serviceKey(envId, id), "tasks"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services/{serviceId}/tasks", {
        params: { path: { envId, serviceId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServiceLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isServiceGone(error) && count < serviceRetry.attempts,
    refetchInterval: () => (document.visibilityState === "visible" ? TASKS_REFETCH_MS : false),
  });
}

export type ServiceSpec = components["schemas"]["ServiceSpecBody"];
export type ServiceForceUpdateResult = components["schemas"]["ServiceForceUpdateBody"];
export type ServiceRollbackResult = components["schemas"]["ServiceRollbackBody"];

// The spec of one service and the one before its last update, under the key of its detail, so
// the events feed and every action refresh an open dialog or editor with the page. The server has
// already replaced every value the caller may not see with <hidden>. The spec carries a version,
// so it is read again each time a dialog opens (gcTime 0 keeps a stale one from being shown).
export function serviceSpecQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...serviceKey(envId, id), "spec"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services/{serviceId}/spec", {
        params: { path: { envId, serviceId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServiceLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isServiceGone(error) && count < serviceRetry.attempts,
    gcTime: 0,
    staleTime: 0,
  });
}

// ServiceActionError is a force update or a rollback the server refused or that got no answer.
// The status is 0 when the server could not be reached; the type is the problem type the server
// named; the fields are what the problem listed (the image and its registry host for
// image_unresolved, the kind and name of what is gone for rollback_object_missing); the detail
// is the manager's own words where the problem carries them.
export class ServiceActionError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ReadonlyMap<string, string>;
  readonly detail?: string;
  constructor(
    status: number,
    type?: string,
    fields: ReadonlyMap<string, string> = new Map(),
    detail?: string,
  ) {
    super(`POST service action failed (${status})`);
    this.name = "ServiceActionError";
    this.status = status;
    this.type = type;
    this.fields = fields;
    this.detail = detail;
  }
}

// isLostAction says whether a service action got no answer at all. The server sees a write
// through, so it may well have happened: the page, read again after every action, shows it.
export function isLostAction(err: unknown): boolean {
  return err instanceof ServiceActionError && err.status === 0;
}

function actionError(body: unknown, response?: { status: number }): ServiceActionError {
  const problem = readProblem(body, response);
  const detail =
    typeof body === "object" && body !== null && "detail" in body && typeof body.detail === "string"
      ? body.detail
      : undefined;
  return new ServiceActionError(
    problem.status,
    problem.type,
    new Map(problem.fields.map((f) => [f.location, f.message])),
    detail,
  );
}

function refreshService(queryClient: ReturnType<typeof useQueryClient>, envId: string) {
  void queryClient.invalidateQueries({ queryKey: servicesKey(envId) });
}

export interface ForceUpdateRequest {
  serviceId: string;
  // True when this repeats a stalled or failed update (Retry update).
  retry?: boolean;
}

// useForceUpdateService is the mutation behind the force update and retry dialogs. It does not
// retry, and reads the service again whatever the answer: a write whose answer was lost may have
// happened (P2 D-42).
export function useForceUpdateService(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ServiceForceUpdateResult, ServiceActionError, ForceUpdateRequest>({
    mutationKey: serviceActionKey(envId),
    mutationFn: async ({ serviceId, retry }) => {
      try {
        const res = await api.POST("/api/env/{envId}/services/{serviceId}/force-update", {
          params: { path: { envId, serviceId } },
          body: retry ? { retry: true } : {},
        });
        if (!res.data) throw actionError(res.error, res.response);
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof ServiceActionError ? e : new ServiceActionError(0);
      }
    },
    retry: false,
    onSettled: () => refreshService(queryClient, envId),
  });
}

export interface RollbackRequest {
  serviceId: string;
  // The version the diff was shown at: the rollback goes back only to the spec that was shown.
  version: number;
}

// useRollbackService is the mutation behind the rollback dialog, with the same rules as the force
// update's.
export function useRollbackService(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ServiceRollbackResult, ServiceActionError, RollbackRequest>({
    mutationKey: serviceActionKey(envId),
    mutationFn: async ({ serviceId, version }) => {
      try {
        const res = await api.POST("/api/env/{envId}/services/{serviceId}/rollback", {
          params: { path: { envId, serviceId } },
          body: { version },
        });
        if (!res.data) throw actionError(res.error, res.response);
        return res.data;
      } catch (e) {
        throw e instanceof ServiceActionError ? e : new ServiceActionError(0);
      }
    },
    retry: false,
    onSettled: () => refreshService(queryClient, envId),
  });
}

export interface RemoveRequest {
  serviceId: string;
  // The service's name, typed. Sent only to remove Bayplan's own agent service.
  confirm?: string;
}

// useRemoveService is the mutation behind the remove dialog. It does not retry, and reads the
// services again whatever the answer, so a service that was already gone leaves the list too: a
// write whose answer was lost may have happened (P2 D-42).
export function useRemoveService(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, ServiceActionError, RemoveRequest>({
    mutationKey: serviceActionKey(envId),
    mutationFn: async ({ serviceId, confirm }) => {
      try {
        const res = await api.DELETE("/api/env/{envId}/services/{serviceId}", {
          params: { path: { envId, serviceId }, query: confirm ? { confirm } : undefined },
        });
        if (!res.response.ok) throw actionError(res.error, res.response);
      } catch (e) {
        throw e instanceof ServiceActionError ? e : new ServiceActionError(0);
      }
    },
    retry: false,
    onSettled: () => refreshService(queryClient, envId),
  });
}

export type ServiceRollout = components["schemas"]["RolloutBody"];
export type RolloutState = ServiceRollout["state"];

// How often a rollout is read while an update or a rollback runs (UI-SPEC "Rollout progress").
export const ROLLOUT_REFETCH_MS = 2000;

// Whether a rollout is still running: the swarm is replacing tasks.
export function isRolloutRunning(state: RolloutState | undefined): boolean {
  return state === "updating" || state === "rollback_started";
}

// How far one service's rolling update has got and the error of its newest failed task, under the
// key of its detail, so the events feed and every action that refresh the service refresh the
// rollout too. It is read every 2 s while an update or rollback runs and the page is visible, at
// the list's pace otherwise, and not at all while the page is hidden.
export function serviceRolloutQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...serviceKey(envId, id), "rollout"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services/{serviceId}/rollout", {
        params: { path: { envId, serviceId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServiceLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isServiceGone(error) && count < serviceRetry.attempts,
    refetchInterval: (query) => {
      if (document.visibilityState !== "visible") return false;
      return isRolloutRunning(query.state.data?.state) ? ROLLOUT_REFETCH_MS : LIST_REFETCH_MS;
    },
  });
}

export type ServiceReview = components["schemas"]["ServiceReviewBody"];
export type ServiceApplyResult = components["schemas"]["ServiceApplyBody"];

export interface EditRequest {
  serviceId: string;
  // The version of the service the spec was loaded at.
  version: number;
  spec: Record<string, unknown>;
}

export function useReviewServiceUpdate(_envId: string) {
  return useMutation<ServiceReview, ServiceActionError, EditRequest>({
    mutationFn: async () => {
      throw new ServiceActionError(0);
    },
  });
}

export function useApplyServiceUpdate(_envId: string) {
  return useMutation<ServiceApplyResult, ServiceActionError, EditRequest>({
    mutationFn: async () => {
      throw new ServiceActionError(0);
    },
  });
}

// The key every service action (scale, force update, rollback) carries, so the page can tell one
// is pending.
export function serviceActionKey(envId: string) {
  return ["service-action", envId] as const;
}
