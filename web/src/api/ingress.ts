// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";

export type IngressRoute = components["schemas"]["IngressRouteBody"];
export type IngressRoutes = components["schemas"]["IngressRoutesBody"];
export type RouteCondition = components["schemas"]["RouteConditionBody"];
export type IngressStatus = components["schemas"]["IngressStatusBody"];
export type IngressDomain = components["schemas"]["IngressDomainBody"];
export type IngressDomains = components["schemas"]["IngressDomainsBody"];

// How often the page reads while something on it is still settling (UI-SPEC S1, D-24/A-09).
export const INGRESS_SETTLING_REFETCH_MS = 10_000;

// The query key every read of an environment's Ingress page extends, so one invalidation refreshes them all.
export function ingressKey(envId: string) {
  return ["env", envId, "ingress"] as const;
}

// IngressLoadError is a failed read of the Ingress page, with the status and the problem type the server sent.
export class IngressLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET ingress failed (${status})`);
    this.name = "IngressLoadError";
    this.status = status;
    this.type = type;
  }
}

// routesSettling says whether any route is still moving towards serving: a route that is not serving, not
// stopped on purpose and not just switched off is one a person is watching.
export function routesSettling(data: IngressRoutes | undefined): boolean {
  return (data?.routes ?? []).some(
    (r) =>
      r.kind === "route" &&
      r.state !== "" &&
      r.state !== "serving" &&
      r.state !== "ingress_off" &&
      !r.stopped,
  );
}

// ingressRefetchInterval is the one cadence of the page: nothing while it is hidden, 10 s while something settles,
// and the list pages' 30 s otherwise.
export function ingressRefetchInterval(settling: boolean, visible: boolean): number | false {
  if (!visible) return false;
  return settling ? INGRESS_SETTLING_REFETCH_MS : LIST_REFETCH_MS;
}

function visible(): boolean {
  return document.visibilityState === "visible";
}

// The environment's ingress switches, the proxy and the virtual IP, as the server reads them.
export function ingressStatusQuery(envId: string) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "status"] as const,
    queryFn: async (): Promise<IngressStatus> => {
      const res = await api.GET("/api/env/{envId}/ingress/status", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    refetchInterval: () => ingressRefetchInterval(false, visible()),
  });
}

// Every route, held name and reserved name of the environment, each route with its five conditions. Read every 10 s
// while a route is not serving, is not stopped and is not switched off, and every 30 s otherwise while the page is
// visible. A failed read is not retried: the tab has its own Retry and a 403 must answer at once.
export function ingressRoutesQuery(envId: string) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "routes"] as const,
    queryFn: async (): Promise<IngressRoutes> => {
      const res = await api.GET("/api/env/{envId}/ingress/routes", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    refetchInterval: (query) => ingressRefetchInterval(routesSettling(query.state.data), visible()),
  });
}

// The domains registered for the environment, read by the Certificates and Domains tabs.
export function ingressDomainsQuery(envId: string) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "domains"] as const,
    queryFn: async (): Promise<IngressDomains> => {
      const res = await api.GET("/api/env/{envId}/ingress/domains", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    refetchInterval: () => ingressRefetchInterval(false, visible()),
  });
}

// IngressActionError is a refused or failed write to the Ingress page, with the status and the server's own words
// for what is wrong, as sent.
export class IngressActionError extends Error {
  readonly status: number;
  readonly detail?: string;
  constructor(status: number, detail?: string) {
    super(`ingress request failed (${status})`);
    this.name = "IngressActionError";
    this.status = status;
    this.detail = detail;
  }
}

// useResumeIngress resumes the ingress of an environment a restore paused (D-07: it has no preview). It needs
// ingress.manage, which the server checks; the page only decides what to show. A resume that worked, or one that
// found nothing paused, refreshes everything on the page.
export function useResumeIngress(envId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/resume", {
        params: { path: { envId } },
      });
      if (!data) {
        const problem = readProblem(error, response);
        throw new IngressActionError(problem.status, problem.detail);
      }
      return data;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ingressKey(envId) }),
  });
}
