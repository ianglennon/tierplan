// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type OwnedItem = components["schemas"]["OwnedBody"];
export type OwnedState = OwnedItem["state"];
export type OwnerCandidate = components["schemas"]["UserRefBody"];

// The ["owned", "user", id] query: the automations a user owns. It is under
// neither ["users"] nor ["me"], so a profile save does not reload it; a transfer
// invalidates it by key.
export const userOwnedQuery = (userId: string) =>
  queryOptions({
    queryKey: ["owned", "user", userId],
    queryFn: async () => {
      const { data } = await api.GET("/api/users/{userId}/owned", {
        params: { path: { userId } },
      });
      if (!data) throw new Error("GET /api/users/{userId}/owned failed");
      return data;
    },
  });

// The ["owned", "eligible", kind, id] query: who could take an automation over
// right now. Never cached past its use: a grant can be removed at any moment, and
// the server checks again on transfer either way.
export const eligibleOwnersQuery = (kind: string, resourceId: string) =>
  queryOptions({
    queryKey: ["owned", "eligible", kind, resourceId],
    queryFn: async () => {
      const { data } = await api.GET("/api/owned/{kind}/{resourceId}/eligible-owners", {
        params: { path: { kind, resourceId } },
      });
      if (!data) throw new Error("GET /api/owned/{kind}/{resourceId}/eligible-owners failed");
      return data;
    },
    gcTime: 0,
    staleTime: 0,
  });

export type TransferResult =
  | { kind: "ok"; item: OwnedItem }
  // The new owner could not run it: 422 owner_not_eligible. Nothing changed.
  | { kind: "not_eligible" }
  // The automation is gone: 404 automation_not_found.
  | { kind: "not_found" }
  | { kind: "error" };

// transferOwned hands an automation to another user. The server checks again that
// they could run it, so a grant removed since the list was read is refused.
export async function transferOwned(
  kind: string,
  resourceId: string,
  ownerId: string,
): Promise<TransferResult> {
  try {
    const { data, error, response } = await api.POST("/api/owned/{kind}/{resourceId}/transfer", {
      params: { path: { kind, resourceId } },
      body: { ownerId },
    });
    if (data) return { kind: "ok", item: data };
    const problem = readProblem(error, response);
    if (problem.status === 422 && problem.type === "owner_not_eligible") {
      return { kind: "not_eligible" };
    }
    if (problem.status === 404) return { kind: "not_found" };
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
