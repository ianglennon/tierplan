// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type ContainerDetail = components["schemas"]["ContainerDetailBody"];

// How often an open detail page reads the container again when no Docker event
// has told it to: the same fallback the lists use.
export const DETAIL_REFETCH_MS = 30_000;

// How many times a failed read is tried again. A container that does not exist is
// never tried again. Exported so tests can set it to none.
export const detailRetry = { attempts: 1 };

// DetailLoadError is a detail read the server refused or could not answer. The
// status and the problem type tell a container that no longer exists (404
// container_not_found) from every other failure.
export class DetailLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET container failed (${status})`);
    this.name = "DetailLoadError";
    this.status = status;
    this.type = type;
  }
}

// isGone says whether the error is the engine answering that the container does
// not exist: it was removed, or recreated under a new ID.
export function isGone(error: unknown): boolean {
  return (
    error instanceof DetailLoadError && error.status === 404 && error.type === "container_not_found"
  );
}

// The key of one container's detail. It extends the containers list key, so the
// events feed and every action, which refresh that key, refresh an open detail
// page too.
export function containerDetailKey(envId: string, id: string, nodeId: string | undefined) {
  return [...listKey(envId, "containers"), "detail", nodeId ?? "", id] as const;
}

export function containerDetailQuery(envId: string, id: string, nodeId?: string) {
  return queryOptions({
    queryKey: containerDetailKey(envId, id, nodeId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/containers/{containerId}", {
        params: { path: { envId, containerId: id }, query: nodeId ? { nodeId } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new DetailLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // A container that does not exist will not exist on a second try.
    retry: (count, error) => !isGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

export type ContainerInspect = components["schemas"]["ContainerInspectBody"];

// The raw inspect document of one container, under the key of its detail, so the
// events feed and every action refresh an open Inspect tab with the page. The server
// has already replaced every value the caller may not see.
export function containerInspectQuery(envId: string, id: string, nodeId?: string) {
  return queryOptions({
    queryKey: [...containerDetailKey(envId, id, nodeId), "inspect"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/containers/{containerId}/inspect", {
        params: { path: { envId, containerId: id }, query: nodeId ? { nodeId } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new DetailLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}
