// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import {
  IngressActionError,
  IngressLoadError,
  ingressKey,
  ingressRefetchInterval,
} from "@/api/ingress";
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

// The certificates and the weekly budget are read together, and every write to one changes the other, so a
// successful write refreshes both at once (D-12).
function useRefreshCertificates(envId: string) {
  const queryClient = useQueryClient();
  return async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ingressCertificatesQuery(envId).queryKey }),
      queryClient.invalidateQueries({ queryKey: ingressCertificateBudgetQuery(envId).queryKey }),
    ]);
  };
}

// refused is a write the server answered with a problem: its status and its own sentence, as sent.
function refused(error: unknown, response: Response): IngressActionError {
  const problem = readProblem(error, response);
  return new IngressActionError(problem.status, problem.detail);
}

// useRequestCertificate asks for a certificate for a hostname ahead of any stack's claim (D-12). It needs
// ingress.manage, which the server checks.
export function useRequestCertificate(envId: string) {
  const refresh = useRefreshCertificates(envId);
  return useMutation({
    mutationFn: async (hostname: string): Promise<IngressCert> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/certificates", {
        params: { path: { envId } },
        body: { hostname },
      });
      if (!data) throw refused(error, response);
      return data;
    },
    onSuccess: refresh,
  });
}

// useRetryCertificate orders a failed certificate again now instead of at its next planned attempt.
export function useRetryCertificate(envId: string) {
  const refresh = useRefreshCertificates(envId);
  return useMutation({
    mutationFn: async (certId: string): Promise<void> => {
      const { error, response } = await api.POST(
        "/api/env/{envId}/ingress/certificates/{certId}/retry",
        { params: { path: { envId, certId } } },
      );
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}

export interface CertificateUpload {
  hostname: string;
  certificatePem: string;
  privateKeyPem: string;
}

// useUploadCertificate sends a certificate chain and its private key for a hostname. The key is in the request
// body and nowhere else: the mutation is dropped from the cache the moment it settles (gcTime 0), so the variables
// that hold it do not outlive the dialog (T-27-58).
export function useUploadCertificate(envId: string) {
  const refresh = useRefreshCertificates(envId);
  return useMutation({
    gcTime: 0,
    mutationFn: async (upload: CertificateUpload): Promise<void> => {
      const { error, response } = await api.POST("/api/env/{envId}/ingress/certificates/upload", {
        params: { path: { envId } },
        body: upload,
      });
      if (!response.ok) throw refused(error, response);
    },
    onSuccess: refresh,
  });
}

// useSwitchCertificateToACME returns an uploaded hostname to Let's Encrypt.
export function useSwitchCertificateToACME(envId: string) {
  const refresh = useRefreshCertificates(envId);
  return useMutation({
    mutationFn: async (certId: string): Promise<IngressCert> => {
      const { data, error, response } = await api.POST(
        "/api/env/{envId}/ingress/certificates/{certId}/acme",
        { params: { path: { envId, certId } } },
      );
      if (!data) throw refused(error, response);
      return data;
    },
    onSuccess: refresh,
  });
}
