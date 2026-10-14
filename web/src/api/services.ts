// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";

export type ServiceItem = components["schemas"]["ServiceItem"];
export type ServiceScaleTarget = components["schemas"]["ServiceScaleTarget"];
export type ServiceScaleResult = components["schemas"]["ServiceScaleResult"];
export type ServiceScaleCode = NonNullable<ServiceScaleResult["code"]>;

// The query key of an environment's services list, and of every read that extends it.
export function servicesKey(envId: string) {
  return ["env", envId, "services"] as const;
}

// ServicesLoadError is a services list the server refused or could not answer. The status and
// the problem type tell a standalone host (409 not_a_swarm) from every other failure.
export class ServicesLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET services failed (${status})`);
    this.name = "ServicesLoadError";
    this.status = status;
    this.type = type;
  }
}

// isNotASwarm says whether the error is the server answering that the environment is a
// standalone host, which has no services.
export function isNotASwarm(error: unknown): boolean {
  return error instanceof ServicesLoadError && error.status === 409 && error.type === "not_a_swarm";
}

// The services of a swarm environment. The rows stay on screen while a refetch is out.
export function servicesQuery(envId: string) {
  return queryOptions({
    queryKey: servicesKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/services", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ServicesLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // A standalone host will not turn into a swarm on a second try.
    retry: (count, error) => !isNotASwarm(error) && count < 3,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// ScaleRequestError is a scale the server refused as a whole (permission lost, a count out of
// range, a standalone host) or that got no answer. The status is 0 when the server could not
// be reached; the type is the problem type the server named. A service that failed is not this:
// it is a result.
export class ScaleRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST services/scale failed (${status})`);
    this.name = "ScaleRequestError";
    this.status = status;
    this.type = type;
  }
}

// isLostScale says whether a request got no answer at all. The server sees a write through, so
// it may well have happened: the list, read again after every request, shows the outcome.
export function isLostScale(err: unknown): boolean {
  return err instanceof ScaleRequestError && err.status === 0;
}

// postScale sets the replica count of one or more services. It never retries: a write whose
// answer was lost may have happened (P2 D-42), and the person asks again.
export async function postScale(
  envId: string,
  targets: ServiceScaleTarget[],
): Promise<ServiceScaleResult[]> {
  try {
    const res = await api.POST("/api/env/{envId}/services/scale", {
      params: { path: { envId } },
      body: { targets },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ScaleRequestError(problem.status, problem.type);
    }
    return res.data.results ?? [];
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ScaleRequestError ? e : new ScaleRequestError(0);
  }
}

// useScaleServices is the mutation behind the scale dialog. It does not retry, and reads the
// list again whatever the answer: a write whose answer was lost may have happened.
export function useScaleServices(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ServiceScaleResult[], ScaleRequestError, ServiceScaleTarget[]>({
    mutationFn: (targets) => postScale(envId, targets),
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: servicesKey(envId) });
    },
  });
}
