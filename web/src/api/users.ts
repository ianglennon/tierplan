// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type UserSummary = components["schemas"]["UserSummaryBody"];
export type OneTimeLink = components["schemas"]["LinkBody"];
export type UserStatus = UserSummary["status"];

// The ["users"] query: every user who is not deleted, sorted by username. The
// whole list is fetched, filtered in the browser and never paged (UI-SPEC).
export const usersQuery = queryOptions({
  queryKey: ["users"],
  queryFn: async () => {
    const { data } = await api.GET("/api/users");
    if (!data) throw new Error("GET /api/users failed");
    return data;
  },
});

export interface NewUser {
  username: string;
  displayName: string;
  email: string;
}

export type CreateUserResult =
  | { kind: "ok"; user: UserSummary; link: OneTimeLink }
  | { kind: "username_taken" }
  | { kind: "username_reserved" }
  | { kind: "fields"; fields: ProblemField[] }
  | { kind: "error" };

// createUser adds a user and returns the one-time invite link. The link is
// handed to the caller and kept nowhere else: not in the query cache, not in
// storage (UI-SPEC "One-time secrets"). A network failure is "error".
export async function createUser(input: NewUser): Promise<CreateUserResult> {
  try {
    const { data, error, response } = await api.POST("/api/users", {
      body: {
        username: input.username,
        displayName: input.displayName || undefined,
        email: input.email || undefined,
      },
    });
    if (data) return { kind: "ok", user: data.user, link: data.link };
    const problem = readProblem(error, response);
    if (problem.status === 409 && problem.type === "username_taken") {
      return { kind: "username_taken" };
    }
    if (problem.status === 409 && problem.type === "username_reserved") {
      return { kind: "username_reserved" };
    }
    if (problem.status === 422 && problem.fields.length > 0) {
      return { kind: "fields", fields: problem.fields };
    }
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
