// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type QueryClient, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { DETAIL_REFETCH_MS, detailRetry } from "@/api/container-detail";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import {
  actionError,
  isNotASwarm,
  ServiceActionError,
  ServicesLoadError,
  servicesKey,
} from "@/api/services";

export type SecretItem = components["schemas"]["SecretItem"];
export type SecretList = components["schemas"]["SecretListBody"];
export type SecretDetail = components["schemas"]["SecretDetailBody"];
export type SecretUser = components["schemas"]["SecretUserBody"];
export type SecretInspect = components["schemas"]["SecretInspectBody"];

// How long after an answer the list is read once more, in case the Docker event that confirms
// the change is late.
const SETTLE_RECHECK_MS = 2000;

// The query key of an environment's secrets list, and of every read that extends it: a searched
// list, a secret's page and its inspect document. One invalidation by a secret event refreshes
// them all.
export function secretsKey(envId: string) {
  return listKey(envId, "secrets");
}

// Changing the search keeps the rows on screen until the new ones arrive, so the table does not
// jump to a skeleton. It never carries rows over from another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

// The secrets of a swarm environment, narrowed by the search text. The unfiltered list keeps the
// bare key and a searched read extends it. The error is the ServicesLoadError the other swarm
// lists throw, so a standalone host (409 not_a_swarm) is told apart the same way.
export function secretsQuery(envId: string, filters: { q?: string } = {}) {
  const set = filters.q ? { q: filters.q } : undefined;
  return queryOptions({
    queryKey: set ? ([...secretsKey(envId), set] as const) : secretsKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/secrets", {
        params: { path: { envId }, query: set },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServicesLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    placeholderData: keepForSameEnv(envId),
    // A standalone host will not turn into a swarm on a second try.
    retry: (count, error) => !isNotASwarm(error) && count < 3,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// SecretLoadError is a detail read the server refused or could not answer. The status and the
// problem type tell a secret that no longer exists (404 secret_not_found) from every other
// failure.
export class SecretLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET secret failed (${status})`);
    this.name = "SecretLoadError";
    this.status = status;
    this.type = type;
  }
}

// isSecretGone says whether the error is the server answering that the secret does not exist:
// it was removed, or a new version replaced it.
export function isSecretGone(error: unknown): boolean {
  return (
    error instanceof SecretLoadError && error.status === 404 && error.type === "secret_not_found"
  );
}

// isSecretNotASwarm says whether the error is the server answering that the environment is a
// standalone host, which has no secrets.
export function isSecretNotASwarm(error: unknown): boolean {
  return error instanceof SecretLoadError && error.status === 409 && error.type === "not_a_swarm";
}

// The key of one secret's detail. It extends the secrets list key, so the events feed and every
// write, which refresh that key, refresh an open page too.
export function secretDetailKey(envId: string, id: string) {
  return [...secretsKey(envId), "detail", id] as const;
}

// A secret that does not exist will not exist on a second try.
const retryDetail = (count: number, error: unknown) =>
  !isSecretGone(error) && !isSecretNotASwarm(error) && count < detailRetry.attempts;

// One secret: its metadata and every service whose spec mentions it. There is no value, ever.
export function secretDetailQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: secretDetailKey(envId, id),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/secrets/{secretId}", {
        params: { path: { envId, secretId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new SecretLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: retryDetail,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

// The manager's inspect document of one secret, under the key of its detail, so the events feed
// and every write refresh an open Inspect tab with the page. It has no data field.
export function secretInspectQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...secretDetailKey(envId, id), "inspect"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/secrets/{secretId}/inspect", {
        params: { path: { envId, secretId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new SecretLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: retryDetail,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

// SecretRequestError is a create or delete the server refused or that got no answer. The status
// is 0 when the server could not be reached. The type is the problem type the server named
// (name_taken, name_invalid, value_required, value_too_large, object_in_use, secret_not_found,
// host_read_only ...). It carries no text of the request.
export class SecretRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string, method = "POST") {
    super(`${method} secrets failed (${status})`);
    this.name = "SecretRequestError";
    this.status = status;
    this.type = type;
  }
}

// refreshLists reads the secrets list and the summary again now and once more a moment later,
// whatever the answer was.
export function refreshSecretLists(queryClient: QueryClient, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: secretsKey(envId) });
    void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
  };
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

// What a delete is aimed at: the secret's ID, and its name, which the server asks to be typed.
export interface SecretTarget {
  id: string;
  name: string;
}

// removeSecret deletes one secret. It never retries: a delete whose answer was lost may have
// happened (P2 D-42), and the next read of the list says which.
export async function removeSecret(envId: string, target: SecretTarget): Promise<void> {
  try {
    const res = await api.DELETE("/api/env/{envId}/secrets/{secretId}", {
      params: {
        path: { envId, secretId: target.id },
        query: { confirm: target.name },
      },
    });
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new SecretRequestError(problem.status, problem.type, "DELETE");
    }
  } catch (e) {
    // A network failure has no status.
    throw e instanceof SecretRequestError ? e : new SecretRequestError(0, undefined, "DELETE");
  }
}

// useDeleteSecret is the mutation behind the delete confirm. It does not retry, and reads the
// list again whatever the answer, so a secret that was already gone leaves the list too.
export function useDeleteSecret(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, SecretRequestError, SecretTarget>({
    mutationFn: (target) => removeSecret(envId, target),
    retry: false,
    onSettled: () => refreshSecretLists(queryClient, envId),
  });
}

export type SecretCreateInput = components["schemas"]["SecretCreateRequest"];
export type SecretCreated = components["schemas"]["SecretCreateBody"];

// postSecret creates one secret. The value goes out in the body, as base64, and nowhere else. It
// never retries: a create whose answer was lost may have happened (P2 D-42), and the next read of
// the list says which. A refusal is read for its status and type only; nothing of the request is
// kept in the error.
export async function postSecret(envId: string, body: SecretCreateInput): Promise<SecretCreated> {
  try {
    const res = await api.POST("/api/env/{envId}/secrets", { params: { path: { envId } }, body });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new SecretRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof SecretRequestError ? e : new SecretRequestError(0);
  }
}

// useCreateSecret is the mutation behind the create sheet. It does not retry, reads the list again
// whatever the answer, and keeps nothing: gcTime 0 drops the mutation, and the value in its
// variables with it, as soon as nothing observes it (T-06-113).
export function useCreateSecret(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<SecretCreated, SecretRequestError, SecretCreateInput>({
    mutationFn: (body) => postSecret(envId, body),
    retry: false,
    gcTime: 0,
    onSettled: () => refreshSecretLists(queryClient, envId),
  });
}

export type RotatePreview = components["schemas"]["RotatePreviewBody"];
export type RotateUser = components["schemas"]["RotateUserBody"];
export type SecretRotateInput = components["schemas"]["SecretRotateRequest"];
export type Rotated = components["schemas"]["RotateBody"];

// secretRotatePreviewQuery is what a rotate of the secret would do under that new name (empty for the
// next version of the old name): the services that use it, each with its spec as it is and as it would
// be. It reads and changes nothing. gcTime 0 drops the masked specs as soon as the dialog stops
// watching, and an error is read for its status and type like any detail read.
export function secretRotatePreviewQuery(envId: string, id: string, newName: string) {
  return queryOptions({
    queryKey: [...secretDetailKey(envId, id), "rotate-preview", newName] as const,
    queryFn: async () => {
      const res = await api.POST("/api/env/{envId}/secrets/{secretId}/rotate/preview", {
        params: { path: { envId, secretId: id } },
        body: newName ? { newName } : {},
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new SecretLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    gcTime: 0,
    staleTime: 0,
  });
}

export interface SecretRotateRequest {
  id: string;
  body: SecretRotateInput;
}

// postSecretRotate rotates one secret to a new version. The value goes out in the body, as base64, and
// nowhere else. It never retries: a rotate whose answer was lost may have happened (P2 D-42), and the
// next read of the list says which. A refusal keeps the problem's type, detail and entries, which name
// the secret and the services and never a value.
export async function postSecretRotate(envId: string, req: SecretRotateRequest): Promise<Rotated> {
  try {
    const res = await api.POST("/api/env/{envId}/secrets/{secretId}/rotate", {
      params: { path: { envId, secretId: req.id } },
      body: req.body,
    });
    if (!res.data) throw actionError(res.error, res.response);
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ServiceActionError ? e : new ServiceActionError(0);
  }
}

// useRotateSecret is the mutation behind the rotate dialog. It does not retry, reads the lists again
// whatever the answer (a run that stopped part-way changed the swarm too), and keeps nothing: gcTime 0
// drops the mutation, and the value in its variables with it, as soon as nothing observes it (T-06-82).
export function useRotateSecret(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<Rotated, ServiceActionError, SecretRotateRequest>({
    mutationFn: (req) => postSecretRotate(envId, req),
    retry: false,
    gcTime: 0,
    onSettled: () => {
      refreshSecretLists(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: servicesKey(envId) });
    },
  });
}
