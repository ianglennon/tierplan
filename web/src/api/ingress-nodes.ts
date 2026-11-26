// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

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
