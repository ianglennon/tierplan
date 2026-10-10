// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type VolumeCreateRequest = components["schemas"]["VolumeCreateRequest"];
export type VolumeCreated = components["schemas"]["VolumeCreateBody"];

// How long after an answer the list is read once more, in case the Docker event
// that confirms the change is late.
const SETTLE_RECHECK_MS = 2000;

// VolumeRequestError is a create the server refused or that got no answer. The
// status is 0 when the server could not be reached. The type is the problem type the
// server named (name_taken, host_read_only, node_required, node_unavailable ...).
export class VolumeRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string, method = "POST") {
    super(`${method} volumes failed (${status})`);
    this.name = "VolumeRequestError";
    this.status = status;
    this.type = type;
  }
}

// postVolume creates one volume. It never retries: a create whose answer was lost may
// have happened (P2 D-42).
export async function postVolume(envId: string, body: VolumeCreateRequest): Promise<VolumeCreated> {
  try {
    const res = await api.POST("/api/env/{envId}/volumes", { params: { path: { envId } }, body });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new VolumeRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof VolumeRequestError ? e : new VolumeRequestError(0);
  }
}

// refreshLists reads the volumes list and the summary again now and once more a
// moment later, whatever the answer was.
function refreshLists(queryClient: QueryClient, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: listKey(envId, "volumes") });
    void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
  };
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

// useCreateVolume is the mutation behind the create dialog. It does not retry, and
// reads the list again whatever the answer.
export function useCreateVolume(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<VolumeCreated, VolumeRequestError, VolumeCreateRequest>({
    mutationFn: (body) => postVolume(envId, body),
    retry: false,
    onSettled: () => refreshLists(queryClient, envId),
  });
}
