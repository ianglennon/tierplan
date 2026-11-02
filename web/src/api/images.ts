// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type QueryClient, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { DETAIL_REFETCH_MS, detailRetry } from "@/api/container-detail";
import { BULK_CHUNK } from "@/api/containers";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type ImageDetail = components["schemas"]["ImageDetailBody"];
export type ImageName = components["schemas"]["ImageNameBody"];
export type ImageNode = components["schemas"]["ImageNodeBody"];

// ImageLoadError is a detail read the server refused or could not answer. The status and
// the problem type tell an image no reporting node has any more (404 image_not_found) from
// every other failure.
export class ImageLoadError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`GET image failed (${status})`);
    this.name = "ImageLoadError";
    this.status = status;
    this.type = type;
  }
}

// isImageGone says whether the error is the server answering that no node has the image:
// it was removed, or its tag moved to another image.
export function isImageGone(error: unknown): boolean {
  return (
    error instanceof ImageLoadError && error.status === 404 && error.type === "image_not_found"
  );
}

// The key of one image's detail. It extends the images list key, so the events feed and
// every write, which refresh that key, refresh an open detail page too.
export function imageDetailKey(envId: string, imageId: string, node: string | undefined) {
  return [...listKey(envId, "images"), "detail", node ?? "", imageId] as const;
}

// One image, with the nodes that have it. node limits the read to one node by its ID.
export function imageQuery(envId: string, imageId: string, node?: string) {
  return queryOptions({
    queryKey: imageDetailKey(envId, imageId, node),
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/images/{imageId}", {
        params: { path: { envId, imageId }, query: node ? { node } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ImageLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // An image that does not exist will not exist on a second try.
    retry: (count, error) => !isImageGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

export type ImageInspect = components["schemas"]["ImageInspectBody"];

// The raw inspect document of one image, under the key of its detail, so the events feed and
// every write refresh an open Inspect tab with the page. The server has already replaced the
// values of the image's configuration environment unless the caller may reveal secrets.
export function imageInspectQuery(envId: string, imageId: string, node?: string) {
  return queryOptions({
    queryKey: [...imageDetailKey(envId, imageId, node), "inspect"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/images/{imageId}/inspect", {
        params: { path: { envId, imageId }, query: node ? { node } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ImageLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isImageGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

export type ImageHistory = components["schemas"]["ImageHistoryBody"];
export type ImageLayer = components["schemas"]["ImageLayerBody"];

// isAgentTooOld says whether the error is the node's agent refusing the history route by its
// own policy, which is what an agent older than the server does for a route it does not know.
export function isAgentTooOld(error: unknown): boolean {
  return error instanceof ImageLoadError && error.type === "agent_policy_denied";
}

// The layers of one image, under the key of its detail.
export function imageHistoryQuery(envId: string, imageId: string, node?: string) {
  return queryOptions({
    queryKey: [...imageDetailKey(envId, imageId, node), "history"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/images/{imageId}/history", {
        params: { path: { envId, imageId }, query: node ? { node } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ImageLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    // Neither an image that is gone nor an agent that is too old gets better on a second try.
    retry: (count, error) =>
      !isImageGone(error) && !isAgentTooOld(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

export type ImageUsedBy = components["schemas"]["ImageUsedByBody"];
export type ImageUsedByItem = components["schemas"]["ImageUsedByItem"];

// What uses one image: its containers on the nodes that have it and the swarm services whose
// spec names it, under the key of its detail, so the events feed and every write refresh an
// open Used by tab with the page.
export function imageUsedByQuery(envId: string, imageId: string, node?: string) {
  return queryOptions({
    queryKey: [...imageDetailKey(envId, imageId, node), "used-by"] as const,
    queryFn: async () => {
      const res = await api.GET("/api/env/{envId}/images/{imageId}/used-by", {
        params: { path: { envId, imageId }, query: node ? { node } : undefined },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new ImageLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: (count, error) => !isImageGone(error) && count < detailRetry.attempts,
    refetchInterval: DETAIL_REFETCH_MS,
  });
}

export type ImageRemoveTarget = components["schemas"]["ImageRemoveTarget"];
export type ImageRemoveResult = components["schemas"]["ImageRemoveResult"];
export type ImageRemoveCode = NonNullable<ImageRemoveResult["code"]>;

// The results of a remove, one per target in the order asked. bulkId is the ID the server
// grouped the audit rows of the operation under, when it made one: a single target with none
// sent has no bulk.
export type ImageRemoveResults = ImageRemoveResult[] & { bulkId?: string };

// ImageRemoveError is a remove or an untag the server refused as a whole (permission lost, a
// server fault) or that got no answer. An image that failed is not this; it is a result. The
// status is 0 when the server could not be reached.
export class ImageRemoveError extends Error {
  readonly status: number;
  // The problem type the server named (forbidden, agent_policy_denied, image_not_found ...).
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST images remove failed (${status})`);
    this.name = "ImageRemoveError";
    this.status = status;
    this.type = type;
  }
}

// How long after an answer the lists are read once more, in case the Docker event that
// confirms the change is late.
const SETTLE_RECHECK_MS = 2000;

// refreshImages reads the images list, and with it every open image page, and the summary
// again now and once more a moment later, whatever the answer was.
function refreshImages(queryClient: QueryClient, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: listKey(envId, "images") });
    void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
  };
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

async function postRemove(
  envId: string,
  targets: ImageRemoveTarget[],
  bulkId: string | undefined,
): Promise<{ bulkId?: string; results: ImageRemoveResult[] }> {
  try {
    const res = await api.POST("/api/env/{envId}/images/remove", {
      params: { path: { envId } },
      body: { targets, bulkId },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ImageRemoveError(problem.status, problem.type);
    }
    return { bulkId: res.data.bulkId, results: res.data.results ?? [] };
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ImageRemoveError ? e : new ImageRemoveError(0);
  }
}

// removeImages removes any number of image copies. The server takes 25 at a time, so a longer
// list goes out in chunks one after the other, every chunk after the first carrying the bulk ID
// the first one was answered with, so all the audit rows of one operation share it. The results
// are merged in the order asked. If the first chunk is refused as a whole, that is thrown; if a
// later one is, its targets are answered as failed and the earlier chunks stand. It never
// retries: a remove whose answer was lost may have happened (P2 D-42).
export async function removeImages(
  envId: string,
  targets: ImageRemoveTarget[],
): Promise<ImageRemoveResults> {
  const results: ImageRemoveResults = [];
  let bulkId: string | undefined;
  for (let at = 0; at < targets.length; at += BULK_CHUNK) {
    const chunk = targets.slice(at, at + BULK_CHUNK);
    try {
      const answer = await postRemove(envId, chunk, bulkId);
      bulkId ??= answer.bulkId;
      results.push(...answer.results);
    } catch (e) {
      if (at === 0) throw e;
      for (const t of chunk) {
        results.push({ ...t, result: "failed", code: "action_failed" });
      }
    }
  }
  if (bulkId !== undefined) results.bulkId = bulkId;
  return results;
}

// useRemoveImages is the mutation behind the remove dialog. It does not retry, and reads the
// lists again whatever the answer.
export function useRemoveImages(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ImageRemoveResults, ImageRemoveError, ImageRemoveTarget[]>({
    mutationFn: (targets) => removeImages(envId, targets),
    retry: false,
    onSettled: () => refreshImages(queryClient, envId),
  });
}

export type ImageUntagResult = components["schemas"]["ImageUntagResult"];

// The results of an untag, one per node, with the bulk ID the server grouped the audit rows
// under when more than one node was asked.
export type ImageUntagResults = ImageUntagResult[] & { bulkId?: string };

// useUntagImage is the mutation behind the untag dialog: one name off an image on the nodes
// chosen. It does not retry, and reads the lists again whatever the answer.
export function useUntagImage(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<
    ImageUntagResults,
    ImageRemoveError,
    { imageId: string; ref: string; nodeIds: string[] }
  >({
    mutationFn: async ({ imageId, ref, nodeIds }) => {
      try {
        const res = await api.POST("/api/env/{envId}/images/untag", {
          params: { path: { envId } },
          body: { imageId, ref, nodeIds },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new ImageRemoveError(problem.status, problem.type);
        }
        const results: ImageUntagResults = [...(res.data.results ?? [])];
        if (res.data.bulkId !== undefined) results.bulkId = res.data.bulkId;
        return results;
      } catch (e) {
        throw e instanceof ImageRemoveError ? e : new ImageRemoveError(0);
      }
    },
    retry: false,
    onSettled: () => refreshImages(queryClient, envId),
  });
}
