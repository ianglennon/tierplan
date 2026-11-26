// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import {
  IngressActionError,
  type IngressDomain,
  ingressDomainsQuery,
  ingressRoutesQuery,
} from "@/api/ingress";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type CreateDomainInput = components["schemas"]["CreateIngressDomainInputBody"];

// refused is a write the server answered with a problem: its status and its own sentence, as sent.
function refused(error: unknown, response: Response): IngressActionError {
  const problem = readProblem(error, response);
  return new IngressActionError(problem.status, problem.detail);
}

// The domains list carries each domain's reserved names and certificate count, and the Routes tab's held and claim
// rows follow the reserved names, so a write to a domain refreshes both at once.
function useRefreshDomains(envId: string) {
  const queryClient = useQueryClient();
  return async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ingressDomainsQuery(envId).queryKey }),
      queryClient.invalidateQueries({ queryKey: ingressRoutesQuery(envId).queryKey }),
    ]);
  };
}

// useReserveHostname holds a hostname back under a domain so no stack can claim it (D-01). It needs ingress.manage,
// which the server checks; a name outside the domain or one a stack already holds is refused with the server's detail.
export function useReserveHostname(envId: string) {
  const refresh = useRefreshDomains(envId);
  return useMutation({
    mutationFn: async ({
      domainId,
      hostname,
    }: {
      domainId: string;
      hostname: string;
    }): Promise<void> => {
      const { error, response } = await api.POST(
        "/api/env/{envId}/ingress/domains/{domainId}/reserved",
        { params: { path: { envId, domainId } }, body: { hostname } },
      );
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}

// useReleaseHostname lifts the hold on a reserved hostname. It only lifts a hold, so it asks for no confirmation.
export function useReleaseHostname(envId: string) {
  const refresh = useRefreshDomains(envId);
  return useMutation({
    mutationFn: async ({
      domainId,
      hostname,
    }: {
      domainId: string;
      hostname: string;
    }): Promise<void> => {
      const { error, response } = await api.DELETE(
        "/api/env/{envId}/ingress/domains/{domainId}/reserved/{hostname}",
        { params: { path: { envId, domainId, hostname } } },
      );
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}

// useCreateDomain registers a domain. The server tests the DNS token first and saves nothing when the test fails; the
// token is in the request body and nowhere else, and the mutation is dropped from the cache the moment it settles
// (gcTime 0), so the variables that hold it do not outlive the dialog (T-27-64).
export function useCreateDomain(envId: string) {
  const refresh = useRefreshDomains(envId);
  return useMutation({
    gcTime: 0,
    mutationFn: async (input: CreateDomainInput): Promise<IngressDomain> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/domains", {
        params: { path: { envId } },
        body: input,
      });
      if (!data) throw refused(error, response);
      return data;
    },
    onSuccess: refresh,
  });
}
