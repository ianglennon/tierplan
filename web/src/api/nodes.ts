// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

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

export type NodeImpact = components["schemas"]["NodeImpactBody"];
export type NodeImpactTask = components["schemas"]["NodeImpactTask"];

// The changes a node dialog is for, as the impact route knows them. Activate has no dialog.
export type NodeImpactAction = "drain" | "pause" | "promote" | "demote";

// What a change to a node will do, judged by the server from a live read. It extends the node's
// key, so a node event refreshes an open dialog's answer, and it is never kept: a dialog opened
// again reads again, since the warnings are about the swarm as it is now.
export function nodeImpactQuery(envId: string, id: string, action: NodeImpactAction) {
  return queryOptions({
    queryKey: [...nodeKey(envId, id), "impact", action] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/nodes/{nodeId}/impact", {
        params: { path: { envId, nodeId: id }, query: { action } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new NodeLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    staleTime: 0,
    gcTime: 0,
    refetchOnWindowFocus: false,
  });
}

// NodeActionError is a node change the server refused or that got no answer. The status and the
// problem type say which refusal it was (confirmation_required, node_changed, last_manager,
// no_manager_connected, ...); the detail is Docker's own words where the problem carries them.
// A status of 0 is a request that got no answer at all.
export class NodeActionError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly detail?: string;
  constructor(status: number, type?: string, detail?: string) {
    super(`POST node action failed (${status})`);
    this.name = "NodeActionError";
    this.status = status;
    this.type = type;
    this.detail = detail;
  }
}

// isLostNodeAction says whether a node change got no answer. The server sees a write through, so
// it may well have happened: the list, read again after every change, shows it.
export function isLostNodeAction(err: unknown): boolean {
  return err instanceof NodeActionError && err.status === 0;
}

function nodeActionError(body: unknown, response: { status: number }): NodeActionError {
  const problem = readProblem(body, response);
  const detail =
    typeof body === "object" && body !== null && "detail" in body && typeof body.detail === "string"
      ? body.detail
      : undefined;
  return new NodeActionError(problem.status, problem.type, detail);
}

// A change to a node moves its tasks, so the nodes (and every page under them) and the services
// list, which the placement view reads, are read again whatever the answer: a write whose answer
// was lost may have happened (P2 D-42).
function refreshNodes(queryClient: ReturnType<typeof useQueryClient>, envId: string) {
  void queryClient.invalidateQueries({ queryKey: nodesKey(envId) });
  void queryClient.invalidateQueries({ queryKey: listKey(envId, "services") });
}

export type NodeAvailability = "active" | "pause" | "drain";
export type NodeRole = "manager" | "worker";

export interface NodeAvailabilityChange {
  nodeId: string;
  availability: NodeAvailability;
  // The node's version as it was last read: the impact's, or the node page's.
  version: number;
  // The node's name, typed, when the impact said it is needed.
  confirm?: string;
}

export interface NodeRoleChange {
  nodeId: string;
  role: NodeRole;
  version: number;
  confirm?: string;
}

// useSetNodeAvailability is the mutation behind Drain, Pause and Activate. It does not retry.
export function useSetNodeAvailability(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NodeActionError, NodeAvailabilityChange>({
    mutationKey: ["env", envId, "node-action"],
    mutationFn: async ({ nodeId, availability, version, confirm }) => {
      try {
        const res = await api.POST("/api/env/{envId}/nodes/{nodeId}/availability", {
          params: { path: { envId, nodeId } },
          body: { availability, version, ...(confirm ? { confirm } : {}) },
        });
        if (!res.response.ok) throw nodeActionError(res.error, res.response);
      } catch (e) {
        throw e instanceof NodeActionError ? e : new NodeActionError(0);
      }
    },
    retry: false,
    onSettled: () => refreshNodes(queryClient, envId),
  });
}

// useSetNodeRole is the mutation behind Promote and Demote. It does not retry.
export function useSetNodeRole(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NodeActionError, NodeRoleChange>({
    mutationKey: ["env", envId, "node-action"],
    mutationFn: async ({ nodeId, role, version, confirm }) => {
      try {
        const res = await api.POST("/api/env/{envId}/nodes/{nodeId}/role", {
          params: { path: { envId, nodeId } },
          body: { role, version, ...(confirm ? { confirm } : {}) },
        });
        if (!res.response.ok) throw nodeActionError(res.error, res.response);
      } catch (e) {
        throw e instanceof NodeActionError ? e : new NodeActionError(0);
      }
    },
    retry: false,
    onSettled: () => refreshNodes(queryClient, envId),
  });
}

export interface NodeRemoval {
  nodeId: string;
  // The node's host name, typed. The server always asks for it.
  confirm: string;
}

// useRemoveNode is the mutation behind Remove… for a node that is down. It does not retry: a
// removal whose answer was lost may have happened. The nodes are read again whatever the answer,
// and so are the agents, since the server revokes the agent that ran on the node.
export function useRemoveNode(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NodeActionError, NodeRemoval>({
    mutationKey: ["env", envId, "node-remove"],
    mutationFn: async ({ nodeId, confirm }) => {
      try {
        const res = await api.DELETE("/api/env/{envId}/nodes/{nodeId}", {
          params: { path: { envId, nodeId }, query: { confirm } },
        });
        if (!res.response.ok) throw nodeActionError(res.error, res.response);
      } catch (e) {
        throw e instanceof NodeActionError ? e : new NodeActionError(0);
      }
    },
    retry: false,
    onSettled: () => {
      refreshNodes(queryClient, envId);
      void queryClient.invalidateQueries({ queryKey: ["agents", envId] });
    },
  });
}

export type NodeLabelImpact = components["schemas"]["NodeLabelImpactBody"];
export type NodeLabelImpactRow = components["schemas"]["NodeLabelImpactRow"];

// useNodeLabelImpact asks the server what a set of labels would do to the services' placement
// constraints. It is a mutation because each answer is for the labels it was asked about and is
// never cached; the editor asks again whenever the labels change, and only the latest call's
// answer is the one the hook shows. It writes nothing.
export function useNodeLabelImpact(envId: string, nodeId: string) {
  return useMutation<NodeLabelImpact, NodeActionError, Record<string, string>>({
    mutationKey: ["env", envId, "node-label-impact", nodeId],
    mutationFn: async (labels) => {
      try {
        const res = await api.POST("/api/env/{envId}/nodes/{nodeId}/labels/impact", {
          params: { path: { envId, nodeId } },
          body: { labels },
        });
        if (!res.data) throw nodeActionError(res.error, res.response);
        return res.data;
      } catch (e) {
        throw e instanceof NodeActionError ? e : new NodeActionError(0);
      }
    },
    retry: false,
  });
}

export interface NodeLabelsChange {
  nodeId: string;
  // The node's labels as they are to be saved: they replace the ones it has.
  labels: Record<string, string>;
  // The version of the impact the person saw, which the server checks against the node's now.
  version: number;
}

// useSetNodeLabels is the mutation behind Save labels. It does not retry: a write whose answer
// was lost may have happened, so the nodes are read again whatever the answer.
export function useSetNodeLabels(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NodeActionError, NodeLabelsChange>({
    mutationKey: ["env", envId, "node-labels"],
    mutationFn: async ({ nodeId, labels, version }) => {
      try {
        const res = await api.PUT("/api/env/{envId}/nodes/{nodeId}/labels", {
          params: { path: { envId, nodeId } },
          body: { labels, version },
        });
        if (!res.response.ok) throw nodeActionError(res.error, res.response);
      } catch (e) {
        throw e instanceof NodeActionError ? e : new NodeActionError(0);
      }
    },
    retry: false,
    onSettled: () => refreshNodes(queryClient, envId),
  });
}
