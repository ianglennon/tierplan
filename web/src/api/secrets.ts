// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { DETAIL_REFETCH_MS, detailRetry } from "@/api/container-detail";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import { isNotASwarm, ServicesLoadError } from "@/api/services";

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
