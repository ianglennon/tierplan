// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { BULK_CHUNK } from "@/api/containers";
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

export type VolumeDeleteResult = components["schemas"]["VolumeDeleteResult"];
export type VolumeDeleteTarget = components["schemas"]["VolumeDeleteTarget"];
export type VolumeDeleteCode = NonNullable<VolumeDeleteResult["code"]>;

// The results of a delete, one per target in the order asked. bulkId is the ID the
// server grouped the audit rows of the operation under, when it made one: a single
// target with none sent has no bulk.
export type VolumeDeleteResults = VolumeDeleteResult[] & { bulkId?: string };

// VolumeDeleteError is a delete the server refused as a whole (permission lost, the
// confirmation not accepted, a server fault) or that got no answer. A volume that
// failed is not this; it is a result.
export class VolumeDeleteError extends Error {
  readonly status: number;
  // The problem type the server named (confirmation_required, forbidden,
  // agent_policy_denied ...), when it answered with one.
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST volumes/delete failed (${status})`);
    this.name = "VolumeDeleteError";
    this.status = status;
    this.type = type;
  }
}

async function postDelete(
  envId: string,
  confirm: string,
  targets: VolumeDeleteTarget[],
  bulkId: string | undefined,
): Promise<{ bulkId?: string; results: VolumeDeleteResult[] }> {
  try {
    const res = await api.POST("/api/env/{envId}/volumes/delete", {
      params: { path: { envId } },
      body: { confirm, targets, bulkId },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new VolumeDeleteError(problem.status, problem.type);
    }
    return { bulkId: res.data.bulkId, results: res.data.results ?? [] };
  } catch (e) {
    // A network failure has no status.
    throw e instanceof VolumeDeleteError ? e : new VolumeDeleteError(0);
  }
}

// deleteVolumes deletes any number of volumes. The server takes 25 at a time, so a
// longer list goes out in chunks one after the other, each carrying the same typed
// confirmation (the phrase names the whole selection), and every chunk after the first
// carries the bulk ID the first one was answered with, so all the audit rows of one
// operation share it. The results are merged in the order asked. If the first chunk is
// refused as a whole, that is thrown; if a later one is, its targets are answered as
// failed and the earlier chunks stand. It never retries: a delete whose answer was lost
// may have happened (P2 D-42).
export async function deleteVolumes(
  envId: string,
  confirm: string,
  targets: VolumeDeleteTarget[],
): Promise<VolumeDeleteResults> {
  const results: VolumeDeleteResults = [];
  let bulkId: string | undefined;
  for (let at = 0; at < targets.length; at += BULK_CHUNK) {
    const chunk = targets.slice(at, at + BULK_CHUNK);
    try {
      const answer = await postDelete(envId, confirm, chunk, bulkId);
      bulkId ??= answer.bulkId;
      results.push(...answer.results);
    } catch (e) {
      if (at === 0) throw e;
      for (const t of chunk) {
        results.push({ nodeId: t.nodeId, name: t.name, result: "failed", code: "action_failed" });
      }
    }
  }
  if (bulkId !== undefined) results.bulkId = bulkId;
  return results;
}

export interface DeleteVolumesVariables {
  confirm: string;
  targets: VolumeDeleteTarget[];
}

// useDeleteVolumes is the mutation behind the delete dialog. It does not retry, and
// reads the list again whatever the answer.
export function useDeleteVolumes(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<VolumeDeleteResults, VolumeDeleteError, DeleteVolumesVariables>({
    mutationFn: ({ confirm, targets }) => deleteVolumes(envId, confirm, targets),
    retry: false,
    onSettled: () => refreshLists(queryClient, envId),
  });
}
