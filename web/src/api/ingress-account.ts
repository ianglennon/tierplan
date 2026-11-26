// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

// The install-wide state of the per-hostname (ingress) certificates: whether an Admin accepted the
// terms of the directories, and whether issuing is paused, as it is after a restore.
export type IngressAccount = components["schemas"]["IngressCertAccountBody"];
export type IngressAccountDirectory = components["schemas"]["IngressCertAccountDirectoryBody"];

// The ["ingress", "account"] query. It is a global settings.read route, so a caller enables it only for
// an instance admin. A refused or failed read is not retried by itself: the card offers a retry button.
export const ingressAccountQuery = queryOptions({
  queryKey: ["ingress", "account"],
  queryFn: async (): Promise<IngressAccount> => {
    const { data } = await api.GET("/api/ingress/certificates/account");
    if (!data) throw new Error("GET /api/ingress/certificates/account failed");
    return data;
  },
  retry: false,
});

// IngressAccountError is a write the server refused. detail is the sentence the server worded for the
// refusal, when it sent one, and type is the problem type ("ingress_certs_not_paused", ...).
export class IngressAccountError extends Error {
  readonly status: number;
  readonly type: string | undefined;
  readonly detail: string | undefined;

  constructor(operation: string, status: number, type?: string, detail?: string) {
    super(`${operation} failed with ${status}`);
    this.name = "IngressAccountError";
    this.status = status;
    this.type = type;
    this.detail = detail;
  }
}

// useAcceptIngressTerms acknowledges the terms of the directories for this install (agreeTos is always
// true: there is no way to withdraw it) and reads the account again, so the card shows it as accepted.
// The mutation stays pending until that read is done.
export function useAcceptIngressTerms() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<IngressAccount> => {
      const { data, error, response } = await api.PUT("/api/ingress/certificates/account", {
        body: { agreeTos: true },
      });
      if (!data) {
        const problem = readProblem(error, response);
        throw new IngressAccountError(
          "PUT /api/ingress/certificates/account",
          response.status,
          problem.type,
          problem.detail,
        );
      }
      return data;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ingressAccountQuery.queryKey }),
  });
}

// useResumeIngressCertificates lifts the pause a restore puts on ordering and renewing certificates,
// then reads the account again.
export function useResumeIngressCertificates() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (): Promise<void> => {
      const { error, response } = await api.POST("/api/ingress/certificates/resume");
      if (!response.ok) {
        const problem = readProblem(error, response);
        throw new IngressAccountError(
          "POST /api/ingress/certificates/resume",
          response.status,
          problem.type,
          problem.detail,
        );
      }
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ingressAccountQuery.queryKey }),
  });
}
