// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { DETAIL_REFETCH_MS, detailRetry } from "@/api/container-detail";
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
