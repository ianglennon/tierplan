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

type Resource =
  | "containers"
  | "images"
  | "volumes"
  | "networks"
  | "services"
  | "nodes"
  | "secrets"
  | "configs";

// The query keys of the lists, so the events feed can invalidate them.
export function listKey(envId: string, resource: Resource) {
  return ["env", envId, resource] as const;
}

// Changing the node filter keeps the rows on screen until the new ones arrive,
// so the table does not jump to a skeleton. It never carries rows over from
// another environment.
function keepForSameEnv(envId: string) {
  return <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === envId ? previous : undefined;
}

// What narrows the containers list on the server, before its 500-row cap: the
// node, the search text and the comma list of statuses (UI-SPEC "Shared list
// behaviour"). Each is left out when it is not set.
export interface ContainerFilters {
  node?: string;
  q?: string;
  status?: string;
}

// The part of a key that names the filters, or none when nothing is set, so the
// unfiltered list keeps the bare listKey and every filtered read extends it.
function filterPart(filters: ContainerFilters & { usage?: string }) {
  const set: ContainerFilters & { usage?: string } = {};
  if (filters.node) set.node = filters.node;
  if (filters.q) set.q = filters.q;
  if (filters.status) set.status = filters.status;
  if (filters.usage) set.usage = filters.usage;
  return Object.keys(set).length > 0 ? set : undefined;
}

export function containersQuery(envId: string, filters: ContainerFilters = {}) {
  const set = filterPart(filters);
  return queryOptions({
    queryKey: set
      ? ([...listKey(envId, "containers"), set] as const)
      : listKey(envId, "containers"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/containers", {
        params: { path: { envId }, query: set },
      });
      if (!data) throw new Error("GET containers failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

// What narrows the images list on the server, before its 500-row cap: the node, the search
// text, whether only untagged images are wanted and whether the rows are merged over nodes
// or one per node copy.
export interface ImageFilters {
  node?: string;
  q?: string;
  dangling?: boolean;
  view?: "merged" | "node";
}

export function imagesQuery(envId: string, filters: ImageFilters = {}) {
  const set: { node?: string; q?: string; dangling?: boolean; view?: "node" } = {};
  if (filters.node) set.node = filters.node;
  if (filters.q) set.q = filters.q;
  if (filters.dangling) set.dangling = true;
  // Merged is the server's default and keeps the bare key.
  if (filters.view === "node") set.view = "node";
  const filtered = Object.keys(set).length > 0;
  return queryOptions({
    queryKey: filtered ? ([...listKey(envId, "images"), set] as const) : listKey(envId, "images"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/images", {
        params: { path: { envId }, query: filtered ? set : undefined },
      });
      if (!data) throw new Error("GET images failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

// What narrows the volumes list on the server, before its 500-row cap: the node, the
// search text and whether the volume is in use.
export interface VolumeFilters {
  node?: string;
  q?: string;
  usage?: "used" | "unused";
}

export function volumesQuery(envId: string, filters: VolumeFilters = {}) {
  const set = filterPart(filters) as VolumeFilters | undefined;
  return queryOptions({
    queryKey: set ? ([...listKey(envId, "volumes"), set] as const) : listKey(envId, "volumes"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/volumes", {
        params: { path: { envId }, query: set },
      });
      if (!data) throw new Error("GET volumes failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}

// What narrows the networks list on the server, before its 500-row cap: the node
// and the search text.
export interface NetworkFilters {
  node?: string;
  q?: string;
}

export function networksQuery(envId: string, filters: NetworkFilters = {}) {
  const set = filterPart(filters);
  return queryOptions({
    queryKey: set ? ([...listKey(envId, "networks"), set] as const) : listKey(envId, "networks"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/networks", {
        params: { path: { envId }, query: set },
      });
      if (!data) throw new Error("GET networks failed");
      return data;
    },
    placeholderData: keepForSameEnv(envId),
    refetchInterval: LIST_REFETCH_MS,
  });
}
