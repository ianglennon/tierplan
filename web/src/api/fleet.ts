// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  queryOptions,
  useIsMutating,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { imageUpdateJobsKey } from "@/api/updates";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";

export type FleetSummary = components["schemas"]["FleetSummaryBody"];
export type FleetFailing = components["schemas"]["FleetFailingBody"];
export type FleetFailingItem = components["schemas"]["FleetFailingItem"];
export type FleetNotIncluded = components["schemas"]["FleetNotIncludedBody"];
export type FleetUpdates = components["schemas"]["FleetUpdatesBody"];
export type FleetUpdateGroup = components["schemas"]["FleetUpdateGroup"];
export type FleetUpdateItem = components["schemas"]["FleetUpdateItem"];
export type FleetUpdateCounts = components["schemas"]["FleetUpdateCounts"];
export type FleetUpdateCoverage = components["schemas"]["FleetUpdateCoverage"];
export type FleetPools = components["schemas"]["FleetPoolsBody"];
export type FleetPoolItem = components["schemas"]["FleetPoolItem"];
export type FleetCheckResult = components["schemas"]["FleetCheckBody"];
export type FleetCheckSkipped = components["schemas"]["FleetCheckSkipped"];
export type FleetBulkRunResult = components["schemas"]["FleetBulkRunBody"];
export type FleetBulkSkipResult = components["schemas"]["FleetBulkSkipBody"];
export type FleetBulkRefused = components["schemas"]["FleetBulkRefused"];

// Which items the Updates tab lists (the Show chips). "updates" is the default and is left out of
// the address.
export type FleetShowValue = "updates" | "problems" | "ignored" | "all";

// Every fleet read hangs under this key, so the health stream can read them all again at once.
export const FLEET_KEY = ["fleet"] as const;

// The fleet tabs and tiles are read again every minute while the page is visible, and on any
// health event (UI-SPEC "Polling cadence"). A hidden tab stops: react-query does not poll one.
export const FLEET_REFETCH_MS = 60_000;

export const fleetSummaryQuery = queryOptions({
  queryKey: [...FLEET_KEY, "summary"],
  queryFn: async (): Promise<FleetSummary> => {
    const { data } = await api.GET("/api/fleet/summary");
    if (!data) throw new Error("GET /api/fleet/summary failed");
    return data;
  },
  refetchInterval: FLEET_REFETCH_MS,
});

// fleetFailingQuery is every failing service and container of the environments the caller may
// read, narrowed to env when it is not empty. The server ignores an ID the caller may not read.
export function fleetFailingQuery(env: readonly string[]) {
  return queryOptions({
    queryKey: [...FLEET_KEY, "failing", [...env].sort()],
    queryFn: async (): Promise<FleetFailing> => {
      const { data } = await api.GET("/api/fleet/failing", {
        params: { query: env.length > 0 ? { env: [...env] } : {} },
      });
      if (!data) throw new Error("GET /api/fleet/failing failed");
      return data;
    },
    refetchInterval: FLEET_REFETCH_MS,
  });
}

// useFleetSummary is the four Overview tiles.
export function useFleetSummary() {
  return useQuery(fleetSummaryQuery);
}

// useFleetFailing is the Failing services tab.
export function useFleetFailing(env: readonly string[]) {
  return useQuery(fleetFailingQuery(env));
}

// fleetUpdatesQuery is every pending update of the environments the caller may read, grouped by
// environment, for one Show value. The counts of the other Show values come with it, so the chips
// are right whichever is showing.
export function fleetUpdatesQuery(env: readonly string[], show: FleetShowValue) {
  return queryOptions({
    queryKey: [...FLEET_KEY, "updates", show, [...env].sort()],
    queryFn: async (): Promise<FleetUpdates> => {
      const { data } = await api.GET("/api/fleet/updates", {
        params: { query: { show, ...(env.length > 0 ? { env: [...env] } : {}) } },
      });
      if (!data) throw new Error("GET /api/fleet/updates failed");
      return data;
    },
    refetchInterval: FLEET_REFETCH_MS,
  });
}

// fleetPoolsQuery is every address pool and overlay network at 70% or more used, fullest first.
export function fleetPoolsQuery(env: readonly string[]) {
  return queryOptions({
    queryKey: [...FLEET_KEY, "pools", [...env].sort()],
    queryFn: async (): Promise<FleetPools> => {
      const { data } = await api.GET("/api/fleet/pools", {
        params: { query: env.length > 0 ? { env: [...env] } : {} },
      });
      if (!data) throw new Error("GET /api/fleet/pools failed");
      return data;
    },
    refetchInterval: FLEET_REFETCH_MS,
  });
}

// useFleetUpdates is the Updates tab.
// While a bulk update is under way the tab reads again every few seconds, so its rows go from Queued
// to Updating… to their end without waiting for the minute.
export const FLEET_BUSY_REFETCH_MS = 3_000;

export function useFleetUpdates(env: readonly string[], show: FleetShowValue, busy = false) {
  return useQuery({
    ...fleetUpdatesQuery(env, show),
    refetchInterval: busy ? FLEET_BUSY_REFETCH_MS : FLEET_REFETCH_MS,
  });
}

// useFleetPools is the Address pools tab.
export function useFleetPools(env: readonly string[]) {
  return useQuery(fleetPoolsQuery(env));
}

// FleetCheckError is a fleet Check now the server refused or never answered. status is 0 when it
// could not be reached; type is the problem type it named (too_many_requests on a 429).
export class FleetCheckError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST fleet updates check failed (${status})`);
    this.name = "FleetCheckError";
    this.status = status;
    this.type = type;
  }
}

const FLEET_CHECK_KEY = [...FLEET_KEY, "updates", "check"] as const;

// useFleetChecking says whether a fleet Check now is out, in every place that shows the button.
export function useFleetChecking(): boolean {
  return useIsMutating({ mutationKey: FLEET_CHECK_KEY }) > 0;
}

// useFleetCheck is Check now for the whole fleet or for the environments chosen: it asks the
// registries again for every image they run. The environments the caller may not check are skipped
// by the server and named in the toast. A second press within the server's limit is a 429, worded
// as a wait and not as a failure. It is never retried: a person pressing the button again is the
// retry. The toasts are part of the mutation, since the button may be gone before the answer.
// Whatever the answer, the fleet reads are made again.
export function useFleetCheck() {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<FleetCheckResult, FleetCheckError, readonly string[]>({
    retry: false,
    mutationKey: FLEET_CHECK_KEY,
    mutationFn: async (envIds) => {
      try {
        const res = await api.POST("/api/fleet/updates/check", {
          body: envIds.length > 0 ? { envIds: [...envIds] } : undefined,
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new FleetCheckError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof FleetCheckError ? e : new FleetCheckError(0);
      }
    },
    onSuccess: (result) => {
      const done = `${t("updates.check.checked", { count: result.checked })} ${t("updates.check.found", { count: result.updates })}`;
      const skipped = result.skipped ?? [];
      if (skipped.length === 0) {
        toastSuccess(done);
        return;
      }
      const names = skipped
        .map((s) =>
          t("fleet.updates.check.skippedName", {
            name: s.envName,
            reason: t(`fleet.updates.check.skippedReason.${s.reason}`),
          }),
        )
        .join(", ");
      toastInfo(`${done} ${t("fleet.updates.check.skipped", { count: skipped.length, names })}`);
    },
    onError: (error) => {
      if (error.status === 429 || error.type === "too_many_requests") {
        toastInfo(t("fleet.updates.check.wait"));
        return;
      }
      toastError(t("updates.check.failed"));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: FLEET_KEY });
    },
  });
}

// The most workloads one bulk request takes (the server answers 422 past it).
export const FLEET_BULK_MAX = 500;

// A workload ticked on the Updates tab, with the environment it is in.
export interface FleetSelected {
  envId: string;
  envName: string;
  type: FleetUpdateGroup["type"];
  item: FleetUpdateItem;
}

// FleetBulkError is a bulk request the server refused or never answered. status is 0 when it could not
// be reached; type is the problem type it named.
export class FleetBulkError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST fleet bulk update failed (${status})`);
    this.name = "FleetBulkError";
    this.status = status;
    this.type = type;
  }
}

// The bulk jobs this browser started, by job ID: the Jobs indicator tells the person, once, how each
// one ended. A job somebody else started, or one from before the page was opened, is not news here.
export interface FollowedBulkJob {
  envId: string;
  envName: string;
}

const followedBulk = new Map<string, FollowedBulkJob>();

export function followBulkJob(jobId: string, job: FollowedBulkJob): void {
  followedBulk.set(jobId, job);
}

// takeFollowedBulkJob answers the job once and forgets it, so one job is told once.
export function takeFollowedBulkJob(jobId: string): FollowedBulkJob | undefined {
  const job = followedBulk.get(jobId);
  followedBulk.delete(jobId);
  return job;
}

// How many names a toast spells out before it says "and {n} more".
const NAMES_SHOWN = 3;

type Translate = (key: string, options?: Record<string, unknown>) => string;

// refusedNames words the workloads a bulk answer did not take, with why, for a toast. The names are
// text.
function refusedNames(t: Translate, refused: readonly FleetBulkRefused[]): string {
  const shown = refused.slice(0, NAMES_SHOWN).map((r) =>
    t("fleet.bulk.refusedName", {
      name: r.name,
      reason: t(`fleet.bulk.reason.${r.reason}`),
    }),
  );
  const rest = refused.length - shown.length;
  if (rest > 0) shown.push(t("fleet.bulk.more", { count: rest }));
  return shown.join(", ");
}

async function bulkFailure(res: { error?: unknown; response: Response }): Promise<FleetBulkError> {
  const problem = readProblem(res.error, res.response);
  return new FleetBulkError(problem.status, problem.type);
}

// useRunFleetUpdates is "Update {n} selected": the server judges every item for the caller, takes the
// environment's lock, and starts one job for each environment that has something to update. What it
// names as not taken is what it knows at once; an item whose newer version moved, whose check failed
// or that failed before is found at its turn, inside the job, so the toast says each one is checked
// again and some may be skipped. Never retried (the click-time check is the server's). The toasts and
// the follow-up are part of the mutation, since the dialog is gone before the answer.
export function useRunFleetUpdates() {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<FleetBulkRunResult, FleetBulkError, readonly FleetSelected[]>({
    retry: false,
    mutationFn: async (selected) => {
      try {
        const res = await api.POST("/api/fleet/updates/run", {
          body: {
            items: selected.map((s) => ({
              envId: s.envId,
              kind: s.item.kind,
              name: s.item.name,
              expect: s.item.toDigest ? { [s.item.name]: s.item.toDigest } : {},
            })),
          },
        });
        if (!res.data) throw await bulkFailure(res);
        return res.data;
      } catch (e) {
        throw e instanceof FleetBulkError ? e : new FleetBulkError(0);
      }
    },
    onSuccess: (result, selected) => {
      const jobs = result.jobs ?? [];
      const refused = result.skipped ?? [];
      const names = new Map<string, string>();
      for (const s of selected) names.set(s.envId, s.envName);
      for (const job of jobs) {
        followBulkJob(job.jobId, { envId: job.envId, envName: names.get(job.envId) ?? "" });
      }
      const taken = Math.max(selected.length - refused.length, 0);
      const notTaken =
        refused.length > 0
          ? t("fleet.bulk.notStarted", {
              count: refused.length,
              names: refusedNames(t, refused),
            })
          : "";
      if (jobs.length === 0) {
        toastError(`${t("fleet.bulk.noneStarted")} ${notTaken}`.trim());
        return;
      }
      toastInfo(`${t("fleet.bulk.started", { count: taken })} ${notTaken}`.trim());
    },
    onError: (error) => {
      toastError(error.status === 403 ? t("fleet.bulk.forbidden") : t("fleet.bulk.failed"));
    },
    onSettled: (_result, _error, selected) => {
      void queryClient.invalidateQueries({ queryKey: FLEET_KEY });
      void queryClient.invalidateQueries({ queryKey: imageUpdateJobsKey });
      for (const envId of new Set(selected.map((s) => s.envId))) {
        void queryClient.invalidateQueries({ queryKey: ["env", envId] });
      }
    },
  });
}

// useSkipFleetUpdates is "Skip selected versions": each ticked workload's offered version is hidden
// until the tag moves again, on every environment the caller may manage updates on. The server names
// what it refused. Never retried.
export function useSkipFleetUpdates() {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<FleetBulkSkipResult, FleetBulkError, readonly FleetSelected[]>({
    retry: false,
    mutationFn: async (selected) => {
      try {
        const res = await api.POST("/api/fleet/updates/skip", {
          body: {
            items: selected.map((s) => ({ envId: s.envId, kind: s.item.kind, name: s.item.name })),
          },
        });
        if (!res.data) throw await bulkFailure(res);
        return res.data;
      } catch (e) {
        throw e instanceof FleetBulkError ? e : new FleetBulkError(0);
      }
    },
    onSuccess: (result) => {
      const refused = result.refused ?? [];
      const notSkipped =
        refused.length > 0
          ? t("fleet.bulk.notSkipped", { count: refused.length, names: refusedNames(t, refused) })
          : "";
      if (result.skipped === 0) {
        toastError(`${t("fleet.bulk.noneSkipped")} ${notSkipped}`.trim());
        return;
      }
      const done = t("fleet.bulk.skipped", { count: result.skipped });
      if (refused.length > 0) toastInfo(`${done} ${notSkipped}`);
      else toastSuccess(done);
    },
    onError: () => {
      toastError(t("fleet.bulk.skipFailed"));
    },
    onSettled: (_result, _error, selected) => {
      void queryClient.invalidateQueries({ queryKey: FLEET_KEY });
      for (const envId of new Set(selected.map((s) => s.envId))) {
        void queryClient.invalidateQueries({ queryKey: ["env", envId] });
      }
    },
  });
}
