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
export function useFleetUpdates(env: readonly string[], show: FleetShowValue) {
  return useQuery(fleetUpdatesQuery(env, show));
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
