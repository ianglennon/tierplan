// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { GrantChange, PutGrantResult, RemoveGrantResult } from "@/api/grants";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import type { ImpactLoad } from "@/api/teams";

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

// previewEnvAccessImpact asks who would lose which environment access if one of
// the environment's grants were removed, or saved with a lower role, through the
// route an env Admin may use. Nothing is changed by asking. Any failure is one
// "error", so the confirm that waits on the preview fails closed.
export async function previewEnvAccessImpact(
  envId: string,
  grantId: string,
  change: GrantChange,
): Promise<ImpactLoad> {
  const body =
    change.op === "delete"
      ? { op: "delete" as const }
      : { op: "update" as const, role: change.role, extras: [...change.extras] };
  try {
    const result = await api.POST("/api/env/{envId}/access/{grantId}/impact", {
      params: { path: { envId, grantId } },
      body,
    });
    return result.data ? { kind: "ok", rows: result.data.impact ?? [] } : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

// removeEnvAccess takes one of the environment's grants away. A grant that is
// already gone is not_found; anything else that fails is an error.
export async function removeEnvAccess(envId: string, grantId: string): Promise<RemoveGrantResult> {
  try {
    const result = await api.DELETE("/api/env/{envId}/access/{grantId}", {
      params: { path: { envId, grantId } },
    });
    if (result.response.ok) return { kind: "ok" };
    return readProblem(result.error, result.response).status === 404
      ? { kind: "not_found" }
      : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
