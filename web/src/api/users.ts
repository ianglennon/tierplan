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
  | { kind: "error" }
  // The request never got an answer: the network or the server is down.
  | { kind: "unreachable" };

// createUser adds a user and returns the one-time invite link. The link is
// handed to the caller and kept nowhere else: not in the query cache, not in
// storage (UI-SPEC "One-time secrets").
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
    return { kind: "unreachable" };
  }
}

export type RegenerateResult =
  | { kind: "ok"; link: OneTimeLink }
  | { kind: "not_invited" }
  | { kind: "error" };

// regenerateInvite replaces a user's invite link; the old one stops working at
// once. Like createUser it hands the link to the caller and keeps it nowhere
// else. A user who is gone (404) is an error to the caller: the list is read
// again either way.
export async function regenerateInvite(userId: string): Promise<RegenerateResult> {
  try {
    const { data, error, response } = await api.POST("/api/users/{userId}/invite", {
      params: { path: { userId } },
    });
    if (data) return { kind: "ok", link: data };
    const problem = readProblem(error, response);
    if (problem.status === 409 && problem.type === "not_invited") return { kind: "not_invited" };
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

export type UserDetail = components["schemas"]["UserDetailBody"];

// UserNotFoundError is the 404 of GET /api/users/{id}: an unknown or deleted
// user. The user page shows the P1 "Page not found" page for it.
export class UserNotFoundError extends Error {
  constructor() {
    super("user not found");
    this.name = "UserNotFoundError";
  }
}

// The ["users", id] query: one user for the user page. It sits under the
// ["users"] key, so refreshing the list refreshes the page too. A 404 is not
// retried.
export const userQuery = (userId: string) =>
  queryOptions({
    queryKey: ["users", userId],
    queryFn: async (): Promise<UserDetail> => {
      const { data, response } = await api.GET("/api/users/{userId}", {
        params: { path: { userId } },
      });
      if (!data) {
        if (response.status === 404) throw new UserNotFoundError();
        throw new Error("GET /api/users/{userId} failed");
      }
      return data;
    },
    retry: (count, error) => !(error instanceof UserNotFoundError) && count < 1,
  });

// What an account change answers. The server's own refusals get their own kind
// so a screen can say why in words: the last active instance admin cannot be
// taken away (409 last_instance_admin) and a save based on an old version of the
// user is stale (409 stale_change).
export type LifecycleResult =
  | { kind: "ok"; user: UserDetail }
  | { kind: "last_instance_admin" }
  | { kind: "stale_change" }
  | { kind: "not_found" }
  | { kind: "fields"; fields: ProblemField[] }
  | { kind: "error" };

function lifecycleFailure(error: unknown, response: { status: number }): LifecycleResult {
  const problem = readProblem(error, response);
  if (problem.status === 409 && problem.type === "last_instance_admin") {
    return { kind: "last_instance_admin" };
  }
  if (problem.status === 409 && problem.type === "stale_change") return { kind: "stale_change" };
  if (problem.status === 404) return { kind: "not_found" };
  if (problem.status === 422 && problem.fields.length > 0) {
    return { kind: "fields", fields: problem.fields };
  }
  return { kind: "error" };
}

// disableUser ends every session of the user and refuses their sign-in; their
// access and teams are kept.
export async function disableUser(userId: string): Promise<LifecycleResult> {
  try {
    const { data, error, response } = await api.POST("/api/users/{userId}/disable", {
      params: { path: { userId } },
    });
    return data ? { kind: "ok", user: data } : lifecycleFailure(error, response);
  } catch {
    return { kind: "error" };
  }
}

// enableUser lets a disabled user sign in again.
export async function enableUser(userId: string): Promise<LifecycleResult> {
  try {
    const { data, error, response } = await api.POST("/api/users/{userId}/enable", {
      params: { path: { userId } },
    });
    return data ? { kind: "ok", user: data } : lifecycleFailure(error, response);
  } catch {
    return { kind: "error" };
  }
}
