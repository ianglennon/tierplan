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
