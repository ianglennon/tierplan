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

// The query keys of the four lists, so the events feed can invalidate them.
export function listKey(envId: string, resource: "containers" | "images" | "volumes" | "networks") {
  return ["env", envId, resource] as const;
}

export function containersQuery(envId: string, _node?: string) {
  return queryOptions({
    queryKey: listKey(envId, "containers"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/containers", {
        params: { path: { envId } },
      });
      if (!data) throw new Error("GET containers failed");
      return data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}

export function imagesQuery(envId: string, _node?: string) {
  return queryOptions({
    queryKey: listKey(envId, "images"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/images", { params: { path: { envId } } });
      if (!data) throw new Error("GET images failed");
      return data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}

export function volumesQuery(envId: string, _node?: string) {
  return queryOptions({
    queryKey: listKey(envId, "volumes"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/volumes", { params: { path: { envId } } });
      if (!data) throw new Error("GET volumes failed");
      return data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}

export function networksQuery(envId: string, _node?: string) {
  return queryOptions({
    queryKey: listKey(envId, "networks"),
    queryFn: async () => {
      const { data } = await api.GET("/api/env/{envId}/networks", { params: { path: { envId } } });
      if (!data) throw new Error("GET networks failed");
      return data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}
