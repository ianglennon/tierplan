// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type NetworkCreateRequest = components["schemas"]["NetworkCreateRequest"];
export type NetworkCreated = components["schemas"]["NetworkCreateBody"];

// How long after an answer the list is read once more, in case the Docker event
// that confirms the change is late.
const SETTLE_RECHECK_MS = 2000;

// NetworkRequestError is a create the server refused or that got no answer. The
// status is 0 when the server could not be reached. The type is the problem type
// the server named (name_taken, host_read_only, node_required, subnet_invalid ...).
export class NetworkRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST networks failed (${status})`);
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

// useCreateNetwork is the mutation behind the create dialog. It does not retry,
// and reads the list again whatever the answer.
export function useCreateNetwork(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<NetworkCreated, NetworkRequestError, NetworkCreateRequest>({
    mutationFn: (body) => postNetwork(envId, body),
    retry: false,
    onSettled: () => {
      const refresh = () => {
        void queryClient.invalidateQueries({ queryKey: listKey(envId, "networks") });
        void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
      };
      refresh();
      setTimeout(refresh, SETTLE_RECHECK_MS);
    },
  });
}
