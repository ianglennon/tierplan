// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type SessionInfo = components["schemas"]["SessionBody"];

// The ["sessions", "mine"] query: where the signed-in user is signed in. It is
// not under ["me"], so changing a password, which refetches ["me"], does not
// reload it.
export const mySessionsQuery = queryOptions({
  queryKey: ["sessions", "mine"],
  queryFn: async () => {
    const { data } = await api.GET("/api/me/sessions");
    if (!data) throw new Error("GET /api/me/sessions failed");
    return data;
  },
});

// The ["sessions", "user", id] query: where another user is signed in, for an
// admin on that user's page.
export const userSessionsQuery = (userId: string) =>
  queryOptions({
    queryKey: ["sessions", "user", userId],
    queryFn: async () => {
      const { data } = await api.GET("/api/users/{userId}/sessions", {
        params: { path: { userId } },
      });
      if (!data) throw new Error("GET /api/users/{userId}/sessions failed");
      return data;
    },
  });

export type RevokeResult =
  | { kind: "ok" }
  // The server refused the session making the request. The row offers no
  // Revoke for it, so this means the screen is out of date.
  | { kind: "current" }
  | { kind: "error" };

// revokeMySession signs one of the caller's other sessions out; its next
// request is refused. A session that has already ended counts as done: it is
// gone, which is what was asked for.
export async function revokeMySession(publicId: string): Promise<RevokeResult> {
  try {
    const { error, response } = await api.DELETE("/api/me/sessions/{publicId}", {
      params: { path: { publicId } },
    });
    if (response.ok) return { kind: "ok" };
    const problem = readProblem(error, response);
    if (problem.status === 404) return { kind: "ok" };
    if (problem.status === 409 && problem.type === "current_session") return { kind: "current" };
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

export type EndedResult = { kind: "ok"; ended: number } | { kind: "error" };

// revokeMyOtherSessions is "Sign out everywhere else".
export async function revokeMyOtherSessions(): Promise<EndedResult> {
  try {
    const { data } = await api.POST("/api/me/sessions/revoke-others");
    return data ? { kind: "ok", ended: data.ended } : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

// revokeUserSessions is an admin's "End all sessions" on a user's page.
export async function revokeUserSessions(userId: string): Promise<EndedResult> {
  try {
    const { data } = await api.POST("/api/users/{userId}/sessions/revoke-all", {
      params: { path: { userId } },
    });
    return data ? { kind: "ok", ended: data.ended } : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
