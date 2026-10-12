// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type Credential = components["schemas"]["CredentialBody"];
export type CredentialRequest = components["schemas"]["CredentialRequest"];
export type RegistryType = Credential["type"];

// How often the Last test and Last use cells are read again while the page is open.
// There is no stream for credentials (UI-SPEC "Live updates").
export const CREDENTIALS_REFETCH_MS = 60_000;

// Every registry query sits under this key, so one invalidation refreshes the list
// and the credential a sheet shows.
export const REGISTRIES_KEY = ["registries"] as const;

// The ["registries", "list"] query: every credential the caller may see. The server
// never includes a secret; it sends only the last characters it kept.
export const credentialsQuery = queryOptions({
  queryKey: [...REGISTRIES_KEY, "list"],
  queryFn: async () => {
    const { data } = await api.GET("/api/registries");
    if (!data) throw new Error("GET /api/registries failed");
    return data.items ?? [];
  },
  refetchInterval: CREDENTIALS_REFETCH_MS,
  refetchOnWindowFocus: true,
});

// CredentialRequestError is a request the server refused or that got no answer.
// status is 0 when the server could not be reached; type is the problem type it
// named (credential_name_taken, host_prefix_taken, hub_fallback_taken,
// validation_failed ...); fields holds the codes of a 422, one per field at fault.
export class CredentialRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  constructor(status: number, type?: string, fields: ProblemField[] = [], method = "GET") {
    super(`${method} registries failed (${status})`);
    this.name = "CredentialRequestError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }
}

// credentialQuery reads one credential, for the sheet a deep link opens. A credential
// the caller may not see answers like one that does not exist (404), and that is not
// retried.
export const credentialQuery = (id: string) =>
  queryOptions({
    queryKey: [...REGISTRIES_KEY, "detail", id],
    queryFn: async (): Promise<Credential> => {
      try {
        const res = await api.GET("/api/registries/{credentialId}", {
          params: { path: { credentialId: id } },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new CredentialRequestError(problem.status, problem.type, problem.fields);
        }
        return res.data;
      } catch (e) {
        throw e instanceof CredentialRequestError ? e : new CredentialRequestError(0);
      }
    },
    retry: (count, error) =>
      count < 2 && !(error instanceof CredentialRequestError && error.status === 404),
  });

// postCredential adds one instance credential. It never retries: a create whose answer
// was lost may have happened (P2 D-42). The submitted body is never copied into the
// error, so the secret travels no further than the request.
export async function postCredential(body: CredentialRequest): Promise<Credential> {
  try {
    const res = await api.POST("/api/registries", { body });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new CredentialRequestError(problem.status, problem.type, problem.fields, "POST");
    }
    return res.data;
  } catch (e) {
    throw e instanceof CredentialRequestError
      ? e
      : new CredentialRequestError(0, undefined, [], "POST");
  }
}

// useCreateCredential is the mutation behind the sheet's Save credential. It reads the
// list again whatever the answer, and puts nothing the user typed in the cache: the
// request body is the mutation's variables, which are dropped when it settles.
export function useCreateCredential() {
  const queryClient = useQueryClient();
  return useMutation<Credential, CredentialRequestError, CredentialRequest>({
    mutationFn: postCredential,
    retry: false,
    // The mutation's variables hold the secret that was typed. With no time to live
    // the cache lets go of them as soon as the sheet that made the request is gone.
    gcTime: 0,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: REGISTRIES_KEY });
    },
  });
}
