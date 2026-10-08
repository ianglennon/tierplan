// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { PutGrantResult } from "@/api/grants";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type EnvAccessList = components["schemas"]["EnvAccessListBody"];
export type EnvAccessTeam = components["schemas"]["EnvAccessTeamBody"];
export type EnvAccessPerson = components["schemas"]["EnvAccessPersonBody"];
export type EnvAccessMember = components["schemas"]["EnvAccessMemberBody"];
export type EnvAccessCandidates = components["schemas"]["EnvAccessCandidatesBody"];
export type EnvGrantInput = components["schemas"]["EnvGrantInputBody"];

// The ["env", id, "access"] query: everyone who can use the environment and why,
// worked out by the server's own evaluator. It sits under the environment's key.
export const envAccessQuery = (envId: string) =>
  queryOptions({
    queryKey: ["env", envId, "access"],
    queryFn: async (): Promise<EnvAccessList> => {
      const { data } = await api.GET("/api/env/{envId}/access", {
        params: { path: { envId } },
      });
      if (!data) throw new Error("GET /api/env/{envId}/access failed");
      return data;
    },
    retry: 1,
  });

// The ["env", id, "access", "candidates"] query: the users and teams the Who list
// offers. Usernames, display names and team names, never an email.
export const envAccessCandidatesQuery = (envId: string) =>
  queryOptions({
    queryKey: ["env", envId, "access", "candidates"],
    queryFn: async (): Promise<EnvAccessCandidates> => {
      const { data } = await api.GET("/api/env/{envId}/access/candidates", {
        params: { path: { envId } },
      });
      if (!data) throw new Error("GET /api/env/{envId}/access/candidates failed");
      return data;
    },
    retry: 1,
  });

// putEnvAccess gives a user or a team a role on this environment, through the
// route an env Admin may use (D-12). The route is the scope: the body names no
// environment, so a request cannot aim anywhere else.
export async function putEnvAccess(envId: string, input: EnvGrantInput): Promise<PutGrantResult> {
  try {
    const result = await api.PUT("/api/env/{envId}/access", {
      params: { path: { envId } },
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
