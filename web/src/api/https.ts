// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type HTTPSStatus = components["schemas"]["HTTPSStatusBody"];
export type HTTPSCertificate = components["schemas"]["HTTPSCertificateBody"];

// The ["https", "status"] query: the certificate the listener serves now, whatever its source.
// It is read again every minute and when the window gets focus back, so a renewal shows up.
export const httpsStatusQuery = queryOptions({
  queryKey: ["https", "status"],
  queryFn: async (): Promise<HTTPSStatus> => {
    const { data } = await api.GET("/api/https/status");
    if (!data) throw new Error("GET /api/https/status failed");
    return data;
  },
  refetchInterval: 60_000,
  refetchOnWindowFocus: true,
});

export type HTTPSACME = components["schemas"]["HTTPSACMEBody"];
export type ACMEDirectory = components["schemas"]["HTTPSACMEDirectoryBody"];
export type ACMEAccount = components["schemas"]["HTTPSACMEAccountBody"];

// The ["https", "acme", "directory"] query: the CA's terms of service address and the profiles it
// offers. The server asks the CA for it, so callers enable it only when the card is neither paused
// nor in certificate-file mode, and it is kept a day.
export const acmeDirectoryQuery = queryOptions({
  queryKey: ["https", "acme", "directory"],
  queryFn: async (): Promise<ACMEDirectory> => {
    const { data } = await api.GET("/api/https/acme/directory");
    if (!data) throw new Error("GET /api/https/acme/directory failed");
    return data;
  },
  staleTime: 24 * 60 * 60_000,
});

// ACMEAccountError is a registration the server refused. type is the problem type
// ("acme_contact_invalid", "acme_tos_required", "acme_ca_error", ...), when it sent one, and caClass
// is the failure class of a CA error ("ca_unavailable", ...).
export class ACMEAccountError extends Error {
  readonly status: number;
  readonly type: string | undefined;
  readonly caClass: string | undefined;

  constructor(status: number, type?: string, caClass?: string) {
    super(`POST /api/https/acme/account failed with ${status}`);
    this.name = "ACMEAccountError";
    this.status = status;
    this.type = type;
    this.caClass = caClass;
  }
}

export interface RegisterACMEAccountValues {
  email: string;
  agreeTos: boolean;
}

// useRegisterACMEAccount registers the Let's Encrypt account and reads the status again, so the
// summary replaces the form.
export function useRegisterACMEAccount() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (values: RegisterACMEAccountValues): Promise<ACMEAccount> => {
      const { data, error, response } = await api.POST("/api/https/acme/account", {
        body: values.email === "" ? { agreeTos: values.agreeTos } : values,
      });
      if (!data) {
        const problem = readProblem(error, response);
        const caClass = problem.fields.find((f) => f.location === "class")?.message;
        throw new ACMEAccountError(response.status, problem.type, caClass);
      }
      return data;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["https", "status"] }),
  });
}
