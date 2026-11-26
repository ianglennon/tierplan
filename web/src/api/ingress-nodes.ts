// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  infiniteQueryOptions,
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "@/api/client";
import {
  IngressActionError,
  IngressLoadError,
  ingressKey,
  ingressRefetchInterval,
} from "@/api/ingress";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type IngressNode = components["schemas"]["IngressNodeBody"];
export type IngressNodeList = components["schemas"]["IngressNodeListBody"];
export type RevokeIngressNodeResult = components["schemas"]["RevokeIngressNodeBodyOut"];
export type IngressVIP = components["schemas"]["IngressVIPBody"];
export type IngressVIPCandidate = components["schemas"]["IngressVIPCandidateBody"];
export type IngressVIPManager = components["schemas"]["IngressVIPManagerBody"];
export type IngressVIPMove = components["schemas"]["IngressVIPMoveBody"];
export type IngressVIPMoves = components["schemas"]["IngressVIPMovesBody"];

// How many moves the history reads at a time (UI-SPEC S4).
export const VIP_MOVES_PAGE_SIZE = 10;

function visible(): boolean {
  return document.visibilityState === "visible";
}

// nodesSettling says whether any active node still holds an older bundle than the one it should: a person who just
// changed a setting is watching the table, so it reads at the quick cadence until every node has caught up (D-24).
export function nodesSettling(data: IngressNodeList | undefined): boolean {
  return (data?.items ?? []).some((n) => n.status === "active" && n.sync === "pending");
}

// Every ingress node of the environment, revoked ones included, oldest first, with the bundle the nodes should hold.
// A failed read is not retried: the tab has its own Retry and a 403 must answer at once.
export function ingressNodesQuery(envId: string) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "nodes"] as const,
    queryFn: async (): Promise<IngressNodeList> => {
      const res = await api.GET("/api/env/{envId}/ingress/nodes", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    refetchInterval: (query) => ingressRefetchInterval(nodesSettling(query.state.data), visible()),
  });
}

// vipSettling says whether the address has no holder: a person who sees that is waiting for one to take it, so the
// card reads at the quick cadence until a node does (D-24).
export function vipSettling(data: IngressVIP | null | undefined): boolean {
  return data != null && data.holder.state === "none";
}

// The swarm's virtual IP: the address, who holds it, how each candidate stands and what could stop it moving. It is
// read only on a swarm (`enabled`); a swarm with none answers 404 vip_not_enabled, which is the answer null and not an
// error. Any other failure is not retried: the card shows its own error and a 403 must answer at once.
export function ingressVIPQuery(envId: string, enabled: boolean) {
  return queryOptions({
    queryKey: [...ingressKey(envId), "vip"] as const,
    enabled,
    queryFn: async (): Promise<IngressVIP | null> => {
      const res = await api.GET("/api/env/{envId}/ingress/vip", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        if (problem.status === 404 && problem.type === "vip_not_enabled") return null;
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    refetchInterval: (query) => ingressRefetchInterval(vipSettling(query.state.data), visible()),
  });
}

// The moves of the address, newest first, VIP_MOVES_PAGE_SIZE at a time. A page's `next` is the cursor the following,
// older page is asked for with; the last page has none.
export function ingressVIPMovesQuery(envId: string) {
  return infiniteQueryOptions({
    queryKey: [...ingressKey(envId), "vip", "moves"] as const,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }): Promise<IngressVIPMoves> => {
      const res = await api.GET("/api/env/{envId}/ingress/vip/moves", {
        params: {
          path: { envId },
          query: { limit: VIP_MOVES_PAGE_SIZE, ...(pageParam ? { before: pageParam } : {}) },
        },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    getNextPageParam: (last) => last.next || undefined,
    retry: false,
    refetchInterval: false,
  });
}

export interface RevokeIngressNodeInput {
  nodeId: string;
  // Why the node is revoked; left out of the request when empty.
  reason?: string;
  // Let the host enrol again with the join token it still holds.
  allowReenrol?: boolean;
}

// useRevokeIngressNode revokes one ingress node (D-13). It needs environments.revoke, which the server checks. The
// answer lists the hostnames whose keys the node may still hold. A refusal throws IngressActionError with the
// server's own words.
export function useRevokeIngressNode(envId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: RevokeIngressNodeInput): Promise<RevokeIngressNodeResult> => {
      const reason = input.reason?.trim();
      const { data, error, response } = await api.POST(
        "/api/env/{envId}/ingress/nodes/{nodeId}/revoke",
        {
          params: { path: { envId, nodeId: input.nodeId } },
          body: {
            ...(reason ? { reason } : {}),
            allowReenrol: input.allowReenrol === true,
          },
        },
      );
      if (!data) {
        const problem = readProblem(error, response);
        throw new IngressActionError(problem.status, problem.detail);
      }
      return data;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ingressNodesQuery(envId).queryKey }),
  });
}
