// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  queryOptions,
  useIsMutating,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useCallback, useEffect, useReducer, useState } from "react";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import type { StackJob } from "@/api/stacks";
import { useEnvName } from "@/features/resources/use-env-name";
import {
  initialStackStream,
  openStackJobStream,
  reduceStackStream,
  type StackFrame,
  type StackJobFollow,
} from "@/features/stack-deploy/stack-job-stream";
import { shortDigest } from "@/features/tags/platform-chips";
import { useT } from "@/i18n/use-t";
import { apiUrl } from "@/lib/base-path";
import { toastError, toastInfo, toastSuccess, toastSuccessWithAction } from "@/lib/toast";

export type UpdateInfo = components["schemas"]["UpdateInfoBody"];
export type UpdateState = UpdateInfo["state"];
export type UpdateReason = UpdateInfo["reason"];
export type CheckUpdatesResult = components["schemas"]["CheckUpdatesBody"];
export type UpdateCheckStatus = components["schemas"]["UpdateCheckStatusBody"];
export type UpdateCheckPolicyRequest = components["schemas"]["UpdateCheckPolicyRequest"];
export type UpdateCheckDefaults = components["schemas"]["UpdateCheckDefaultsBody"];
export type UpdateCheckDefaultsRequest = components["schemas"]["UpdateCheckDefaultsRequest"];
export type UpdateSchedulePreview = components["schemas"]["UpdateSchedulePreviewBody"];
export type UpdateSchedulePreset = UpdateCheckDefaults["preset"];

// CheckUpdatesError is a Check now the server refused or could not answer. The status and the
// problem type tell a standalone host (409 not_a_swarm) or a check that outlasted its minute (504
// check_timed_out) from every other failure; the person gets one generic line either way.
export class CheckUpdatesError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST updates/check failed (${status})`);
    this.name = "CheckUpdatesError";
    this.status = status;
    this.type = type;
  }
}

function checkKey(envId: string) {
  return ["env", envId, "updates", "check"] as const;
}

// useCheckingUpdates says whether a Check now of the environment is out. It reads the mutation
// cache, so it is true in every menu of the page, and a second press waits for the first.
export function useCheckingUpdates(envId: string): boolean {
  return useIsMutating({ mutationKey: checkKey(envId) }) > 0;
}

// useCheckUpdates is the mutation behind Check now: it asks the registries again for the
// environment's images. It is never retried (P2 D-42), since a person pressing the button again
// is the retry. The toasts are part of the mutation, not of the call, because the menu that starts
// it closes and unmounts before the answer arrives. Whatever the answer, every list of the
// environment is read again: a check that timed out still stored what it learned, and each list
// carries the update objects.
export function useCheckUpdates(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<CheckUpdatesResult, CheckUpdatesError, void>({
    retry: false,
    mutationKey: checkKey(envId),
    mutationFn: async () => {
      try {
        const res = await api.POST("/api/env/{envId}/updates/check", {
          params: { path: { envId } },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new CheckUpdatesError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof CheckUpdatesError ? e : new CheckUpdatesError(0);
      }
    },
    onSuccess: (result) => {
      toastSuccess(
        `${t("updates.check.checked", { count: result.checked })} ${t("updates.check.found", { count: result.updates })}`,
      );
    },
    onError: (error) => {
      // The server allows one check of an environment per few seconds: a second press is a wait,
      // not a failure.
      if (error.status === 429 || error.type === "too_many_requests") {
        toastInfo(t("updates.check.wait"));
        return;
      }
      toastError(t("updates.check.failed"));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["env", envId] });
    },
  });
}

// UpdateSettingsError is an update-check settings call the server refused or never answered. status
// is 0 when the server could not be reached; type is the problem type it named (settings_changed,
// invalid_schedule, schedule_too_frequent); fields hold what the problem names (changedBy).
export class UpdateSettingsError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  constructor(method: string, status: number, type?: string, fields: ProblemField[] = []) {
    super(`${method} update checks failed (${status})`);
    this.name = "UpdateSettingsError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }

  // fieldOf is what the problem named at a location, empty when it named none.
  fieldOf(location: string): string {
    return this.fields.find((f) => f.location === location)?.message ?? "";
  }
}

function failureOf(method: string, res: { error?: unknown; response: Response }) {
  const problem = readProblem(res.error, res.response);
  return new UpdateSettingsError(method, problem.status, problem.type, problem.fields);
}

// conflictOf is who saved the schedule last, as a 409 settings_changed names them. Empty when the
// server did not know the person.
export function conflictOf(err: UpdateSettingsError): string {
  return err.fieldOf("changedBy");
}

// The key of one environment's update-check status. The schedule previews sit under it, so one
// invalidation of the status makes them stale together. It starts with "env" and the environment, so
// the invalidation after a Check now reaches it too.
export function updateCheckStatusKey(envId: string) {
  return ["env", envId, "update-checks", "status"] as const;
}

// updateCheckStatusQuery is an environment's update-check schedule: for the switch and the schedule
// the value that applies and whether the environment set it itself, when it was last checked, when
// the next check starts, the version to send back with a save and the server's time zone. It is
// read when the page opens and when the window regains focus, not on a timer: a person's unsaved
// edits are kept against the snapshot they began from.
export function updateCheckStatusQuery(envId: string) {
  return queryOptions({
    queryKey: updateCheckStatusKey(envId),
    queryFn: async (): Promise<UpdateCheckStatus> => {
      try {
        const res = await api.GET("/api/env/{envId}/updates", { params: { path: { envId } } });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateSettingsError ? e : new UpdateSettingsError("GET", 0);
      }
    },
  });
}

// useUpdateCheckStatus reads the status; enabled is false for a person who may not read the
// environment.
export function useUpdateCheckStatus(envId: string, enabled = true) {
  return useQuery({ ...updateCheckStatusQuery(envId), enabled });
}

// useSaveUpdateCheckPolicy saves the environment's two settings (a PUT is a full replace: a field
// that is null follows the instance default). It is never retried (P2 D-42). The answer is the status
// as saved and replaces what the page holds. A save made against another version is 409
// settings_changed, and an unusable schedule 422 invalid_schedule or schedule_too_frequent.
export function useSaveUpdateCheckPolicy(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<UpdateCheckStatus, UpdateSettingsError, UpdateCheckPolicyRequest>({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/env/{envId}/updates/policy", {
          params: { path: { envId } },
          body,
        });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateSettingsError ? e : new UpdateSettingsError("PUT", 0);
      }
    },
    onSuccess: (saved) => queryClient.setQueryData(updateCheckStatusKey(envId), saved),
  });
}

// How long after the last keystroke the server is asked what a cron expression would do. The server
// holds a person to 120 calls a minute, so typing never gets near it.
export const UPDATE_SCHEDULE_DEBOUNCE_MS = 400;

export interface UpdateSchedulePreviewState {
  // The server's answer for the expression as it stands now. Undefined while the person is still
  // typing, while the answer is on its way, for a blank expression and when the question failed.
  data: UpdateSchedulePreview | undefined;
  // True from a keystroke until the answer to it has come.
  waiting: boolean;
}

// usePreviewUpdateSchedule asks the server which times an expression would check at, 400 ms after the
// last change. The times are the scheduler's own (it does the cron arithmetic, in the server's time
// zone, with the environment's start offset); the browser does none. It reads only, so asking again is
// harmless. enabled is false for a preset, whose times need no question.
export function usePreviewUpdateSchedule(
  envId: string,
  schedule: string,
  enabled = true,
): UpdateSchedulePreviewState {
  const typed = schedule.trim();
  const [settled, setSettled] = useState(typed);
  useEffect(() => {
    if (typed === settled) return;
    const id = setTimeout(() => setSettled(typed), UPDATE_SCHEDULE_DEBOUNCE_MS);
    return () => clearTimeout(id);
  }, [typed, settled]);
  const asking = enabled && settled !== "";
  const query = useQuery({
    queryKey: [...updateCheckStatusKey(envId), "schedule-preview", settled] as const,
    queryFn: async (): Promise<UpdateSchedulePreview> => {
      const res = await api.POST("/api/env/{envId}/updates/schedule-preview", {
        params: { path: { envId } },
        body: { schedule: settled },
      });
      if (!res.data) throw failureOf("POST", res);
      return res.data;
    },
    enabled: asking,
    retry: false,
    staleTime: 30_000,
  });
  const current = typed === settled;
  return {
    data: asking && current ? query.data : undefined,
    waiting: enabled && typed !== "" && (!current || query.isFetching),
  };
}

// The key of the instance-wide update-check defaults (Settings, General). It sits outside the "env"
// keys because it belongs to no environment.
export const updateCheckDefaultsKey = ["settings", "update-checks"] as const;

// updateCheckDefaultsQuery is what every environment's update checks follow until the environment
// sets the field itself, the version a save must send back and how many environments follow. Reading
// needs settings.read.
export function updateCheckDefaultsQuery() {
  return queryOptions({
    queryKey: updateCheckDefaultsKey,
    queryFn: async (): Promise<UpdateCheckDefaults> => {
      try {
        const res = await api.GET("/api/settings/update-checks");
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateSettingsError ? e : new UpdateSettingsError("GET", 0);
      }
    },
  });
}

// useUpdateCheckDefaults reads the instance-wide defaults; enabled is false for a person without
// settings.read.
export function useUpdateCheckDefaults(enabled = true) {
  return useQuery({ ...updateCheckDefaultsQuery(), enabled });
}

// useSaveUpdateCheckDefaults saves the defaults (a PUT is a full replace). It is never retried (P2
// D-42). The answer replaces what the page holds, and every environment's status is made stale,
// because the environments that follow a default now check by the new one. A save made against
// another version is 409 settings_changed, an unusable schedule 422 invalid_schedule or
// schedule_too_frequent.
export function useSaveUpdateCheckDefaults() {
  const queryClient = useQueryClient();
  return useMutation<UpdateCheckDefaults, UpdateSettingsError, UpdateCheckDefaultsRequest>({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/settings/update-checks", { body });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateSettingsError ? e : new UpdateSettingsError("PUT", 0);
      }
    },
    onSuccess: (saved) => queryClient.setQueryData(updateCheckDefaultsKey, saved),
    onSettled: () =>
      queryClient.invalidateQueries({
        predicate: (q) => q.queryKey[0] === "env" && q.queryKey[2] === "update-checks",
      }),
  });
}

export type UpdateIgnore = components["schemas"]["UpdateIgnoreBody"];

// A service or a container, as the skip and ignore routes name one.
export interface UpdateWorkload {
  kind: "service" | "container";
  name: string;
}

// What an update is of: a service or a container, or every service of a stack TierPlan manages that has
// an update on offer (kind stack, named by the stack's name).
export type UpdateTarget = UpdateWorkload | { kind: "stack"; name: string };

// What an ignore covers, as the person chose it. text is how the toasts name it: the image
// ("nginx:1.27") for an image, the workload's name for one workload.
export type IgnoreTarget =
  | { scope: "image"; image: string; text: string }
  | { scope: "workload"; kind: UpdateWorkload["kind"]; name: string; text: string };

// UpdateHoldError is a skip, an ignore or the taking back of one that the server refused or never
// answered. status is 0 when the server could not be reached; type is the problem type it named
// (nothing_to_skip, ignored_by_label, skip_not_found, ignore_not_found, invalid_image).
export class UpdateHoldError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`skip or ignore request failed (${status})`);
    this.name = "UpdateHoldError";
    this.status = status;
    this.type = type;
  }
}

function holdFailure(res: { error?: unknown; response: Response }) {
  const problem = readProblem(res.error, res.response);
  return new UpdateHoldError(problem.status, problem.type);
}

// guarded turns whatever a request throws, a network failure included, into an UpdateHoldError.
async function guarded<T>(run: () => Promise<T>): Promise<T> {
  try {
    return await run();
  } catch (e) {
    throw e instanceof UpdateHoldError ? e : new UpdateHoldError(0);
  }
}

function ignoreBody(target: IgnoreTarget) {
  return target.scope === "image"
    ? ({ scope: "image", image: target.image } as const)
    : ({ scope: "workload", kind: target.kind, name: target.name } as const);
}

async function skipRequest(envId: string, workload: UpdateWorkload) {
  return guarded(async () => {
    const res = await api.POST("/api/env/{envId}/updates/skip", {
      params: { path: { envId } },
      body: { kind: workload.kind, name: workload.name },
    });
    if (!res.data) throw holdFailure(res);
    return res.data;
  });
}

async function unskipRequest(envId: string, workload: UpdateWorkload) {
  return guarded(async () => {
    const res = await api.DELETE("/api/env/{envId}/updates/skip", {
      params: { path: { envId } },
      body: { kind: workload.kind, name: workload.name },
    });
    if (res.error !== undefined || !res.response.ok) throw holdFailure(res);
  });
}

async function ignoreRequest(envId: string, target: IgnoreTarget) {
  return guarded(async () => {
    const res = await api.POST("/api/env/{envId}/updates/ignore", {
      params: { path: { envId } },
      body: ignoreBody(target),
    });
    if (!res.data) throw holdFailure(res);
    return res.data;
  });
}

async function unignoreRequest(envId: string, target: IgnoreTarget) {
  return guarded(async () => {
    const res = await api.DELETE("/api/env/{envId}/updates/ignore", {
      params: { path: { envId } },
      body: ignoreBody(target),
    });
    if (res.error !== undefined || !res.response.ok) throw holdFailure(res);
  });
}

export interface SkipVariables extends UpdateWorkload {
  // The version on offer when the person chose, for the toast should the answer not name it. The
  // server skips the one it last found, whatever this says.
  digest?: string;
}

// useSkipUpdate is "Skip this version": it hides the version the workload is offered until the tag
// moves again. It is never retried (P2 D-42). The success toast carries "Undo", an explicit action
// that calls the unskip route once; the toasts are part of the mutation because the menu that
// started it closes and unmounts before the answer. Whatever the answer, the environment's lists
// are read again.
export function useSkipUpdate(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["env", envId] });
  return useMutation<components["schemas"]["UpdateHeldBody"], UpdateHoldError, SkipVariables>({
    retry: false,
    mutationFn: (vars) => skipRequest(envId, vars),
    onSuccess: (held, vars) => {
      const digest = held.update?.ignore?.digest || held.update?.remoteDigest || vars.digest || "";
      toastSuccessWithAction(
        t("updates.skip.done", { digest: digest ? shortDigest(digest) : "", name: vars.name }),
        t("updates.undo"),
        () => {
          void unskipRequest(envId, vars)
            .then(() => toastSuccess(t("updates.unskip.done", { scope: vars.name })))
            .catch(() => toastError(t("updates.unskip.failed", { name: vars.name })))
            .finally(() => void refresh());
        },
      );
    },
    onError: (_err, vars) => {
      toastError(t("updates.skip.failed", { name: vars.name }));
    },
    onSettled: () => void refresh(),
  });
}

// useUnskipUpdate is "Stop skipping": the skipped version shows as an update again. Never retried.
export function useUnskipUpdate(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<void, UpdateHoldError, UpdateWorkload>({
    retry: false,
    mutationFn: (vars) => unskipRequest(envId, vars),
    onSuccess: (_void, vars) => {
      toastSuccess(t("updates.unskip.done", { scope: vars.name }));
    },
    onError: (_err, vars) => {
      toastError(t("updates.unskip.failed", { name: vars.name }));
    },
    onSettled: () => void queryClient.invalidateQueries({ queryKey: ["env", envId] }),
  });
}

// useIgnoreUpdates is the commit of the ignore dialog: updates of one image in this environment, or of
// one workload, stay hidden until taken back. Never retried; "Undo" on the toast is an explicit action
// that calls the unignore route once.
export function useIgnoreUpdates(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["env", envId] });
  return useMutation<components["schemas"]["UpdateHeldBody"], UpdateHoldError, IgnoreTarget>({
    retry: false,
    mutationFn: (target) => ignoreRequest(envId, target),
    onSuccess: (_held, target) => {
      toastSuccessWithAction(
        t("updates.ignore.done", { scope: target.text }),
        t("updates.undo"),
        () => {
          void unignoreRequest(envId, target)
            .then(() => toastSuccess(t("updates.unignore.done", { scope: target.text })))
            .catch(() => toastError(t("updates.unignore.failed", { name: target.text })))
            .finally(() => void refresh());
        },
      );
    },
    onError: (_err, target) => {
      toastError(
        t("updates.ignore.failed", {
          name: target.scope === "workload" ? target.name : target.text,
        }),
      );
    },
    onSettled: () => void refresh(),
  });
}

// useUnignoreUpdates is "Stop ignoring updates". Never retried. An ignore set by the compose label
// cannot be removed here; the server answers 409 ignored_by_label and the menu does not offer it.
export function useUnignoreUpdates(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<void, UpdateHoldError, IgnoreTarget>({
    retry: false,
    mutationFn: (target) => unignoreRequest(envId, target),
    onSuccess: (_void, target) => {
      toastSuccess(t("updates.unignore.done", { scope: target.text }));
    },
    onError: (_err, target) => {
      toastError(t("updates.unignore.failed", { name: target.text }));
    },
    onSettled: () => void queryClient.invalidateQueries({ queryKey: ["env", envId] }),
  });
}

export type UpdatePreview = components["schemas"]["UpdatePreviewBody"];
export type UpdatePreviewItem = components["schemas"]["UpdatePreviewItem"];
export type UpdateWarning = components["schemas"]["UpdateWarningBody"];
export type UpdateHolder = components["schemas"]["UpdateHolderBody"];
export type UpdateJob = components["schemas"]["UpdateJobBody"];
export type ImageUpdateJobSummary = components["schemas"]["ImageUpdateJobSummary"];

// UpdateRequestError is a read or a write of an image update the server refused or never answered.
// status is 0 when the server could not be reached; type is the problem type it named (update_moved,
// operation_locked, nothing_to_update, not_updatable, update_check_failed, update_job_not_found).
export class UpdateRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  // The services an update_moved says changed again, with the digests the server saw.
  readonly moved: MovedService[];
  // What the problem names, by location (a not_updatable code, a lock's workload and job).
  readonly fields: ProblemField[];
  constructor(
    method: string,
    status: number,
    type?: string,
    moved: MovedService[] = [],
    fields: ProblemField[] = [],
  ) {
    super(`${method} image update failed (${status})`);
    this.name = "UpdateRequestError";
    this.status = status;
    this.type = type;
    this.moved = moved;
    this.fields = fields;
  }

  // fieldOf is what the problem named at a location, empty when it named none.
  fieldOf(location: string): string {
    return this.fields.find((f) => f.location === location)?.message ?? "";
  }
}

export interface MovedService {
  service: string;
  fromDigest: string;
  toDigest: string;
}

// movedOf reads the services of a 409 update_moved from the raw problem. Only the three strings of
// each are copied; nothing else of the body travels on.
function movedOf(body: unknown): MovedService[] {
  const errors = (body as { errors?: unknown } | null)?.errors;
  if (!Array.isArray(errors)) return [];
  const out: MovedService[] = [];
  for (const e of errors) {
    const value = (e as { value?: unknown } | null)?.value as Record<string, unknown> | undefined;
    if (value && typeof value.service === "string") {
      out.push({
        service: value.service,
        fromDigest: typeof value.fromDigest === "string" ? value.fromDigest : "",
        toDigest: typeof value.toDigest === "string" ? value.toDigest : "",
      });
    }
  }
  return out;
}

function updateFailure(method: string, res: { error?: unknown; response: Response }) {
  const problem = readProblem(res.error, res.response);
  const moved = problem.type === "update_moved" ? movedOf(res.error) : [];
  return new UpdateRequestError(method, problem.status, problem.type, moved, problem.fields);
}

// imageUpdatePreviewKey is the key of what an update of one service would change. It starts with
// "env" and the environment, so every list's invalidation reaches it.
export function imageUpdatePreviewKey(envId: string, workload: UpdateTarget) {
  return ["env", envId, "updates", "preview", workload.kind, workload.name] as const;
}

// imageUpdatePreviewQuery is what the confirm shows: the digest change, the pre-flight warnings and
// the lock. It asks no registry (the server reads the stored check), so it is cheap to read again, and
// it is never kept: a confirm shows what is true when it opens.
export function imageUpdatePreviewQuery(envId: string, workload: UpdateTarget) {
  return queryOptions({
    queryKey: imageUpdatePreviewKey(envId, workload),
    queryFn: async (): Promise<UpdatePreview> => {
      try {
        const res = await api.GET("/api/env/{envId}/updates/preview", {
          params: {
            path: { envId },
            query: {
              kind: workload.kind,
              name: workload.name,
            },
          },
        });
        if (!res.data) throw updateFailure("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateRequestError ? e : new UpdateRequestError("GET", 0);
      }
    },
    retry: false,
    staleTime: 0,
    gcTime: 0,
  });
}

// useUpdatePreview reads the preview while the confirm is open.
export function useUpdatePreview(envId: string, workload: UpdateTarget, enabled = true) {
  return useQuery({ ...imageUpdatePreviewQuery(envId, workload), enabled });
}

export interface RunUpdateVariables {
  workload: UpdateTarget;
  // For each service, the digest the confirm showed. The server compares it with the registry's.
  expect: Record<string, string>;
  // True only from the "Update anyway" form: a person's choice to try again, for this one workload, a
  // version whose update failed before. Without it the server refuses such a workload with 409
  // previously_failed and changes nothing. It is never sent by anything but that form.
  force?: boolean;
}

// useRunUpdate is the commit of the confirm: the server asks the registry again, takes the lock and
// starts the job. It is never retried (P2 D-42; the click-time re-check is the server's, not a retry),
// and a second press waits for the first (isPending). A refused lock is a toast as well as the alert the
// dialog draws, since the person may have looked away. Whatever the answer, the environment's lists
// are read again: a moved digest refreshes the rows, a started job turns the badges to Updating.
export function useRunUpdate(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  const envName = useEnvName(envId);
  return useMutation<
    components["schemas"]["UpdateRunBody"],
    UpdateRequestError,
    RunUpdateVariables
  >({
    retry: false,
    mutationFn: async ({ workload, expect, force }) => {
      try {
        const res = await api.POST("/api/env/{envId}/updates/run", {
          params: { path: { envId } },
          body: {
            kind: workload.kind,
            name: workload.name,
            expect,
            // Left out unless chosen, so an ordinary update sends exactly what it always did.
            ...(force ? { force: true } : {}),
          },
        });
        if (!res.data) throw updateFailure("POST", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateRequestError ? e : new UpdateRequestError("POST", 0);
      }
    },
    onError: (err) => {
      if (err.type === "operation_locked") {
        toastError(t("updates.confirm.lockedToast", { environment: envName }));
      }
    },
    onSettled: () => void queryClient.invalidateQueries({ queryKey: ["env", envId] }),
  });
}

// useImageUpdateJobs lists the image updates running or ended in the last ten minutes on the
// environments the caller may read, for the Jobs indicator. Polled as the other job lists are; a
// poll that fails keeps the last list.
export const imageUpdateJobsKey = ["update-jobs"] as const;
export const UPDATE_JOBS_POLL_RUNNING_MS = 3_000;
export const UPDATE_JOBS_POLL_IDLE_MS = 30_000;

export const imageUpdateJobsQuery = queryOptions({
  queryKey: imageUpdateJobsKey,
  queryFn: async (): Promise<ImageUpdateJobSummary[]> => {
    const res = await api.GET("/api/update-jobs");
    if (!res.data) throw updateFailure("GET", res);
    return res.data.jobs ?? [];
  },
  refetchInterval: (query) =>
    query.state.data?.some((j) => j.state === "running")
      ? UPDATE_JOBS_POLL_RUNNING_MS
      : UPDATE_JOBS_POLL_IDLE_MS,
  retry: false,
});

export function useImageUpdateJobs() {
  return useQuery(imageUpdateJobsQuery);
}

// An update job as the deploy console's parts read it: the stack job's shape, with what an update
// adds. The server's body has no mode or stack, so they are filled in.
export type UpdateJobView = StackJob &
  Pick<UpdateJob, "workload" | "fromDigest" | "toDigest" | "kind" | "runId">;

function asStackJob(job: UpdateJob): UpdateJobView {
  return { ...job, mode: "update", stackId: "" };
}

// isUpdateJobGone says whether the server answered that the job is not listed: a finished job stays for
// ten minutes, and the address may name a job of another environment.
export function isUpdateJobGone(error: unknown): boolean {
  return (
    error instanceof UpdateRequestError &&
    error.status === 404 &&
    error.type === "update_job_not_found"
  );
}

function imageUpdateJobKey(envId: string, jobId: string) {
  return ["env", envId, "update-jobs", jobId] as const;
}

// How often a view of a running update asks for it while its stream is not live.
const UPDATE_JOB_REFETCH_MS = 1000;

// isBulkJob says whether a job in the list is the one job of a bulk update in an environment. Its summary
// names the bulk update it belongs to; an ordinary update's does not.
export function isBulkJob(job: ImageUpdateJobSummary): boolean {
  return Boolean(job.bulk) || (job.kind as string) === "bulk";
}

export function imageUpdateJobQuery(envId: string, jobId: string) {
  return queryOptions({
    queryKey: imageUpdateJobKey(envId, jobId),
    queryFn: async (): Promise<UpdateJob> => {
      try {
        const res = await api.GET("/api/env/{envId}/update-jobs/{jobId}", {
          params: { path: { envId, jobId } },
        });
        if (!res.data) throw updateFailure("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof UpdateRequestError ? e : new UpdateRequestError("GET", 0);
      }
    },
    retry: (count, error) => !isUpdateJobGone(error) && count < 3,
    refetchOnWindowFocus: false,
  });
}

function updateJobStreamUrl(envId: string, jobId: string, lastEventId?: string): string {
  const q = lastEventId ? `?${new URLSearchParams({ lastEventId })}` : "";
  return apiUrl(
    `/api/env/${encodeURIComponent(envId)}/update-jobs/${encodeURIComponent(jobId)}/stream${q}`,
  );
}

async function probeUpdateJob(envId: string, jobId: string): Promise<number> {
  try {
    const res = await api.GET("/api/env/{envId}/update-jobs/{jobId}", {
      params: { path: { envId, jobId } },
    });
    return res.response.status;
  } catch {
    return 0;
  }
}

export interface UpdateJobFollow extends Omit<StackJobFollow, "job"> {
  job?: UpdateJobView;
}

// useImageUpdateJob follows an image update as useStackJob follows a deploy: the job at once from one
// read, then live from its stream, with the read going on every second while the stream is not live.
// Opening a job that is already running re-attaches to it, and one that has ended shows as it ended.
export function useImageUpdateJob(envId: string, jobId: string): UpdateJobFollow {
  const [state, dispatch] = useReducer(reduceStackStream, initialStackStream);
  const base = imageUpdateJobQuery(envId, jobId);
  const quiet = state.status === "live" || state.finished;
  const query = useQuery({
    ...base,
    refetchInterval: (q) => {
      if (isUpdateJobGone(q.state.error) || quiet) return false;
      const job = q.state.data;
      return job !== undefined && job.state !== "running" ? false : UPDATE_JOB_REFETCH_MS;
    },
  });

  useEffect(() => {
    dispatch({ type: "reset" });
    const stream = openStackJobStream({
      envId,
      stackId: "",
      jobId,
      streamUrl: (last) => updateJobStreamUrl(envId, jobId, last),
      probe: () => probeUpdateJob(envId, jobId),
      onFrame: (frame: StackFrame) =>
        dispatch(
          frame.type === "snapshot" || frame.type === "done" || frame.type === "polled"
            ? { ...frame, job: asStackJob(frame.job as unknown as UpdateJob) }
            : frame,
        ),
    });
    return () => stream.close();
  }, [envId, jobId]);

  const polled = query.data;
  useEffect(() => {
    if (polled) dispatch({ type: "polled", job: asStackJob(polled) });
  }, [polled]);

  const gone = state.gone || isUpdateJobGone(query.error);
  const job = (state.job ?? (polled ? asStackJob(polled) : undefined)) as UpdateJobView | undefined;
  const refetch = query.refetch;
  const reload = useCallback(() => void refetch(), [refetch]);
  return { ...state, job, gone, loadError: query.isError && !gone && job === undefined, reload };
}
