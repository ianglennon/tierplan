// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

export type ContainerRow = components["schemas"]["ContainerItem"];
export type ImageRow = components["schemas"]["ImageItem"];
export type VolumeRow = components["schemas"]["VolumeItem"];
export type NetworkRow = components["schemas"]["NetworkItem"];

// The lists refetch on this interval as the polling fallback while the events
// stream does not drive them (D-57). The offline and unsupported answers are
// turned into full-page states by the environment layout, which reads the
// summary; a list that fails here shows its own inline error with Retry.
export const LIST_REFETCH_MS = 30_000;

// What a swarm-wide network (and nothing else) shows in the Node column, and
// one of the names in the lists' `nodes`. The server says "Cluster" (envs.NodeCluster).
export const CLUSTER_NODE = "Cluster";

type Resource = "containers" | "images" | "volumes" | "networks";

// The query keys of the four lists, so the events feed can invalidate them.
export function listKey(envId: string, resource: Resource) {
  return ["env", envId, resource] as const;
}

// The key of a list read for one node. The unfiltered key is listKey itself and
// a filtered one extends it, so invalidating listKey refreshes every filter.
function nodeKey(envId: string, resource: Resource, node: string | undefined) {
  return node ? ([...listKey(envId, resource), { node }] as const) : listKey(envId, resource);
}

// Changing the node filter keeps the rows on screen until the new ones arrive,
// so the table does not jump to a skeleton. It never carries rows over from
// another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

export function containersQuery(envId: string, node?: string) {
  return queryOptions({
    queryKey: nodeKey(envId, "containers", node),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/containers", {
        params: { path: { envId }, query: node ? { node } : undefined },
      });
      if (!data) throw new Error("GET containers failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

export function imagesQuery(envId: string, node?: string) {
  return queryOptions({
    queryKey: nodeKey(envId, "images", node),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/images", {
        params: { path: { envId }, query: node ? { node } : undefined },
      });
      if (!data) throw new Error("GET images failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

export function volumesQuery(envId: string, node?: string) {
  return queryOptions({
    queryKey: nodeKey(envId, "volumes", node),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/volumes", {
        params: { path: { envId }, query: node ? { node } : undefined },
      });
      if (!data) throw new Error("GET volumes failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

export function networksQuery(envId: string, node?: string) {
  return queryOptions({
    queryKey: nodeKey(envId, "networks", node),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/networks", {
        params: { path: { envId }, query: node ? { node } : undefined },
      });
      if (!data) throw new Error("GET networks failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}
