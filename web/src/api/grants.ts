// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type Grant = components["schemas"]["GrantBody"];
export type EffectiveEnv = components["schemas"]["EffectiveEnvBody"];
export type GrantSource = components["schemas"]["SourceBody"];
export type UserAccess = components["schemas"]["UserAccessBody"];
export type GrantInput = components["schemas"]["GrantInputBody"];

export type Role = Grant["role"];
export type SubjectType = Grant["subjectType"];

// UserNotFoundForAccessError is the 404 of GET /api/users/{id}/access: the user
// is gone. The user page shows its own "Page not found" for that.
export class AccessNotFoundError extends Error {
  constructor() {
    super("subject not found");
    this.name = "AccessNotFoundError";
  }
}

// The ["users", id, "access"] query: what a user may do on each environment and
// why, plus the user's own grants. It sits under the user's own key, so reading
// the user again reads this too.
export const userAccessQuery = (userId: string) =>
  queryOptions({
    queryKey: ["users", userId, "access"],
    queryFn: async (): Promise<UserAccess> => {
      const { data, response } = await api.GET("/api/users/{userId}/access", {
        params: { path: { userId } },
      });
      if (!data) {
        if (response.status === 404) throw new AccessNotFoundError();
        throw new Error("GET /api/users/{userId}/access failed");
      }
      return data;
    },
    retry: (count, error) => !(error instanceof AccessNotFoundError) && count < 1,
  });

// The ["teams", id, "grants"] query: the grants made to a team.
export const teamGrantsQuery = (teamId: string) =>
  queryOptions({
    queryKey: ["teams", teamId, "grants"],
    queryFn: async (): Promise<Grant[]> => {
      const { data, response } = await api.GET("/api/teams/{teamId}/grants", {
        params: { path: { teamId } },
      });
      if (!data) {
        if (response.status === 404) throw new AccessNotFoundError();
        throw new Error("GET /api/teams/{teamId}/grants failed");
      }
      return data;
    },
    retry: (count, error) => !(error instanceof AccessNotFoundError) && count < 1,
  });

export type PutGrantResult =
  | { kind: "ok"; grant: Grant; created: boolean }
  // 422 viewer_cannot_have_extras (D-17): the dialog clears extras on a Viewer, so
  // this only happens when the screen is out of step with the server.
  | { kind: "viewer_extras" }
  // 403: the caller may not give that access.
  | { kind: "forbidden" }
  // 409 stale_change: the grant changed since the dialog opened.
  | { kind: "stale_change" }
  // 404: the person, the team or the environment is gone.
  | { kind: "not_found" }
  | { kind: "error" };

// putGrant saves a grant for a user or a team: a new one is created, an existing
// one for the same scope is edited. With `expected`, a grant that has changed
// since is stale_change and nothing is saved.
export async function putGrant(
  subjectType: SubjectType,
  subjectId: string,
  input: GrantInput,
): Promise<PutGrantResult> {
  try {
    const result =
      subjectType === "user"
        ? await api.PUT("/api/users/{userId}/grants", {
            params: { path: { userId: subjectId } },
            body: input,
          })
        : await api.PUT("/api/teams/{teamId}/grants", {
            params: { path: { teamId: subjectId } },
            body: input,
          });
    if (result.data) {
      return { kind: "ok", grant: result.data, created: result.response.status === 201 };
    }
    const problem = readProblem(result.error, result.response);
    if (problem.status === 422 && problem.type === "viewer_cannot_have_extras") {
      return { kind: "viewer_extras" };
    }
    if (problem.status === 403) return { kind: "forbidden" };
    if (problem.status === 409 && problem.type === "stale_change") return { kind: "stale_change" };
    if (problem.status === 404) return { kind: "not_found" };
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
