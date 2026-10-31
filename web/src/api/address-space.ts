// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";

export type AddressSpace = components["schemas"]["AddressSpaceBody"];
export type AddressPool = components["schemas"]["AddressPoolBody"];
export type OverlayUsage = components["schemas"]["OverlayUsageBody"];

// The key of the environment's address space, so the events feed can make it stale.
export function addressSpaceKey(envId: string) {
  return ["env", envId, "address-space"] as const;
}

// The environment's pools, its ingress and its overlays against their capacity. Docker is the
// source of truth, so nothing here is kept for another visit: the page reads again on a network,
// container or service event and every 30 s while the tab is visible (TanStack does not poll a
// hidden tab). The rows of another environment are never carried over.
export function addressSpaceQuery(envId: string) {
  return queryOptions({
    queryKey: addressSpaceKey(envId),
    queryFn: async (): Promise<AddressSpace> => {
      const { data } = await api.GET("/api/env/{envId}/address-space", {
        params: { path: { envId } },
      });
      if (!data) throw new Error("GET address-space failed");
      return data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}
