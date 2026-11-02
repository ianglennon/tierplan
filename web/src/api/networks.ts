// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type QueryClient, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { DETAIL_REFETCH_MS, detailRetry } from "@/api/container-detail";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type NetworkCreateRequest = components["schemas"]["NetworkCreateRequest"];
export type NetworkConnectRequest = components["schemas"]["NetworkConnectRequest"];
export type NetworkCreated = components["schemas"]["NetworkCreateBody"];

// How long after an answer the list is read once more, in case the Docker event
// that confirms the change is late.
const SETTLE_RECHECK_MS = 2000;

// NetworkRequestError is a create or delete the server refused or that got no
// answer. The status is 0 when the server could not be reached. The type is the
// problem type the server named (name_taken, host_read_only, node_required,
// subnet_invalid, network_in_use, network_builtin, network_not_found ...).
export class NetworkRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string, method = "POST") {
    super(`${method} networks failed (${status})`);
    this.name = "NetworkRequestError";
    this.status = status;
    this.type = type;
  }
}

// postNetwork creates one network. It never retries: a create whose answer was
// lost may have happened (P2 D-42).
export async function postNetwork(
  envId: string,
  body: NetworkCreateRequest,
): Promise<NetworkCreated> {
  try {
    const res = await api.POST("/api/env/{envId}/networks", { params: { path: { envId } }, body });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new NetworkRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof NetworkRequestError ? e : new NetworkRequestError(0);
  }
}

// refreshLists reads the networks list and the summary again now and once more a
// moment later, whatever the answer was.
function refreshLists(queryClient: QueryClient, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: listKey(envId, "networks") });
    void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
  };
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

// useCreateNetwork is the mutation behind the create dialog. It does not retry,
// and reads the list again whatever the answer.
export function useCreateNetwork(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<NetworkCreated, NetworkRequestError, NetworkCreateRequest>({
    mutationFn: (body) => postNetwork(envId, body),
    retry: false,
    onSettled: () => refreshLists(queryClient, envId),
  });
}

// refreshAttachments is refreshLists for a connect or a disconnect, which change what a
// container is attached to as well: the containers list and every open container page
// (their keys extend the containers key) are read again too. An open network page is
// under the networks key, so it follows.
function refreshAttachments(queryClient: QueryClient, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: listKey(envId, "containers") });
  };
  refreshLists(queryClient, envId);
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

// What a connect is aimed at: the network, and the body naming the container, its node
// and the optional alias and addresses.
export interface ConnectTarget {
  networkId: string;
  body: NetworkConnectRequest;
}

// connectToNetwork attaches one container to one network. It never retries: a connect
// whose answer was lost may have happened (P2 D-42), and the next read says which.
export async function connectToNetwork(envId: string, target: ConnectTarget): Promise<void> {
  try {
    const res = await api.POST("/api/env/{envId}/networks/{networkId}/connect", {
      params: { path: { envId, networkId: target.networkId } },
      body: target.body,
    });
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new NetworkRequestError(problem.status, problem.type);
    }
  } catch (e) {
    // A network failure has no status.
    throw e instanceof NetworkRequestError ? e : new NetworkRequestError(0);
  }
}

// useConnectContainer is the mutation behind the connect dialog. It does not retry, and
// reads the lists again whatever the answer.
export function useConnectContainer(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NetworkRequestError, ConnectTarget>({
    mutationFn: (target) => connectToNetwork(envId, target),
    retry: false,
    onSettled: () => refreshAttachments(queryClient, envId),
  });
}

// What a disconnect is aimed at: the network and the container on its node.
export interface DisconnectTarget {
  networkId: string;
  containerId: string;
  nodeId: string;
  // The container's name, typed, which the server asks for on TierPlan's own server
  // container and on an agent's (422 confirmation_required).
  confirm?: string;
}

// disconnectFromNetwork detaches one container from one network. It never retries: a
// disconnect whose answer was lost may have happened (P2 D-42), and the next read says
// which. There is no force.
export async function disconnectFromNetwork(
  envId: string,
  target: DisconnectTarget,
): Promise<void> {
  try {
    const res = await api.POST("/api/env/{envId}/networks/{networkId}/disconnect", {
      params: { path: { envId, networkId: target.networkId } },
      body: {
        containerId: target.containerId,
        nodeId: target.nodeId,
        ...(target.confirm ? { confirm: target.confirm } : {}),
      },
    });
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new NetworkRequestError(problem.status, problem.type);
    }
  } catch (e) {
    // A network failure has no status.
    throw e instanceof NetworkRequestError ? e : new NetworkRequestError(0);
  }
}

// useDisconnectContainer is the mutation behind the disconnect confirm. It does not
// retry, and reads the lists again whatever the answer, so a container that was already
// gone from the network leaves its row too.
export function useDisconnectContainer(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NetworkRequestError, DisconnectTarget>({
    mutationFn: (target) => disconnectFromNetwork(envId, target),
    retry: false,
    onSettled: () => refreshAttachments(queryClient, envId),
  });
}

// What a delete is aimed at: the network's ID and, for a local network, the node
// it is on (the row's nodeId).
export interface NetworkTarget {
  id: string;
  nodeId?: string;
}

// removeNetwork deletes one network. It never retries: a delete whose answer was
// lost may have happened (P2 D-42), and the next read of the list says which.
export async function removeNetwork(envId: string, target: NetworkTarget): Promise<void> {
  try {
    const res = await api.DELETE("/api/env/{envId}/networks/{networkId}", {
      params: {
        path: { envId, networkId: target.id },
        query: target.nodeId ? { nodeId: target.nodeId } : undefined,
      },
    });
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new NetworkRequestError(problem.status, problem.type, "DELETE");
    }
  } catch (e) {
    // A network failure has no status.
    throw e instanceof NetworkRequestError ? e : new NetworkRequestError(0, undefined, "DELETE");
  }
}

// useDeleteNetwork is the mutation behind the delete confirm. It does not retry,
// and reads the list again whatever the answer, so a network that was already gone
// leaves the list too.
export function useDeleteNetwork(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, NetworkRequestError, NetworkTarget>({
    mutationFn: (target) => removeNetwork(envId, target),
    retry: false,
    onSettled: () => refreshLists(queryClient, envId),
  });
}

export type NetworkDetail = components["schemas"]["NetworkDetailBody"];
export type NetworkAttachment = components["schemas"]["NetworkAttachmentBody"];

// NetworkLoadError is a detail read the server refused or could not answer. The
// status and the problem type tell a network that no longer exists (404
// network_not_found) from every other failure.
export class NetworkLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET network failed (${status})`);
    this.name = "NetworkLoadError";
    this.status = status;
    this.type = type;
  }
}

// isNetworkGone says whether the error is the server answering that the network
// does not exist: it was removed, or a deploy replaced it.
export function isNetworkGone(error: unknown): boolean {
  return (
    error instanceof NetworkLoadError && error.status === 404 && error.type === "network_not_found"
  );
}

// The key of one network's detail. It extends the networks list key, so the events
// feed and every write, which refresh that key, refresh an open detail page too.
export function networkDetailKey(envId: string, id: string, nodeId: string | undefined) {
  return [...listKey(envId, "networks"), "detail", nodeId ?? "", id] as const;
}

// One network, with what is attached to it. nodeId names the node of a local network.
export function networkDetailQuery(envId: string, id: string, nodeId?: string) {
  return queryOptions({
    queryKey: networkDetailKey(envId, id, nodeId),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/networks/{networkId}", {
        params: { path: { envId, networkId: id }, query: nodeId ? { nodeId } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new NetworkLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // A network that does not exist will not exist on a second try.
    retry: (count, error) => !isNetworkGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

export type NetworkInspect = components["schemas"]["NetworkInspectBody"];

// The raw inspect document of one network, under the key of its detail, so the events
// feed and every write refresh an open Inspect tab with the page.
export function networkInspectQuery(envId: string, id: string, nodeId?: string) {
  return queryOptions({
    queryKey: [...networkDetailKey(envId, id, nodeId), "inspect"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/networks/{networkId}/inspect", {
        params: { path: { envId, networkId: id }, query: nodeId ? { nodeId } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new NetworkLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isNetworkGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}
