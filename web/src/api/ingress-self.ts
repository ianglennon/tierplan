// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { IngressActionError } from "@/api/ingress";
import type { IngressStarted, ReviewItem, ReviewPreview } from "@/api/ingress-switches";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type IngressSelfRoute = components["schemas"]["IngressSelfRouteBody"];
type SelfRoutePreviewBody = components["schemas"]["IngressSelfRoutePreviewBody"];

// SelfRoutePreview is the self route's preview in the one shape the review dialog draws: no findings, no stacks, no
// nodes, only the ordered items (D-17). It also keeps the hostname as the server judged it and the direct address that
// keeps reaching TierPlan whatever the route does (empty for the page's own origin).
export interface SelfRoutePreview extends ReviewPreview {
  host: string;
  directAddress: string;
}

// refused is a write the server answered with a problem: its status, its own sentence and its type, as sent.
function refused(error: unknown, response: Response): IngressActionError {
  const problem = readProblem(error, response);
  return new IngressActionError(problem.status, problem.detail, problem.type);
}

function previewOf(data: SelfRoutePreviewBody): SelfRoutePreview {
  const items: ReviewItem[] = (data.items ?? []).map((item) => ({
    code: item.code,
    state: item.state,
    message: item.message,
    ...(item.snippet ? { snippet: { kind: item.snippet.kind, text: item.snippet.text } } : {}),
  }));
  return {
    findings: [],
    redeploy: [],
    unreachable: [],
    items,
    host: data.host,
    directAddress: data.directAddress,
  };
}

// useSelfRoutePreview reads what serving TierPlan itself on the hostname would do (D-17). It writes nothing and answers
// 200 whether or not an item refuses; a refused item is drawn in the list and keeps the confirm off. Like every preview
// it is a mutation dropped the moment it is reset (gcTime 0): it is only true when it is read, and the job checks again.
export function useSelfRoutePreview(envId: string, hostname: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<SelfRoutePreview> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/self/preview", {
        params: { path: { envId } },
        body: { hostname: hostname.trim() },
      });
      if (!data) throw refused(error, response);
      return previewOf(data);
    },
  });
}

// useEnableSelfRoute switches TierPlan's own route on. The server answers 202 with the job's ID; the job runs the checks
// again first and ends at self_check with the refusing item's sentence when one refuses.
export function useEnableSelfRoute(envId: string, hostname: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<IngressStarted> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/self", {
        params: { path: { envId } },
        body: { hostname: hostname.trim() },
      });
      if (!data) throw refused(error, response);
      return { jobId: data.jobId };
    },
  });
}

// useSelfRouteOffPreview reads what switching TierPlan's own route off would do: the origin that goes and whether the
// tierplan-self network goes with it. It never refuses because of the route's state (D-11); 409 self_route_not_on is the
// one refusal, drawn as the server's sentence.
export function useSelfRouteOffPreview(envId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<SelfRoutePreview> => {
      const { data, error, response } = await api.POST(
        "/api/env/{envId}/ingress/self/off/preview",
        { params: { path: { envId } } },
      );
      if (!data) throw refused(error, response);
      return previewOf(data);
    },
  });
}

// useDisableSelfRoute switches TierPlan's own route off, as a job. Nothing about the route, the engine or the
// certificate stops it: TierPlan stays reachable on its direct address.
export function useDisableSelfRoute(envId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<IngressStarted> => {
      const { data, error, response } = await api.DELETE("/api/env/{envId}/ingress/self", {
        params: { path: { envId } },
      });
      if (!data) throw refused(error, response);
      return { jobId: data.jobId };
    },
  });
}
