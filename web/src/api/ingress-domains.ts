// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import {
  IngressActionError,
  type IngressDomain,
  ingressDomainsQuery,
  ingressKey,
  ingressRoutesQuery,
} from "@/api/ingress";
import { ingressCertificatesQuery } from "@/api/ingress-certs";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type CreateDomainInput = components["schemas"]["CreateIngressDomainInputBody"];
export type IssuerPreview = components["schemas"]["IngressIssuerPreviewBody"];
export type DomainIssuer = IssuerPreview["issuer"];

// refused is a write the server answered with a problem: its status and its own sentence, as sent.
function refused(error: unknown, response: Response): IngressActionError {
  const problem = readProblem(error, response);
  return new IngressActionError(problem.status, problem.detail, problem.type);
}

// The domains list carries each domain's reserved names and certificate count, the Routes tab's held and claim rows
// follow the reserved names, and the certificates under a domain follow its issuer, token and existence, so a write to a
// domain refreshes all three at once.
function useRefreshDomains(envId: string) {
  const queryClient = useQueryClient();
  return async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ingressDomainsQuery(envId).queryKey }),
      queryClient.invalidateQueries({ queryKey: ingressRoutesQuery(envId).queryKey }),
      queryClient.invalidateQueries({ queryKey: ingressCertificatesQuery(envId).queryKey }),
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

// useIssuerPreview reads what changing a domain's issuer would do (D-07): how many certificates are ordered again and
// which uploads are left alone. It reads nothing until an issuer is chosen, and is never kept: a count is only true at
// the moment it is read, and the write checks it again.
export function useIssuerPreview(envId: string, domainId: string, issuer: DomainIssuer | null) {
  return useQuery({
    queryKey: [...ingressKey(envId), "domain-issuer-preview", domainId, issuer] as const,
    enabled: issuer !== null,
    gcTime: 0,
    staleTime: 0,
    retry: false,
    queryFn: async (): Promise<IssuerPreview> => {
      if (issuer === null) throw new IngressActionError(0);
      const { data, error, response } = await api.GET(
        "/api/env/{envId}/ingress/domains/{domainId}/issuer",
        { params: { path: { envId, domainId }, query: { issuer } } },
      );
      if (!data) throw refused(error, response);
      return data;
    },
  });
}

// useChangeIssuer changes a domain's issuer, sending back the count the preview showed. The server counts again inside
// the write and answers 409 when a certificate came or went since, with its own sentence for the dialog.
export function useChangeIssuer(envId: string) {
  const refresh = useRefreshDomains(envId);
  return useMutation({
    mutationFn: async ({
      domainId,
      issuer,
      confirmCount,
    }: {
      domainId: string;
      issuer: DomainIssuer;
      confirmCount: number;
    }): Promise<void> => {
      const { error, response } = await api.PUT(
        "/api/env/{envId}/ingress/domains/{domainId}/issuer",
        { params: { path: { envId, domainId } }, body: { issuer, confirmCount } },
      );
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}

// useReplaceDomainToken replaces a domain's DNS token. The server tests the new one first and keeps the old one when
// the test fails. Like registration, the mutation is dropped from the cache the moment it settles (gcTime 0), so the
// variables that hold the token do not outlive the dialog (T-27-66).
export function useReplaceDomainToken(envId: string) {
  const refresh = useRefreshDomains(envId);
  return useMutation({
    gcTime: 0,
    mutationFn: async ({ domainId, token }: { domainId: string; token: string }): Promise<void> => {
      const { error, response } = await api.PUT(
        "/api/env/{envId}/ingress/domains/{domainId}/token",
        { params: { path: { envId, domainId } }, body: { token } },
      );
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}

// useDeleteDomain deletes a domain and the names reserved under it. The server refuses while a stack holds a name under
// it (409 ingress_domain_in_use, the holders in the detail) and while DNS records of its own remain; the refusal is the
// mutation's error, carrying the problem type so the dialog can tell the two apart.
export function useDeleteDomain(envId: string) {
  const refresh = useRefreshDomains(envId);
  return useMutation({
    mutationFn: async ({ domainId }: { domainId: string }): Promise<void> => {
      const { error, response } = await api.DELETE("/api/env/{envId}/ingress/domains/{domainId}", {
        params: { path: { envId, domainId } },
      });
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}
