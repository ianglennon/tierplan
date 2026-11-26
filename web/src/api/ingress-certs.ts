// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { IngressLoadError, ingressKey, ingressRefetchInterval } from "@/api/ingress";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type IngressCert = components["schemas"]["IngressCertBody"];
export type IngressCerts = components["schemas"]["IngressCertsBody"];
export type IngressCertBudget = components["schemas"]["IngressCertBudgetBody"];
export type IngressCertBudgets = components["schemas"]["IngressCertBudgetsBody"];

function visible(): boolean {
  return document.visibilityState === "visible";
}

// certsSettling says whether any certificate is still on its way: one that is queued or being ordered is one a
// person is watching, so the tab reads at the quick cadence until it lands (D-24/A-09).
export function certsSettling(data: IngressCerts | undefined): boolean {
  return (data?.items ?? []).some((c) => c.state === "queued" || c.state === "ordering");
}

// Every certificate of the environment, live and retired, in the server's hostname order. Read every 10 s while one is
// queued or ordering and every 30 s otherwise while the page is visible. A failed read is not retried: the tab has its
// own Retry and a 403 must answer at once.
export function ingressCertificatesQuery(envId: string) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "certificates"] as const,
    queryFn: async (): Promise<IngressCerts> => {
      const res = await api.GET("/api/env/{envId}/ingress/certificates", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    refetchInterval: (query) => ingressRefetchInterval(certsSettling(query.state.data), visible()),
  });
}

// The weekly budget of new certificates, one row per registered domain and issuer in use (D-12).
export function ingressCertificateBudgetQuery(envId: string) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "certificates", "budget"] as const,
    queryFn: async (): Promise<IngressCertBudgets> => {
      const res = await api.GET("/api/env/{envId}/ingress/certificates/budget", {
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
