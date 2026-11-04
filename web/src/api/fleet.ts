// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

export type FleetSummary = components["schemas"]["FleetSummaryBody"];
export type FleetFailing = components["schemas"]["FleetFailingBody"];
export type FleetFailingItem = components["schemas"]["FleetFailingItem"];
export type FleetNotIncluded = components["schemas"]["FleetNotIncludedBody"];

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
