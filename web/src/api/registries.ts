// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type Credential = components["schemas"]["CredentialBody"];
export type CredentialRequest = components["schemas"]["CredentialRequest"];
export type CredentialPatch = components["schemas"]["CredentialPatchRequest"];
export type RegistryType = Credential["type"];
export type ResolveResult = components["schemas"]["ResolveBody"];
export type ResolvedCredential = components["schemas"]["ResolvedCredentialBody"];
export type ConsideredCredential = components["schemas"]["ConsideredCredentialBody"];

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
  // The name of the other credential a 409 clashed with, when the server gave it.
  get conflictName(): string | undefined {
    return this.fields.find((f) => f.location === "conflict.credential")?.message;
  }
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

// postCredential adds one credential: of the environment named by envId, or an instance
// credential when there is none. It never retries: a create whose answer was lost may have
// happened (P2 D-42). The submitted body is never copied into the error, so the secret travels
// no further than the request.
export async function postCredential(body: CredentialRequest, envId?: string): Promise<Credential> {
  try {
    const res = envId
      ? await api.POST("/api/env/{envId}/registries", { params: { path: { envId } }, body })
      : await api.POST("/api/registries", { body });
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

// What the create mutation is given: the request, and the environment it is for. An absent
// envId is the instance layer; the scope chosen in the sheet picks the route family.
export interface CreateCredentialVariables {
  body: CredentialRequest;
  envId?: string;
}

// useCreateCredential is the mutation behind the sheet's Save credential. It reads the
// list again whatever the answer, and puts nothing the user typed in the cache: the
// request body is the mutation's variables, which are dropped when it settles.
export function useCreateCredential() {
  const queryClient = useQueryClient();
  return useMutation<Credential, CredentialRequestError, CreateCredentialVariables>({
    mutationFn: ({ body, envId }) => postCredential(body, envId),
    retry: false,
    // The mutation's variables hold the secret that was typed. With no time to live
    // the cache lets go of them as soon as the sheet that made the request is gone.
    gcTime: 0,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: REGISTRIES_KEY });
    },
  });
}

// patchCredential changes one credential, through the route of its layer: the environment's
// when envId is given, the instance's otherwise. Members left out keep what is stored; a secret
// left out (or empty) keeps the stored secret, which no route ever returns. It never retries,
// and the submitted body is never copied into the error.
export async function patchCredential(
  id: string,
  body: CredentialPatch,
  envId?: string,
): Promise<Credential> {
  try {
    const res = envId
      ? await api.PATCH("/api/env/{envId}/registries/{credentialId}", {
          params: { path: { envId, credentialId: id } },
          body,
        })
      : await api.PATCH("/api/registries/{credentialId}", {
          params: { path: { credentialId: id } },
          body,
        });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new CredentialRequestError(problem.status, problem.type, problem.fields, "PATCH");
    }
    return res.data;
  } catch (e) {
    throw e instanceof CredentialRequestError
      ? e
      : new CredentialRequestError(0, undefined, [], "PATCH");
  }
}

// What the update mutation is given: the credential, the change, and the environment the
// credential belongs to (absent for an instance credential), which picks the route family.
export interface UpdateCredentialVariables {
  id: string;
  body: CredentialPatch;
  envId?: string;
}

// useUpdateCredential is the mutation behind the edit sheet's Save changes. Like the
// create it keeps nothing the user typed: no retry, no cache time for the variables that
// hold a replacement secret, and the list and the credential are read again whatever the
// answer.
export function useUpdateCredential() {
  const queryClient = useQueryClient();
  return useMutation<Credential, CredentialRequestError, UpdateCredentialVariables>({
    mutationFn: ({ id, body, envId }) => patchCredential(id, body, envId),
    retry: false,
    gcTime: 0,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: REGISTRIES_KEY });
    },
  });
}

// deleteCredential removes one credential, through the route of its layer: the environment's
// when envId is given, the instance's otherwise. It never retries: a repeat after a lost answer
// would find it gone, and that is told apart by the 404.
export async function deleteCredential(id: string, envId?: string): Promise<void> {
  try {
    const res = envId
      ? await api.DELETE("/api/env/{envId}/registries/{credentialId}", {
          params: { path: { envId, credentialId: id } },
        })
      : await api.DELETE("/api/registries/{credentialId}", {
          params: { path: { credentialId: id } },
        });
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new CredentialRequestError(problem.status, problem.type, problem.fields, "DELETE");
    }
  } catch (e) {
    throw e instanceof CredentialRequestError
      ? e
      : new CredentialRequestError(0, undefined, [], "DELETE");
  }
}

// What the delete mutation is given: the credential and, for an environment's, the environment.
export interface DeleteCredentialVariables {
  id: string;
  envId?: string;
}

// useDeleteCredential is the mutation behind the delete dialog. It reads the list again
// whatever the answer, and drops the deleted credential's own query so a stale copy is
// never shown.
export function useDeleteCredential() {
  const queryClient = useQueryClient();
  return useMutation<void, CredentialRequestError, DeleteCredentialVariables>({
    mutationFn: ({ id, envId }) => deleteCredential(id, envId),
    retry: false,
    onSettled: (_data, _error, { id }) => {
      queryClient.removeQueries({ queryKey: [...REGISTRIES_KEY, "detail", id] });
      void queryClient.invalidateQueries({ queryKey: REGISTRIES_KEY });
    },
  });
}

// ResolveRequestError is a check the server refused or that got no answer. status is 0
// when the server could not be reached; type is the problem type it named (ref_invalid).
export class ResolveRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST registries/resolve failed (${status})`);
    this.name = "ResolveRequestError";
    this.status = status;
    this.type = type;
  }
}

// postResolve asks which credential a pull of the image would use on one environment. It
// is the same function pulls choose with (D-10), so the answer cannot drift from a pull.
// Nothing in the answer is a secret.
export async function postResolve(envId: string, ref: string): Promise<ResolveResult> {
  try {
    const res = await api.POST("/api/env/{envId}/registries/resolve", {
      params: { path: { envId } },
      body: { ref },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ResolveRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    throw e instanceof ResolveRequestError ? e : new ResolveRequestError(0);
  }
}

// useResolveCredential is the mutation behind the "Which credential?" tester and the pull
// form's credential preview. A read, so it writes nothing and invalidates nothing; being a
// mutation, a call replaces the one before it, so only the latest answer is ever shown.
export function useResolveCredential(envId: string) {
  return useMutation<ResolveResult, ResolveRequestError, string>({
    mutationFn: (ref) => postResolve(envId, ref),
    retry: false,
  });
}
