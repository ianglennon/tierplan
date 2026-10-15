// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { DETAIL_REFETCH_MS, detailRetry } from "@/api/container-detail";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import { isNotASwarm, ServicesLoadError } from "@/api/services";

export type ConfigItem = components["schemas"]["ConfigItem"];
export type ConfigList = components["schemas"]["ConfigListBody"];
export type ConfigDetail = components["schemas"]["ConfigDetailBody"];
export type ConfigUser = components["schemas"]["ConfigUserBody"];
export type ConfigInspect = components["schemas"]["ConfigInspectBody"];

// How long after an answer the list is read once more, in case the Docker event that confirms
// the change is late.
const SETTLE_RECHECK_MS = 2000;

// The query key of an environment's configs list, and of every read that extends it: a searched
// list, a config's page and its inspect document. One invalidation by a config event refreshes
// them all.
export function configsKey(envId: string) {
  return listKey(envId, "configs");
}

// Changing the search keeps the rows on screen until the new ones arrive, so the table does not
// jump to a skeleton. It never carries rows over from another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

// The configs of a swarm environment, narrowed by the search text. The unfiltered list keeps the
// bare key and a searched read extends it. The error is the ServicesLoadError the other swarm
// lists throw, so a standalone host (409 not_a_swarm) is told apart the same way.
export function configsQuery(envId: string, filters: { q?: string } = {}) {
  const set = filters.q ? { q: filters.q } : undefined;
  return queryOptions({
    queryKey: set ? ([...configsKey(envId), set] as const) : configsKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/configs", {
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

// ConfigLoadError is a detail read the server refused or could not answer. The status and the
// problem type tell a config that no longer exists (404 config_not_found) from every other
// failure.
export class ConfigLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET config failed (${status})`);
    this.name = "ConfigLoadError";
    this.status = status;
    this.type = type;
  }
}

// isConfigGone says whether the error is the server answering that the config does not exist:
// it was removed, or a new version replaced it.
export function isConfigGone(error: unknown): boolean {
  return (
    error instanceof ConfigLoadError && error.status === 404 && error.type === "config_not_found"
  );
}

// isConfigNotASwarm says whether the error is the server answering that the environment is a
// standalone host, which has no configs.
export function isConfigNotASwarm(error: unknown): boolean {
  return error instanceof ConfigLoadError && error.status === 409 && error.type === "not_a_swarm";
}

// The key of one config's detail. It extends the configs list key, so the events feed and every
// write, which refresh that key, refresh an open page too.
export function configDetailKey(envId: string, id: string) {
  return [...configsKey(envId), "detail", id] as const;
}

// A config that does not exist will not exist on a second try.
const retryDetail = (count: number, error: unknown) =>
  !isConfigGone(error) && !isConfigNotASwarm(error) && count < detailRetry.attempts;

// One config: its metadata, its content (as base64, and as text when it is valid UTF-8) and every
// service whose spec mentions it. A config is not encrypted: whoever reads configs reads this.
export function configDetailQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: configDetailKey(envId, id),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/configs/{configId}", {
        params: { path: { envId, configId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ConfigLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: retryDetail,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

// The manager's inspect document of one config, under the key of its detail, so the events feed
// and every write refresh an open Inspect tab with the page. It has no data field.
export function configInspectQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...configDetailKey(envId, id), "inspect"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/configs/{configId}/inspect", {
        params: { path: { envId, configId: id } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ConfigLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: retryDetail,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

// ConfigRequestError is a create or delete the server refused or that got no answer. The status
// is 0 when the server could not be reached. The type is the problem type the server named
// (name_taken, name_invalid, value_required, value_too_large, templating_invalid, object_in_use,
// config_not_found, host_read_only ...). It carries no text of the request.
export class ConfigRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string, method = "POST") {
    super(`${method} configs failed (${status})`);
    this.name = "ConfigRequestError";
    this.status = status;
    this.type = type;
  }
}

// refreshLists reads the configs list and the summary again now and once more a moment later,
// whatever the answer was.
export function refreshConfigLists(queryClient: QueryClient, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: configsKey(envId) });
    void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
  };
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

// What a delete is aimed at: the config's ID, and its name, for the words around it. The server
// asks for no typed name.
export interface ConfigTarget {
  id: string;
  name: string;
}

// removeConfig deletes one config. It never retries: a delete whose answer was lost may have
// happened (P2 D-42), and the next read of the list says which.
export async function removeConfig(envId: string, target: ConfigTarget): Promise<void> {
  try {
    const res = await api.DELETE("/api/env/{envId}/configs/{configId}", {
      params: { path: { envId, configId: target.id } },
    });
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new ConfigRequestError(problem.status, problem.type, "DELETE");
    }
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ConfigRequestError ? e : new ConfigRequestError(0, undefined, "DELETE");
  }
}

// useDeleteConfig is the mutation behind the delete confirm. It does not retry, and reads the
// list again whatever the answer, so a config that was already gone leaves the list too.
export function useDeleteConfig(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, ConfigRequestError, ConfigTarget>({
    mutationFn: (target) => removeConfig(envId, target),
    retry: false,
    onSettled: () => refreshConfigLists(queryClient, envId),
  });
}

export type ConfigCreateInput = components["schemas"]["ConfigCreateRequest"];
export type ConfigCreated = components["schemas"]["ConfigCreateBody"];

// postConfig creates one config. The content goes out in the body, as base64, and nowhere else. It
// never retries: a create whose answer was lost may have happened (P2 D-42), and the next read of
// the list says which. A refusal is read for its status and type only; nothing of the request is
// kept in the error.
export async function postConfig(envId: string, body: ConfigCreateInput): Promise<ConfigCreated> {
  try {
    const res = await api.POST("/api/env/{envId}/configs", { params: { path: { envId } }, body });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ConfigRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ConfigRequestError ? e : new ConfigRequestError(0);
  }
}

// useCreateConfig is the mutation behind the create sheet. It does not retry, reads the list again
// whatever the answer, and keeps nothing: gcTime 0 drops the mutation, and the content in its
// variables with it, as soon as nothing observes it. A config is not a secret, but what the
// person typed is still theirs alone until the request is answered.
export function useCreateConfig(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ConfigCreated, ConfigRequestError, ConfigCreateInput>({
    mutationFn: (body) => postConfig(envId, body),
    retry: false,
    gcTime: 0,
    onSettled: () => refreshConfigLists(queryClient, envId),
  });
}
