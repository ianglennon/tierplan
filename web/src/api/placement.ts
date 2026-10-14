// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { isNotASwarm, ServicesLoadError, servicesKey } from "@/api/services";

export type Placement = components["schemas"]["PlacementBody"];

// The placement is read again this often while the page is visible. Node events join the
// service events in a later plan; until then the poll is what notices a node going down.
export const PLACEMENT_REFETCH_MS = 5_000;

// How many times a failed read is tried again. A standalone host is never tried again.
// Exported so tests can set it to none.
export const placementRetry = { attempts: 3 };

// The query key of where the tasks run, for the whole swarm or for one service. It extends the
// services list's key, so a service event, which refreshes that list, refreshes this too.
export function placementKey(envId: string, serviceId?: string) {
  return [...servicesKey(envId), "placement", serviceId ?? "all"] as const;
}

// The nodes, the tasks that should be running and the services they belong to, polled every
// 5 s while the page is visible and not at all while it is hidden. The error is the same
// ServicesLoadError the services list throws, so a standalone host (409 not_a_swarm) is told
// from every other failure.
export function placementQuery(envId: string, serviceId?: string) {
  return queryOptions({
    queryKey: placementKey(envId, serviceId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/placement", {
        params: { path: { envId }, query: serviceId ? { serviceId } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServicesLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // A standalone host will not turn into a swarm on a second try.
    retry: (count, error) => !isNotASwarm(error) && count < placementRetry.attempts,
    refetchInterval: () => (document.visibilityState === "visible" ? PLACEMENT_REFETCH_MS : false),
  });
}
