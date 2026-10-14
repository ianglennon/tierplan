// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS, listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import { isNotASwarm, ServicesLoadError } from "@/api/services";

export type NodeItem = components["schemas"]["NodeItem"];
export type NodeDetail = components["schemas"]["NodeDetailBody"];
export type NodeList = components["schemas"]["NodeListBody"];
export type NodeInspect = components["schemas"]["NodeInspectBody"];
export type NodeTasks = components["schemas"]["NodeTasksBody"];
export type NodeTaskItem = components["schemas"]["NodeTaskItem"];

// The query key of an environment's nodes list, and of every read that extends it: a filtered
// list, a node's page, its tasks and its inspect document. One invalidation by a node event
// refreshes them all.
export function nodesKey(envId: string) {
  return listKey(envId, "nodes");
}

// The nodes list and the node pages read again this often while the page is visible. Docker
// sends no task events, so the task counts need the poll; a node event refreshes sooner.
export const NODES_REFETCH_MS = 10_000;

const visibleEvery = (ms: number) => () => (document.visibilityState === "visible" ? ms : false);

// What narrows the nodes list on the server: the search text and the comma list of status chips
// (managers, workers, drained, down). Each is left out when it is not set.
export interface NodeFilters {
  q?: string;
  status?: string;
}

// Changing the search or the chips keeps the rows on screen until the new ones arrive, so the
// table does not jump to a skeleton. It never carries rows over from another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

// The nodes of a swarm environment, narrowed by the filters. The unfiltered list keeps the bare
// key and every filtered read extends it. The error is the ServicesLoadError the other swarm
// reads throw, so a standalone host (409 not_a_swarm) is told from every other failure.
export function nodesQuery(envId: string, filters: NodeFilters = {}) {
  const set: NodeFilters = {};
  if (filters.q) set.q = filters.q;
  if (filters.status) set.status = filters.status;
  const filtered = Object.keys(set).length > 0;
  return queryOptions({
    queryKey: filtered ? ([...nodesKey(envId), set] as const) : nodesKey(envId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/nodes", {
        params: { path: { envId }, query: filtered ? set : undefined },
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
    refetchInterval: visibleEvery(NODES_REFETCH_MS),
  });
}

// NodeLoadError is a node read the server refused or could not answer. The status and the
// problem type tell a node that no longer exists (404 node_not_found) from every other failure.
export class NodeLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET node failed (${status})`);
    this.name = "NodeLoadError";
    this.status = status;
    this.type = type;
  }
}

// isNodeGone says whether the error is the swarm answering that the node does not exist: it left
// the swarm or was removed.
export function isNodeGone(error: unknown): boolean {
  return error instanceof NodeLoadError && error.status === 404 && error.type === "node_not_found";
}

// How many times a failed node read is tried again. A node that does not exist is never tried
// again. Exported so tests can set it to none.
export const nodeRetry = { attempts: 1 };

// The key of one node's page. It extends the nodes list key, so the events feed refreshes an
// open node page with the list.
export function nodeKey(envId: string, id: string) {
  return [...nodesKey(envId), "detail", id] as const;
}

async function readNode<T>(
  read: () => Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  const res = await read();
  if (!res.data) {
    const problem = readProblem(res.error, res.response);
    throw new NodeLoadError(problem.status, problem.type);
  }
  return res.data;
}

const retryUnlessGone = (count: number, error: unknown) =>
  !isNodeGone(error) && count < nodeRetry.attempts;

// The facts of one node, read live from a manager.
export function nodeQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: nodeKey(envId, id),
    queryFn: () =>
      readNode(() =>
        api.GET("/api/env/{envId}/nodes/{nodeId}", { params: { path: { envId, nodeId: id } } }),
      ),
    retry: retryUnlessGone,
    refetchInterval: visibleEvery(LIST_REFETCH_MS),
  });
}

// The raw inspect document of one node, under the key of its page.
export function nodeInspectQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...nodeKey(envId, id), "inspect"] as const,
    queryFn: () =>
      readNode(() =>
        api.GET("/api/env/{envId}/nodes/{nodeId}/inspect", {
          params: { path: { envId, nodeId: id } },
        }),
      ),
    retry: retryUnlessGone,
    refetchInterval: visibleEvery(LIST_REFETCH_MS),
  });
}

// The tasks of one node: those that should be running, then the newest of the others. Polled
// while the page is visible, since Docker sends no task events.
export function nodeTasksQuery(envId: string, id: string) {
  return queryOptions({
    queryKey: [...nodeKey(envId, id), "tasks"] as const,
    queryFn: () =>
      readNode(() =>
        api.GET("/api/env/{envId}/nodes/{nodeId}/tasks", {
          params: { path: { envId, nodeId: id } },
        }),
      ),
    retry: retryUnlessGone,
    refetchInterval: visibleEvery(NODES_REFETCH_MS),
  });
}
