// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import { StacksLoadError, stackKey } from "@/api/stacks";

export type StackRoute = components["schemas"]["StackRouteBody"];
export type StackRoutes = components["schemas"]["StackRoutesBody"];
export type StackRouteState = StackRoute["state"];

// How often the routes are read while a rollout or a certificate is still on its way (UI-SPEC S2).
export const ROUTES_SETTLING_REFETCH_MS = 10_000;

// The query key of a stack's route states. It extends the stack's key, so whatever refreshes the stack's page
// refreshes its routes too.
export function routesKey(envId: string, stackId: string) {
  return [...stackKey(envId, stackId), "routes"] as const;
}

// settling says whether any route is still moving towards serving: pending (not every node has it) or waiting
// for a certificate. Those are the states a person is watching, so they are read faster.
export function settling(data: StackRoutes | undefined): boolean {
  return (data?.routes ?? []).some(
    (r) => r.state === "pending" || r.state === "waiting_certificate",
  );
}

// The routes a stack holds and the state each is in, derived by the server when read (D-24). Read every 10 s while
// any route is pending or waiting for a certificate, and otherwise with the page's 30-second refresh while the page
// is visible. A failed read is not retried.
export function stackRoutesQuery(envId: string, stackId: string) {
  return queryOptions({
    queryKey: routesKey(envId, stackId),
    queryFn: async (): Promise<StackRoutes> => {
      const res = await api.GET("/api/env/{envId}/stacks/{stackId}/routes", {
        params: { path: { envId, stackId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new StacksLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // Not retried: the card has its own Retry, the page reads again on its timer, and a 403 or a gone stack must answer at once.
    retry: false,
    refetchInterval: (query) => {
      if (document.visibilityState !== "visible") return false;
      return settling(query.state.data) ? ROUTES_SETTLING_REFETCH_MS : LIST_REFETCH_MS;
    },
  });
}
