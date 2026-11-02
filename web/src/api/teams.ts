// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type Team = components["schemas"]["TeamBody"];
export type TeamDetail = components["schemas"]["TeamDetailBody"];
export type TeamMember = components["schemas"]["TeamMemberBody"];

// The ["teams"] query: every team, sorted by name. The whole list is fetched,
// never paged (UI-SPEC).
export const teamsQuery = queryOptions({
  queryKey: ["teams"],
  queryFn: async () => {
    const { data } = await api.GET("/api/teams");
    if (!data) throw new Error("GET /api/teams failed");
    return data;
  },
});

// TeamNotFoundError is the 404 of GET /api/teams/{id}: an unknown team. The team
// page shows the P1 "Page not found" page for it.
export class TeamNotFoundError extends Error {
  constructor() {
    super("team not found");
    this.name = "TeamNotFoundError";
  }
}

// The ["teams", id] query: one team with its members. It sits under the
// ["teams"] key, so refreshing the list refreshes the page too. A 404 is not
// retried.
export const teamQuery = (teamId: string) =>
  queryOptions({
    queryKey: ["teams", teamId],
    queryFn: async (): Promise<TeamDetail> => {
      const { data, response } = await api.GET("/api/teams/{teamId}", {
        params: { path: { teamId } },
      });
      if (!data) {
        if (response.status === 404) throw new TeamNotFoundError();
        throw new Error("GET /api/teams/{teamId} failed");
      }
      return data;
    },
    retry: (count, error) => !(error instanceof TeamNotFoundError) && count < 1,
  });

export type CreateTeamResult =
  | { kind: "ok"; team: Team }
  | { kind: "name_taken" }
  | { kind: "fields"; fields: ProblemField[] }
  | { kind: "error" }
  // The request never got an answer: the network or the server is down.
  | { kind: "unreachable" };

// createTeam adds a team. A name another team holds, ignoring case, is
// name_taken; an empty or too long one comes back as a field error.
export async function createTeam(name: string): Promise<CreateTeamResult> {
  try {
    const { data, error, response } = await api.POST("/api/teams", { body: { name } });
    if (data) return { kind: "ok", team: data };
    const problem = readProblem(error, response);
    if (problem.status === 409 && problem.type === "team_name_taken") return { kind: "name_taken" };
    if (problem.status === 422 && problem.fields.length > 0) {
      return { kind: "fields", fields: problem.fields };
    }
    return { kind: "error" };
  } catch {
    return { kind: "unreachable" };
  }
}

export type AddMembersResult =
  | { kind: "ok"; added: number }
  // The team or one of the users is gone: the screen is out of date.
  | { kind: "not_found" }
  | { kind: "error" };

// addTeamMembers puts users in a team, all or none. Users who were already in it
// are not counted.
export async function addTeamMembers(teamId: string, userIds: string[]): Promise<AddMembersResult> {
  try {
    const { data, error, response } = await api.POST("/api/teams/{teamId}/members", {
      params: { path: { teamId } },
      body: { userIds },
    });
    if (data) return { kind: "ok", added: data.added };
    if (readProblem(error, response).status === 404) return { kind: "not_found" };
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

export type ImpactRow = components["schemas"]["ImpactRowBody"];

// What an impact preview answers. A failure of any kind is one "error": the
// confirm that waits on the preview stays unusable (it fails closed). Grant
// removal (plan 03-19) returns the same shape, so ImpactPreview serves both.
export type ImpactLoad = { kind: "ok"; rows: ImpactRow[] } | { kind: "error" };

export type TeamChange = { op: "delete" } | { op: "remove_member"; userId: string };

// previewTeamImpact asks who would lose which environment access if the team
// were deleted or a member removed. Nothing is changed by asking.
export async function previewTeamImpact(teamId: string, change: TeamChange): Promise<ImpactLoad> {
  try {
    const { data } = await api.POST("/api/teams/{teamId}/impact", {
      params: { path: { teamId } },
      body:
        change.op === "delete" ? { op: "delete" } : { op: "remove_member", userId: change.userId },
    });
    return data ? { kind: "ok", rows: data.impact ?? [] } : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

export type RenameTeamResult =
  | { kind: "ok"; team: Team }
  | { kind: "name_taken" }
  // The team changed since it was read (409 stale_change).
  | { kind: "stale_change" }
  | { kind: "not_found" }
  | { kind: "fields"; fields: ProblemField[] }
  | { kind: "error" };

// renameTeam changes a team's name. It carries the updatedAt it was based on: if
// the team changed since, nothing is saved and the result is stale_change.
export async function renameTeam(
  teamId: string,
  name: string,
  updatedAt: string,
): Promise<RenameTeamResult> {
  try {
    const { data, error, response } = await api.PATCH("/api/teams/{teamId}", {
      params: { path: { teamId } },
      body: { name, updatedAt },
    });
    if (data) return { kind: "ok", team: data };
    const problem = readProblem(error, response);
    if (problem.status === 409 && problem.type === "team_name_taken") return { kind: "name_taken" };
    if (problem.status === 409 && problem.type === "stale_change") return { kind: "stale_change" };
    if (problem.status === 404) return { kind: "not_found" };
    if (problem.status === 422 && problem.fields.length > 0) {
      return { kind: "fields", fields: problem.fields };
    }
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

export type TeamChangeResult = { kind: "ok" } | { kind: "not_found" } | { kind: "error" };

// deleteTeam removes a team with its grants and memberships. It answers the
// impact the delete had; the screen already showed it, so it is not returned.
export async function deleteTeam(teamId: string): Promise<TeamChangeResult> {
  try {
    const { error, response } = await api.DELETE("/api/teams/{teamId}", {
      params: { path: { teamId } },
    });
    if (response.ok) return { kind: "ok" };
    return readProblem(error, response).status === 404 ? { kind: "not_found" } : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

// removeTeamMember takes a user out of a team. A user who is no longer a member
// (404) is not_found: the screen is out of date and reads again.
export async function removeTeamMember(teamId: string, userId: string): Promise<TeamChangeResult> {
  try {
    const { error, response } = await api.DELETE("/api/teams/{teamId}/members/{userId}", {
      params: { path: { teamId, userId } },
    });
    if (response.ok) return { kind: "ok" };
    return readProblem(error, response).status === 404 ? { kind: "not_found" } : { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
